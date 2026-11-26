// Package session is the control plane's half of one pi session: it places the session
// on a machine, runs `pi --mode rpc` there, relays records in both directions, and holds
// the lease that keeps the machine alive.
//
// The data path is Autumn's decision (2026-11-26): pi runs on the machine, and the
// control plane relays its JSONL over ssh. The phone never reaches the machine, and pi
// never runs on the control plane. Everything this package uses already existed for
// another reason -- placement decides where, `internal/machine/ssh` carries the command,
// `internal/rpc/relay` frames it -- so what is here is the joining: a lease held for a
// stream's life, a process started where placement said, and a machine given back when
// the last record has been relayed.
//
// # The judgement calls this package makes
//
//  1. **The lease is renewed by stream activity, not by existence.** A record in either
//     direction renews it, at most once per half TTL. The alternative -- a goroutine with a
//     ticker -- is a second clock in a design that injects the one it has, and "the agent
//     is thinking" is exactly the activity the old design could not detect. The cost is
//     stated rather than hidden: a session that says nothing for a whole TTL loses its
//     machine to the drain window, and the next record reports the lapse.
//
//  2. **The lease is released with a context that cannot be cancelled.** The stream ends
//     for two reasons: pi finished, or the caller gave up. In the second case the session's
//     own context is already dead, and a release that inherited it would fail exactly when
//     giving the machine back matters most.
//
//  3. **A process that cannot start does not keep its machine.** The lease was taken to run
//     something; releasing it on the failure path is the difference between a machine that
//     drains and one that bills for a session nobody can reach.
//
//  4. **The id is validated once, as an id.** The same string becomes a holder name, a
//     directory under the workspace, and pi's own session id. Uppercase is refused as well
//     as punctuation, because the first two are filesystem paths: two ids differing only in
//     case are one directory on a case-insensitive filesystem, which is two sessions in one
//     checkout.
//
//  5. **A clean end of stream is not a clean end of session.** pi can emit its last record
//     and then exit non-zero, so EOF asks the process why. A relay that reported EOF alone
//     would let the phone believe the turn completed.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
	"github.com/AutumnsGrove/verge/internal/placement"
	"github.com/AutumnsGrove/verge/internal/rpc/relay"
)

const (
	// DefaultTTL is how long a session's lease lasts without activity. It is renewed at
	// half its length, so a session under any activity never notices it.
	DefaultTTL = time.Hour

	// DefaultWorkspace is the image's workspace root, the directory verge-save tars.
	DefaultWorkspace = "/workspace"

	// maxID is the longest session id, chosen to leave room for the workspace path and
	// pi's own session directory rather than because anything counts the characters.
	maxID = 64
)

var (
	// ErrConfig means the manager cannot be built from its configuration.
	ErrConfig = errors.New("session: invalid configuration")

	// ErrPending means the machine is booting. It is not a failure: the caller asks
	// again, and the retry is cheap.
	ErrPending = errors.New("session: the machine is booting")

	// ErrBadID means the id cannot be a session: it is not a holder name, a path element,
	// and pi's session id at once.
	ErrBadID = errors.New("session: the id is not a session id")
)

// directory is where one session's work lives on the machine.
//
// One directory per session under the single workspace root, rather than one machine per
// session: machines are shared by policy (up to four sessions), and two pi processes in
// one checkout would edit each other's files. verge-save archives the whole workspace, so
// every session's directory travels with the save.
//
// It is unexported deliberately: it is only safe after validID, and a caller that could
// build a path from an unvalidated id would be the quoting this package exists to avoid.
func directory(id string) string { return DefaultWorkspace + "/" + id }

// Command is what runs on the machine, and it is the image's contract the way verge-join
// and verge-save are.
//
// Three parts are load-bearing:
//
//   - `mkdir -p` and `cd`: pi works in the directory it is started in, and the workspace
//     is empty until the first session arrives.
//   - `--session-dir` inside the workspace: pi's session file is the thing a resume
//     replays, and the only state that survives a machine is what verge-save archives.
//     Outside the workspace it would be lost with the machine.
//   - `--session-id`: the same id on a later boot finds the file the save carried,
//     instead of creating a second session beside it.
//
// The id is interpolated rather than quoted because it cannot need quoting: validID has
// already refused everything a shell or a filesystem would read as syntax.
func Command(id string) string {
	dir := directory(id)
	return "mkdir -p " + dir + " && cd " + dir +
		" && exec pi --mode rpc --session-dir " + dir + "/.pi/sessions --session-id " + id
}

