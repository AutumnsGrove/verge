// The session layer's tests are an integration test: the real placement, the real
// firefly, and the shared fake cloud -- only the machine's process is a double, because
// the thing on the far side is a network and a computer that does not exist offline. That
// is the one seam docs/testing.md allows a test double at, and it is the same seam the
// syncer's fetcher sits on.
package session_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/cloud/fake"
	"github.com/AutumnsGrove/verge/internal/firefly"
	"github.com/AutumnsGrove/verge/internal/placement"
	"github.com/AutumnsGrove/verge/internal/rpc/relay"
	"github.com/AutumnsGrove/verge/internal/session"
)

var base = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// world is one firefly, the placement above it, and a manager above that -- the same
// shape cmd/verge will build.
type world struct {
	t     *testing.T
	cloud *fake.Cloud
	net   *fake.Network
	store *countingStore
	f     *firefly.Firefly
	p     *placement.Placement
	run   *runner
	m     *session.Manager

	// now is the shared clock: the manager reads it and the test moves it, so lease
	// expiry is arithmetic rather than waiting.
	now time.Time
}

func newWorld(t *testing.T, opts ...func(*session.Config)) *world {
	t.Helper()

	w := &world{
		t:     t,
		cloud: fake.New(base).Boot(30 * time.Second),
		store: &countingStore{MemStore: firefly.NewMemStore()},
		now:   base,
	}
	w.net = fake.NewNetwork(w.cloud)

	f, err := firefly.New(firefly.Config{
		Store:             w.store,
		Cloud:             w.cloud,
		Network:           w.net,
		Syncer:            fake.NewSyncer(),
		Label:             "verge=machine",
		MaxLifetime:       4 * time.Hour,
		DrainWindow:       15 * time.Minute,
		BootDeadline:      10 * time.Minute,
		SyncAttempts:      3,
		TerminateDeadline: 5 * time.Minute,
		RetryBase:         30 * time.Second,
		RetryMax:          5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("firefly.New: %v", err)
	}
	w.f = f

	p, err := placement.New(placement.Config{
		Firefly: f,
		NewSpec: func(name string) (firefly.Spec, error) {
			return firefly.Spec{
				CloudInit: []byte("#cloud-config\n# hostname: " + name + "\n"),
				Image:     "ubuntu-24.04",
				Region:    "fsn1",
				Size:      "cx22",
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("placement.New: %v", err)
	}
	w.p = p

	w.run = &runner{}
	cfg := session.Config{
		Placement: p,
		Firefly:   f,
		Runner:    w.run,
		Now:       func() time.Time { return w.now },
	}
	for _, o := range opts {
		o(&cfg)
	}
	m, err := session.New(cfg)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	w.m = m
	return w
}

// tick walks the loop once at now, which is how a provisioned machine becomes READY.
func (w *world) tick(at time.Time) firefly.Result {
	w.t.Helper()
	w.now = at
	if d := at.Sub(w.cloud.Now()); d > 0 {
		w.cloud.Advance(d)
	}
	res, err := w.f.Tick(context.Background(), at)
	if err != nil {
		w.t.Fatalf("Tick at %s: %v", at.Sub(base), err)
	}
	return res
}

// ready drives the machine in the snapshot to READY and returns its id.
func (w *world) ready() firefly.MachineID {
	w.t.Helper()
	for i := 1; i <= 10; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		for _, ev := range w.tick(at).Transitions {
			if ev.To == firefly.Ready {
				return ev.Machine
			}
		}
	}
	w.t.Fatal("no machine became READY")
	return ""
}

// open places a session, retrying through a boot the way a phone's request would: the
// first answer is pending and the caller asks again.
func (w *world) open(id string) *session.Session {
	w.t.Helper()
	return w.openWith(context.Background(), id)
}

// openWith is open on a context the test owns, which is how a caller that gave up is
// modelled.
func (w *world) openWith(ctx context.Context, id string) *session.Session {
	w.t.Helper()
	req := session.Request{ID: id}

	_, err := w.m.Open(ctx, req, w.now)
	if !errors.Is(err, session.ErrPending) {
		w.t.Fatalf("Open before the machine booted = %v, want %v", err, session.ErrPending)
	}
	w.ready()

	sess, err := w.m.Open(ctx, req, w.now)
	if err != nil {
		w.t.Fatalf("Open after the boot: %v", err)
	}
	return sess
}

func (w *world) snapshot() firefly.Snapshot {
	w.t.Helper()
	snap, err := w.f.Snapshot(context.Background(), w.now)
	if err != nil {
		w.t.Fatalf("Snapshot: %v", err)
	}
	return snap
}

func (w *world) live() int {
	w.t.Helper()
	total := 0
	for _, m := range w.snapshot().Machines {
		total += m.Live
	}
	return total
}

// drain ticks, a minute at a time, until every machine is gone: what a session that gave
// its lease back should cause. Bounded, because "destroyed eventually" is not a test.
func (w *world) drain() {
	w.t.Helper()
	for i := 0; i < 60; i++ {
		if len(w.snapshot().Machines) == 0 {
			return
		}
		w.tick(w.now.Add(time.Minute))
	}
	w.t.Fatalf("%d machines still exist with nothing holding them, want 0", len(w.snapshot().Machines))
}

// countingStore counts every attempt to write the document, so a rule about *how often* a
// write happens is a fact rather than an inference from the lease's liveness. It can also
// fail one write, which is what a store having a bad minute looks like.
//
// The lease lifecycle is firefly's and its own tests cover it; what is tested through this
// is the session's policy -- renew at most once per half TTL, retry on the next activity
// after a failure, and never on every record.
type countingStore struct {
	*firefly.MemStore

	mu       sync.Mutex
	attempts int
	failNext bool
}

func (s *countingStore) Save(ctx context.Context, doc firefly.Document) error {
	s.mu.Lock()
	s.attempts++
	fail := s.failNext
	s.failNext = false
	s.mu.Unlock()
	if fail {
		return errors.New("the store is unreachable")
	}
	return s.MemStore.Save(ctx, doc)
}

func (s *countingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *countingStore) failOnce() {
	s.mu.Lock()
	s.failNext = true
	s.mu.Unlock()
}

func TestOpen(t *testing.T) {
	t.Run("happy/a_session_runs_on_the_machine_it_waited_for", func(t *testing.T) {
		w := newWorld(t)
		sess := w.open("s-one")

		if sess.ID() != "s-one" {
			t.Fatalf("ID = %q, want s-one", sess.ID())
		}
		if sess.Machine() == "" {
			t.Fatal("the session names no machine")
		}
		if sess.Lease() == "" {
			t.Fatal("the session holds no lease, so nothing keeps the machine alive")
		}
		if got := len(w.run.ids); got != 1 {
			t.Fatalf("started %d processes, want 1: the pending answer must not start one", got)
		}
		if w.run.ids[0] != sess.Machine() {
			t.Fatalf("started on %q and reports %q", w.run.ids[0], sess.Machine())
		}

		// The name travels rather than the id: the tailnet is keyed on it, and a
		// transport handed only the provider's id would have to ask the provider.
		name, ok := w.cloud.Name(sess.Machine())
		if !ok {
			t.Fatalf("the build has no name for %s", sess.Machine())
		}
		if w.run.names[0] != name {
			t.Fatalf("ssh was pointed at %q, want the machine's name %q", w.run.names[0], name)
		}
	})

	t.Run("happy/the_command_is_what_the_image_must_provide", func(t *testing.T) {
		// The other half of the image's contract, next to verge-join and verge-save: pi
		// in RPC mode, in the session's own directory. A command that guessed a different
		// path would fail on every session with "command not found" and read as a broken
		// machine.
		w := newWorld(t)
		w.open("s-one")

		got := w.run.commands[0]
		for _, want := range []string{"pi --mode rpc", session.DefaultWorkspace + "/s-one"} {
			if !strings.Contains(got, want) {
				t.Fatalf("the remote command %q does not contain %q", got, want)
			}
		}
	})

	t.Run("happy/records_go_both_ways", func(t *testing.T) {
		w := newWorld(t)
		sess := w.open("s-one")
		p := w.run.last()

		const said = `{"type":"agent_settled"}`
		p.say(said)
		rec, err := sess.Next(context.Background())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if string(rec) != said {
			t.Fatalf("Next = %q, want %q", rec, said)
		}

		const prompt = `{"type":"prompt","message":"review this"}`
		if err := sess.Send(context.Background(), []byte(prompt)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		select {
		case got := <-p.said:
			if got != prompt {
				t.Fatalf("pi received %q, want %q", got, prompt)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("pi never received the record")
		}
	})

	t.Run("happy/pending_during_a_boot_starts_no_process", func(t *testing.T) {
		// The caller retries, and a retry during a boot is cheap: it must not buy a
		// process on a machine that cannot accept connections yet, nor a second machine.
		w := newWorld(t)
		_, err := w.m.Open(context.Background(), session.Request{ID: "s-one"}, w.now)
		if !errors.Is(err, session.ErrPending) {
			t.Fatalf("Open = %v, want %v", err, session.ErrPending)
		}
		if len(w.run.commands) != 0 {
			t.Fatalf("a pending placement started %d processes", len(w.run.commands))
		}
		if len(w.cloud.Specs()) != 1 {
			t.Fatalf("provisioned %d machines, want 1", len(w.cloud.Specs()))
		}
	})

	t.Run("sad/a_create_that_backed_off_is_still_pending", func(t *testing.T) {
		// A failed create is placement's "retry shortly", and it arrives here without a
		// machine id. The caller's answer is the same as a booting machine, and the error
		// says so without an empty name in it.
		w := newWorld(t)
		w.cloud.FailCreate(errors.New("quota exceeded"))
		req := session.Request{ID: "s-one"}

		if _, err := w.m.Open(context.Background(), req, w.now); err == nil {
			t.Fatal("the first failed create was hidden")
		}
		_, err := w.m.Open(context.Background(), req, w.now.Add(time.Second))
		if !errors.Is(err, session.ErrPending) {
			t.Fatalf("Open during backoff = %v, want %v", err, session.ErrPending)
		}
		if err.Error() != session.ErrPending.Error() {
			t.Fatalf("Open during backoff = %q, want the bare reason with no machine", err)
		}
	})

	t.Run("sad/a_session_that_cannot_start_holds_no_machine", func(t *testing.T) {
		// The lease was taken to run a process that never started. Keeping it is a machine
		// billing for a session nobody can reach; releasing it lets the drain window do
		// its job.
		w := newWorld(t)
		req := session.Request{ID: "s-one"}
		if _, err := w.m.Open(context.Background(), req, w.now); !errors.Is(err, session.ErrPending) {
			t.Fatalf("Open before the boot = %v, want %v", err, session.ErrPending)
		}
		w.ready()

		w.run.err = errors.New("ssh: the machine never answered")
		if _, err := w.m.Open(context.Background(), req, w.now); err == nil {
			t.Fatal("Open with no transport = nil error, want a failure")
		}
		if got := w.live(); got != 0 {
			t.Fatalf("%d leases live after a failed start, want 0", got)
		}

		// The consequence that matters: nothing holds the machine, so it fades.
		w.drain()
		if n := len(w.cloud.Specs()); n != 1 {
			t.Fatalf("swept %d machines, want the one failed machine to be destroyed", n)
		}
	})

	t.Run("sad/a_cancelled_start_still_hands_the_machine_back", func(t *testing.T) {
		// A caller that gave up and a transport that failed arrive together: the context is
		// dead by the time the start fails. The release must survive that, or the lease the
		// failed start took keeps a machine alive for a session nobody can reach.
		w := newWorld(t)
		req := session.Request{ID: "s-one"}
		if _, err := w.m.Open(context.Background(), req, w.now); !errors.Is(err, session.ErrPending) {
			t.Fatalf("Open before the boot = %v, want %v", err, session.ErrPending)
		}
		w.ready()

		ctx, cancel := context.WithCancel(context.Background())
		w.run.cancel = cancel
		w.run.err = errors.New("ssh: the machine never answered")
		if _, err := w.m.Open(ctx, req, w.now); err == nil {
			t.Fatal("Open with a cancelled context = nil error, want a failure")
		}
		if got := w.live(); got != 0 {
			t.Fatalf("%d leases live after a cancelled start, want 0", got)
		}
	})

	t.Run("bad/an_id_that_cannot_be_a_session", func(t *testing.T) {
		// The id is a holder name, a directory under the workspace, and pi's session id.
		// It is validated once here rather than quoted in three places, and the ones that
		// matter are the ones that would escape the workspace or be read as flags.
		w := newWorld(t)
		for _, id := range []string{"", "two words", "../escape", "-dash", "a/b", "a\nb", strings.Repeat("a", 65)} {
			if _, err := w.m.Open(context.Background(), session.Request{ID: id}, w.now); !errors.Is(err, session.ErrBadID) {
				t.Fatalf("Open(%q) = %v, want %v", id, err, session.ErrBadID)
			}
		}
		if len(w.cloud.Specs()) != 0 {
			t.Fatalf("a refused id provisioned %d machines", len(w.cloud.Specs()))
		}
		if len(w.run.commands) != 0 {
			t.Fatal("a refused id started a process")
		}
	})
}

func TestNext(t *testing.T) {
	t.Run("happy/the_end_of_the_stream_releases_the_lease", func(t *testing.T) {
		// The handoff's rule for the session layer: the lease is held for the session's
		// life and released when the stream ends. The consequence is the whole point --
		// the machine drains, and then it is destroyed.
		w := newWorld(t)
		sess := w.open("s-one")
		if w.live() != 1 {
			t.Fatalf("%d leases live while the session runs, want 1", w.live())
		}

		w.run.last().finish(nil)
		// By value, not errors.Is: a reader's end of stream is compared with ==, and a
		// joined EOF is an EOF half the callers in the world would miss.
		if _, err := sess.Next(context.Background()); err != io.EOF {
			t.Fatalf("Next after the stream ended = %v, want io.EOF", err)
		}
		if got := w.live(); got != 0 {
			t.Fatalf("%d leases live after the stream ended, want 0", got)
		}

		w.drain()
	})

	t.Run("sad/pi_that_died_names_its_exit_status", func(t *testing.T) {
		// A clean end of stream is not a clean end of session: pi emits its last record
		// and then exits non-zero, and a relay that reported EOF would let the phone
		// believe the turn completed.
		w := newWorld(t)
		sess := w.open("s-one")
		w.run.last().finish(errors.New("exit status 1: the model provider refused the token"))

		_, err := sess.Next(context.Background())
		if err == nil {
			t.Fatal("Next after a failed exit = nil, want the failure")
		}
		if errors.Is(err, io.EOF) {
			t.Fatalf("Next = %v, want the exit status rather than EOF", err)
		}
		if !strings.Contains(err.Error(), "refused the token") {
			t.Fatalf("Next = %v, want pi's failure in it", err)
		}
		if got := w.live(); got != 0 {
			t.Fatalf("%d leases live after pi died, want 0", got)
		}
	})

	t.Run("sad/a_record_over_the_limit_is_an_error", func(t *testing.T) {
		// The relay is where the framing rules live; this only asserts the session did not
		// swallow them.
		w := newWorld(t)
		sess := w.open("s-one")
		w.run.last().say(strings.Repeat("x", relay.DefaultLimit+2))

		if _, err := sess.Next(context.Background()); !errors.Is(err, relay.ErrRecordTooLong) {
			t.Fatalf("Next = %v, want %v", err, relay.ErrRecordTooLong)
		}
	})
}

func TestRenew(t *testing.T) {
	t.Run("happy/stream_activity_keeps_the_lease_alive", func(t *testing.T) {
		// A session outlives one lease TTL, so activity renews it. Without this a long
		// session silently loses its machine halfway through.
		w := newWorld(t, func(c *session.Config) { c.TTL = 10 * time.Minute })
		sess := w.open("s-one")

		// Past the half-TTL renewal point but inside the lease: one record arrives, and the
		// machine is held past the lease's original expiry.
		w.now = base.Add(8 * time.Minute)
		w.run.last().say(`{"type":"agent_settled"}`)
		if _, err := sess.Next(context.Background()); err != nil {
			t.Fatalf("Next: %v", err)
		}
		w.now = base.Add(15 * time.Minute)
		if got := w.live(); got != 1 {
			t.Fatalf("%d leases live at %s, want the renewed lease", got, w.now.Sub(base))
		}
	})

	t.Run("happy/records_inside_the_half_ttl_do_not_renew", func(t *testing.T) {
		// The throttle is a write rate, not a behaviour, so the number of store writes is
		// the assertion. Without it, a busy session writes the document once per record.
		w := newWorld(t)
		sess := w.open("s-one")

		before := w.store.count()
		for i := 0; i < 3; i++ {
			w.run.last().say(`{"type":"message_update"}`)
			if _, err := sess.Next(context.Background()); err != nil {
				t.Fatalf("Next: %v", err)
			}
		}
		if got := w.store.count() - before; got != 0 {
			t.Fatalf("%d lease writes for three records inside the half TTL, want 0", got)
		}

		// Past the half TTL the first record renews, and the two that follow it are inside
		// the new window.
		w.now = base.Add(45 * time.Minute)
		for i := 0; i < 3; i++ {
			w.run.last().say(`{"type":"message_update"}`)
			if _, err := sess.Next(context.Background()); err != nil {
				t.Fatalf("Next past the half TTL: %v", err)
			}
		}
		if got := w.store.count() - before; got != 1 {
			t.Fatalf("%d lease writes for three records past the half TTL, want exactly 1", got)
		}
	})

	t.Run("sad/a_failed_renewal_is_retried_on_the_next_record", func(t *testing.T) {
		// A store that has a bad minute must not cost the session: the failed attempt does
		// not move the window, so the next record tries again while the old lease is still
		// live. Advancing the window on failure would put the retry at the lease's expiry,
		// where a successful retry is no longer possible.
		w := newWorld(t, func(c *session.Config) { c.TTL = 4 * time.Hour })
		sess := w.open("s-one")

		// Past the half TTL, and the store refuses the first write.
		w.now = base.Add(3 * time.Hour)
		w.store.failOnce()
		before := w.store.count()

		w.run.last().say(`{"type":"message_update"}`)
		if _, err := sess.Next(context.Background()); err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got := w.store.count() - before; got != 1 {
			t.Fatalf("%d lease writes for the failed renewal, want 1", got)
		}

		// The very next record retries, and this time it works: the lease is live past
		// where the original one would have expired.
		w.run.last().say(`{"type":"message_update"}`)
		if _, err := sess.Next(context.Background()); err != nil {
			t.Fatalf("Next after a failed renewal: %v", err)
		}
		if got := w.store.count() - before; got != 2 {
			t.Fatalf("%d lease writes after the retry, want 2", got)
		}

		w.now = base.Add(6 * time.Hour)
		if got := w.live(); got != 1 {
			t.Fatalf("%d leases live at %s, want the retried lease", got, w.now.Sub(base))
		}
	})

	t.Run("sad/a_lease_that_lapsed_ends_the_session", func(t *testing.T) {
		// An idle session loses its machine -- the lease is renewed by activity, not by
		// existence -- and the next record says so rather than pretending the machine is
		// still there.
		w := newWorld(t, func(c *session.Config) { c.TTL = 10 * time.Minute })
		sess := w.open("s-one")

		// Past the TTL with no activity: the tick prunes the lease.
		w.tick(base.Add(time.Hour))
		if got := w.live(); got != 0 {
			t.Fatalf("%d leases live after the TTL, want 0", got)
		}

		// The record is still delivered -- it is the one thing the session produced --
		// and the lapse is the answer to the next call, which is where a looping caller
		// meets it.
		w.run.last().say(`{"type":"agent_settled"}`)
		if _, err := sess.Next(context.Background()); err != nil {
			t.Fatalf("Next with a record and a lapsed lease = %v, want the record", err)
		}
		if _, err := sess.Next(context.Background()); !errors.Is(err, firefly.ErrLeaseExpired) {
			t.Fatalf("Next on a lapsed lease = %v, want %v", err, firefly.ErrLeaseExpired)
		}
	})
}

func TestClose(t *testing.T) {
	t.Run("happy/closing_asks_pi_to_shut_down_and_gives_the_machine_back", func(t *testing.T) {
		w := newWorld(t)
		sess := w.open("s-one")
		p := w.run.last()

		if err := sess.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if n := p.kills(); n != 0 {
			t.Fatalf("Close killed pi %d times, want an orderly shutdown", n)
		}
		if !p.stdinClosed() {
			t.Fatal("Close did not close pi's stdin, so nothing asked it to finish")
		}
		if got := w.live(); got != 0 {
			t.Fatalf("%d leases live after Close, want 0", got)
		}
		if err := sess.Close(context.Background()); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})

	t.Run("sad/a_shutdown_past_its_deadline_kills_the_command", func(t *testing.T) {
		// pi is given time to finish, and the deadline is the caller's: a machine that has
		// stopped answering must not pin a shutdown open.
		w := newWorld(t)
		sess := w.open("s-one")
		p := w.run.last()
		p.watchIgnored = true // a pi that ignores the pipe ending

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := sess.Close(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close = %v, want the deadline", err)
		}
		if p.kills() == 0 {
			t.Fatal("Close past its deadline did not kill the command")
		}
		if got := w.live(); got != 0 {
			t.Fatalf("%d leases live after a killed shutdown, want 0", got)
		}
	})

	t.Run("happy/an_exit_status_is_reported_rather_than_hidden", func(t *testing.T) {
		w := newWorld(t)
		sess := w.open("s-one")
		w.run.last().finish(errors.New("exit status 2: the workspace is not there"))

		err := sess.Close(context.Background())
		if err == nil || !strings.Contains(err.Error(), "the workspace is not there") {
			t.Fatalf("Close = %v, want pi's exit status in it", err)
		}
	})

	t.Run("happy/a_caller_that_gave_up_still_gives_the_machine_back", func(t *testing.T) {
		// The usual reason a stream ends is that the caller gave up, which cancels the
		// session's context. A release that inherited that context would fail exactly when
		// handing the machine back matters most, and the machine would bill on.
		w := newWorld(t)
		ctx, cancel := context.WithCancel(context.Background())
		sess := w.openWith(ctx, "s-one")

		cancel()
		err := sess.Close(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close on a cancelled session = %v, want the cancellation", err)
		}
		if got := w.live(); got != 0 {
			t.Fatalf("%d leases live after a cancelled close, want 0", got)
		}
	})

	t.Run("happy/close_from_another_goroutine_unblocks_a_read", func(t *testing.T) {
		// A phone that hangs up is Close while a read is waiting on a machine. Under -race
		// this is the case that fails if the bookkeeping is unguarded.
		w := newWorld(t)
		sess := w.open("s-one")

		done := make(chan error, 1)
		go func() {
			_, err := sess.Next(context.Background())
			done <- err
		}()

		if err := sess.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Close did not unblock the read")
		}
	})
}

func TestSend(t *testing.T) {
	t.Run("bad/a_record_that_is_not_one_record", func(t *testing.T) {
		w := newWorld(t)
		sess := w.open("s-one")
		if err := sess.Send(context.Background(), []byte("{\"type\":\"prompt\"}\n{\"type\":\"abort\"}")); !errors.Is(err, relay.ErrRecordHasLF) {
			t.Fatalf("Send = %v, want %v", err, relay.ErrRecordHasLF)
		}
	})
}

func TestNew(t *testing.T) {
	t.Run("bad/missing_pieces", func(t *testing.T) {
		w := newWorld(t)
		for name, cfg := range map[string]session.Config{
			"no placement": {Firefly: w.f, Runner: w.run},
			"no firefly":   {Placement: w.p, Runner: w.run},
			"no runner":    {Placement: w.p, Firefly: w.f},
			"negative ttl": {Placement: w.p, Firefly: w.f, Runner: w.run, TTL: -time.Minute},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := session.New(cfg); !errors.Is(err, session.ErrConfig) {
					t.Fatalf("New = %v, want %v", err, session.ErrConfig)
				}
			})
		}
	})

	t.Run("happy/the_defaults", func(t *testing.T) {
		w := newWorld(t)
		if _, err := session.New(session.Config{Placement: w.p, Firefly: w.f, Runner: w.run}); err != nil {
			t.Fatalf("New: %v", err)
		}
	})
}

// runner is the machine transport at its seam: it records what it was asked to run and
// hands back a process. The real one is internal/machine/ssh, exercised by its own tests.
type runner struct {
	// err fails the start. cancel, when set, is called first: a caller that gave up and a
	// transport that failed are the same instant from the session's side, and that is the
	// case the release rule exists for.
	err      error
	cancel   context.CancelFunc
	procs    []*proc
	ids      []firefly.MachineID
	names    []string
	commands []string
}

func (r *runner) Run(_ context.Context, id firefly.MachineID, name, command string) (session.Proc, error) {
	r.ids = append(r.ids, id)
	r.names = append(r.names, name)
	r.commands = append(r.commands, command)
	if r.err != nil {
		if r.cancel != nil {
			r.cancel()
		}
		return nil, r.err
	}
	p := newProc()
	r.procs = append(r.procs, p)
	return p, nil
}

func (r *runner) last() *proc { return r.procs[len(r.procs)-1] }

// proc is pi on the machine. The pipes are real, so a read the session makes blocks and
// is interrupted exactly as it would be against ssh; what is faked is only the computer
// on the far side.
type proc struct {
	in   *io.PipeReader // what the control plane sent
	inW  *io.PipeWriter
	out  *io.PipeReader // what pi said
	outW *io.PipeWriter

	said chan string // records pi received, in order

	once    sync.Once
	exited  chan struct{}
	waitErr error

	// watchIgnored models a pi that does not shut down when its stdin ends.
	watchIgnored bool

	killed atomic.Int32
	closed atomic.Bool
}

func newProc() *proc {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &proc{in: inR, inW: inW, out: outR, outW: outW, said: make(chan string, 16), exited: make(chan struct{})}
	go p.consume()
	return p
}

// consume is pi reading its commands, and exiting when stdin ends -- pi's own shutdown
// contract.
func (p *proc) consume() {
	br := bufio.NewReader(p.in)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			p.said <- strings.TrimSuffix(line, "\n")
		}
		if err == nil {
			continue
		}
		if p.watchIgnored {
			// A pi that ignores the pipe ending: it stays alive until it is killed.
			return
		}
		p.finish(nil)
		return
	}
}