// Proc is a command running on the machine: the transport's half, as
// internal/machine/ssh's Proc implements it.
type Proc interface {
	// Stdin is where the control plane's records go. Closing it asks the command to shut
	// down, which is pi's own contract.
	Stdin() io.WriteCloser

	// Stdout is where the command's records come from. The reader reports the stream's
	// end; Wait reports the command's.
	Stdout() io.Reader

	// Wait waits for the command and reports why it failed, bounded by ctx: a shutdown
	// must not await a machine that has stopped answering.
	Wait(ctx context.Context) error

	// Kill stops the command and everything it started.
	Kill() error

	// Close releases the pipes. It neither waits nor kills.
	Close() error
}

// Runner starts a command on a machine. *ssh.Client is the implementation, and the seam
// exists because the far side is a computer.
type Runner interface {
	Run(ctx context.Context, id firefly.MachineID, name, command string) (Proc, error)
}

// Config is the manager's dependencies and its one policy value.
type Config struct {
	// Placement decides which machine the session runs on. Required.
	Placement *placement.Placement

	// Firefly is where the lease lives. Required: placement takes it, and this package
	// renews and releases it.
	Firefly *firefly.Firefly

	// Runner starts pi on the machine. Required.
	Runner Runner

	// TTL is the session lease's length. Zero means DefaultTTL.
	TTL time.Duration

	// Now is the clock renewal is measured against. Defaults to time.Now; a test passes
	// its own, which is the only way a TTL is arithmetic rather than a wait.
	Now func() time.Time

	// Log records the lease failures that are not fatal. Defaults to slog.Default().
	Log *slog.Logger
}

// Manager opens sessions. One is enough for a deployment.
//
// It holds no registry of open sessions: the snapshot is firefly's, and a registry here
// would be a second copy of the truth to keep in step.
type Manager struct {
	placement *placement.Placement
	firefly   *firefly.Firefly
	runner    Runner
	ttl       time.Duration
	now       func() time.Time
	log       *slog.Logger
}

// New validates the configuration and returns the manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Placement == nil {
		return nil, fmt.Errorf("%w: no Placement", ErrConfig)
	}
	if cfg.Firefly == nil {
		return nil, fmt.Errorf("%w: no Firefly", ErrConfig)
	}
	if cfg.Runner == nil {
		return nil, fmt.Errorf("%w: no Runner", ErrConfig)
	}
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: TTL must be positive", ErrConfig)
	}

	m := &Manager{
		placement: cfg.Placement,
		firefly:   cfg.Firefly,
		runner:    cfg.Runner,
		ttl:       ttl,
		now:       cfg.Now,
		log:       cfg.Log,
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	return m, nil
}

// Request is a caller asking for a session.
type Request struct {
	// ID is the session's identity: lowercase letters, digits, dashes and underscores,
	// starting with a letter or digit. Required, and refused rather than sanitised --
	// a mangled id is a session nobody can find again.
	ID string

	// TTL is this session's lease length. Zero means the manager's.
	TTL time.Duration
}

// Open places the session and starts pi.
//
// The three answers match placement's, because this is where they become a caller's:
//
//   - (*Session, nil): pi is running and the lease is held.
//   - (nil, ErrPending): the machine is booting. Ask again; a retry is cheap.
//   - (nil, err): nothing will change until a session ends, or something is wrong.
//
// ctx governs the process rather than the call: a caller that hands it a request-scoped
// context kills pi when the request returns. The context to pass is the one belonging to
// the stream that will carry the session.
//
// There is no `now` argument, deliberately. Placement and the renewal window are the same
// arithmetic on the same lease, and taking the instant from the caller while renewal read
// the manager's clock was two clocks to keep in step: a caller a minute ahead of Config.Now
// acquired a lease that the first renewal would have called lapsed. The manager's clock is
// the deployment's, and a test moves it.
func (m *Manager) Open(ctx context.Context, req Request) (*Session, error) {
	now := m.now()

	if !validID(req.ID) {
		return nil, fmt.Errorf("%w: %q", ErrBadID, req.ID)
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = m.ttl
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: TTL must be positive", ErrConfig)
	}

	seat, err := m.placement.Place(ctx, placement.Request{
		Holder: "session:" + req.ID,
		Kind:   firefly.Session,
		TTL:    ttl,
	}, now)
	if err != nil {
		return nil, err
	}
	if seat.Pending {
		// The machine is empty when a create backed off before an id existed. That is
		// still "ask again", so it is the same answer without the empty name in it.
		if seat.Machine == "" {
			return nil, ErrPending
		}
		return nil, fmt.Errorf("%w: %s", ErrPending, seat.Machine)
	}

	proc, err := m.runner.Run(ctx, seat.Machine, seat.Name, Command(req.ID))
	if err != nil {
		startErr := fmt.Errorf("session: starting pi on %s: %w", seat.Machine, err)
		// The lease was taken to run this process. Handing it back has to work even if
		// ctx is the reason the start failed.
		if rerr := m.firefly.Release(context.WithoutCancel(ctx), seat.Lease); rerr != nil {
			return nil, errors.Join(startErr, fmt.Errorf("session: releasing the lease on %s: %w", seat.Machine, rerr))
		}
		return nil, startErr
	}

	return &Session{
		id:      req.ID,
		machine: seat.Machine,
		lease:   seat.Lease,
		ttl:     ttl,
		ctx:     ctx,
		proc:    proc,
		reader:  relay.NewReader(proc.Stdout()),
		writer:  relay.NewWriter(proc.Stdin()),
		f:       m.firefly,
		now:     m.now,
		log:     m.log,
		renewAt: now.Add(ttl / 2),
	}, nil
}

// Session is one running pi, with the machine held open under it.
//
// Next is the single reader and Send the single writer; Close may be called from another
// goroutine, which is what a phone hanging up is.
type Session struct {
	id      string
	machine firefly.MachineID
	lease   firefly.LeaseID
	ttl     time.Duration
	ctx     context.Context
	proc    Proc
	reader  *relay.Reader
	writer  *relay.Writer
	f       *firefly.Firefly
	now     func() time.Time
	log     *slog.Logger

	mu       sync.Mutex
	renewAt  time.Time
	released bool
	closed   bool
	// pending is a terminal failure found after a record arrived. The record is still
	// delivered, and the next call reports the reason: a lapse is not a reason to drop the
	// last thing the session said.
	pending error
}

// ID is the session's id, Machine the machine it runs on, and Lease the lease holding it.
func (s *Session) ID() string                 { return s.id }
func (s *Session) Machine() firefly.MachineID { return s.machine }
func (s *Session) Lease() firefly.LeaseID     { return s.lease }

// Next returns the next record pi sent, renewing the lease on activity.
//
// It returns io.EOF when the stream ended cleanly, and the command's own failure when it
// ended because pi failed -- the two are separated here rather than in the relay because
// only this side holds the process. Either way the lease has been released by the time the
// error is returned: the session is over. The process is not killed here; Close is what
// stops it, and a caller that does not call Close leaves a process the machine's drain
// window will outlive.
func (s *Session) Next(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	if s.pending != nil {
		err := s.pending
		s.pending = nil
		s.mu.Unlock()
		return nil, s.withRelease(err)
	}
	s.mu.Unlock()

	rec, err := s.reader.Next()
	if err != nil {
		// The stream is over, whether it ended or broke. If it ended, ask the process
		// what it made of the session; a failed exit is the answer rather than EOF.
		if errors.Is(err, io.EOF) {
			if werr := s.proc.Wait(ctx); werr != nil {
				err = fmt.Errorf("session: pi on %s ended: %w", s.machine, werr)
			}
		}
		return nil, s.withRelease(err)
	}

	if rerr := s.renew(ctx); rerr != nil {
		s.mu.Lock()
		s.pending = rerr
		s.mu.Unlock()
	}
	return rec, nil
}