func (p *proc) Stdin() io.WriteCloser { return p.inW }
func (p *proc) Stdout() io.Reader     { return p.out }

func (p *proc) Wait(ctx context.Context) error {
	select {
	case <-p.exited:
		return p.waitErr
	case <-ctx.Done():
		_ = p.Kill()
		<-p.exited
		return ctx.Err()
	}
}

func (p *proc) Kill() error {
	p.killed.Add(1)
	p.finish(errors.New("pi was killed"))
	return nil
}

func (p *proc) Close() error {
	p.closed.Store(true)
	_ = p.out.Close()
	return nil
}

// say is what pi emits, from its own goroutine: a pipe write blocks until the session
// reads it, and a test that wrote inline would deadlock against the reader it is about to
// become.
func (p *proc) say(records ...string) {
	go func() {
		for _, r := range records {
			if _, err := io.WriteString(p.outW, r+"\n"); err != nil {
				return
			}
		}
	}()
}

// finish ends the process: its output closes and Wait learns why.
func (p *proc) finish(err error) {
	p.once.Do(func() {
		p.waitErr = err
		_ = p.outW.Close()
		close(p.exited)
	})
}

func (p *proc) kills() int32 { return p.killed.Load() }
func (p *proc) stdinClosed() bool {
	// A closed stdin is a pipe read that has ended; reading it again reports the error.
	_, err := p.inW.Write([]byte("probe"))
	return err != nil
}