// Send writes one record to pi, renewing the lease on activity.
//
// It is one writer because the relay's writer is: a stream with two writers interleaves
// records, and the far side reads JSON per line.
func (s *Session) Send(ctx context.Context, rec []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writer.Write(rec); err != nil {
		return fmt.Errorf("session: writing to pi on %s: %w", s.machine, err)
	}
	if rerr := s.renew(ctx); rerr != nil {
		return s.withRelease(rerr)
	}
	return nil
}

// Close ends the session: it asks pi to shut down by closing its stdin, waits for it
// bounded by ctx, and gives the machine back.
//
// The order is pi's own contract. A signal would work too, but stdin EOF is the one the
// protocol documents as orderly, and it leaves pi free to finish what it was doing.
// A command still running when the deadline passes is killed by Wait, and the deadline is
// what Close reports.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	var errs []error
	if err := s.proc.Stdin().Close(); err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		errs = append(errs, fmt.Errorf("session: closing pi's stdin: %w", err))
	}
	if werr := s.proc.Wait(ctx); werr != nil {
		errs = append(errs, fmt.Errorf("session: pi on %s: %w", s.machine, werr))
	}
	if cerr := s.proc.Close(); cerr != nil {
		errs = append(errs, fmt.Errorf("session: closing the stream on %s: %w", s.machine, cerr))
	}
	if rerr := s.release(); rerr != nil {
		errs = append(errs, rerr)
	}
	return errors.Join(errs...)
}

// renew extends the lease if half of it has passed since the last renewal.
//
// A lapse is terminal: firefly is about to drain the machine, and a session on it is a
// session about to lose its work.
//
// Any other failure leaves the window where it was, so the next record tries again. The
// first version advanced it before the attempt on the reasoning that a failing store should
// not be written to once per record -- but that puts the retry at exactly the lease's own
// expiry, which is the one moment a retry cannot help: a store that came back one second
// after the failure would still find the session gone. One write per record while a store
// is down is the cheaper mistake.
func (s *Session) renew(ctx context.Context) error {
	now := s.now()

	s.mu.Lock()
	if now.Before(s.renewAt) {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	err := s.f.Renew(ctx, s.lease, s.ttl, now)
	if err == nil {
		s.mu.Lock()
		s.renewAt = now.Add(s.ttl / 2)
		s.mu.Unlock()
		return nil
	}
	if errors.Is(err, firefly.ErrLeaseExpired) {
		return fmt.Errorf("session: the lease on %s lapsed: %w", s.machine, err)
	}
	s.log.Error("renewing the session lease failed", "machine", s.machine, "err", err)
	return nil
}

// withRelease gives the machine back and returns err beside any failure to do so.
//
// It returns err bare when the release worked, rather than through errors.Join, because
// io.EOF is compared by value by every reader in Go: a joined end of stream is an end of
// stream that half the callers in the world would miss.
func (s *Session) withRelease(err error) error {
	if rerr := s.release(); rerr != nil {
		return errors.Join(err, rerr)
	}
	return err
}

// release gives the machine back, once.
//
// It runs on a context that cannot be cancelled, and that is the whole point: the usual
// reason a stream ended is that the caller gave up, which cancels the session's context,
// and a release that inherited it would fail exactly then.
func (s *Session) release() error {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return nil
	}
	s.released = true
	s.mu.Unlock()

	if err := s.f.Release(context.WithoutCancel(s.ctx), s.lease); err != nil {
		// Let a later Close try again rather than pretending the machine was given back.
		s.mu.Lock()
		s.released = false
		s.mu.Unlock()
		return fmt.Errorf("session: releasing the lease on %s: %w", s.machine, err)
	}
	return nil
}

// validID reports whether id can be a holder name, a directory, and pi's session id at
// once.
//
// Lowercase only, deliberately: the value becomes a path element, and a case-insensitive
// filesystem would make of two ids that differ only in case one directory -- one session's
// checkout overwritten by another's.
func validID(id string) bool {
	if id == "" || len(id) > maxID {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}
