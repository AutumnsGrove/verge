// Package ssh is the transport: it runs one command on a machine and hands the
// syncer that command's stdout.
//
// The machine-side half is a small program the image provides — `verge-save`, the
// sibling of the `verge-join` the cloud-init renderer already assumes — which tars the
// workspace and writes it to stdout. This half knows nothing about tar, because the
// format of a workspace is the machine's business: what the control plane needs is a
// stream it can spool and store.
//
// # Why ssh, and the judgement calls in it
//
// The machines are on a tailnet, and Tailscale SSH is the one way to reach them that
// needs no new listener, no port on the public internet, and no second credential:
// the tailnet token already decides who may connect. So the transport shells out to
// ssh, which means the control plane needs the ssh binary and nothing else.
//
// Three things here are decisions the specification did not settle:
//
//  1. **A destination is validated, not trusted.** The name is minted by firefly, but
//     it is the string that becomes an ssh argument, and a name with a space in it is
//     a name that arrives as two arguments. The same rule as an object key applies.
//  2. **A non-zero exit is an error, even with output.** A partial tarball that reads
//     to EOF is indistinguishable from a complete one to whoever restores it, so the
//     exit status is checked when the stream ends and its stderr is carried into the
//     error.
//  3. **Cancellation kills the process.** exec.CommandContext is what makes the
//     Fetcher contract true — the reader unblocks when the context ends — rather than
//     a promise the machine would have to keep.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

const (
	// DefaultCommand is what the image must provide: a program that tars the
	// workspace to stdout. It is the sibling of verge-join, and the same contract
	// applies — if it is missing, every save fails loudly rather than silently.
	DefaultCommand = "/usr/local/bin/verge-save"

	// DefaultUser is the account on the machine. It matches the one the cloud-init
	// renderer expects the agent to run as.
	DefaultUser = "verge"

	// maxStderr is how much of the machine's stderr is kept for the error. Enough
	// for a message an operator wrote, bounded because it comes from a machine.
	maxStderr = 4 << 10
)

// ErrConfig means the transport cannot be built from its configuration.
var ErrConfig = errors.New("ssh: invalid configuration")

// ErrBadName means the machine's name cannot be an ssh destination.
var ErrBadName = errors.New("ssh: the machine name cannot be a host")

// Config is how to reach a machine.
type Config struct {
	// Binary is what runs the command: "ssh" is the ordinary value, and a test
	// passes a script. Required.
	Binary string

	// User is the account on the machine. Empty means DefaultUser.
	User string

	// Args are extra arguments placed before the destination, such as
	// "-o", "StrictHostKeyChecking=accept-new". They are the deployment's policy,
	// not this package's: which host keys a control plane accepts is not a decision
	// a transport should make for it.
	Args []string

	// Command is what runs on the machine. Empty means DefaultCommand.
	Command string
}

// Fetcher runs the save command on a machine over ssh.
type Fetcher struct {
	binary  string
	user    string
	args    []string
	command string
}

// New validates the configuration and returns the transport.
//
// Nothing is executed here: whether ssh exists is discovered on the first save, which
// is where it matters.
func New(cfg Config) (*Fetcher, error) {
	if cfg.Binary == "" {
		return nil, fmt.Errorf("%w: no Binary", ErrConfig)
	}
	user := cfg.User
	if user == "" {
		user = DefaultUser
	}
	if !validDest(user) {
		return nil, fmt.Errorf("%w: User %q is not an account name", ErrConfig, user)
	}
	command := cfg.Command
	if command == "" {
		command = DefaultCommand
	}
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, "\n\x00") {
		return nil, fmt.Errorf("%w: Command %q is not a command", ErrConfig, cfg.Command)
	}

	args := make([]string, len(cfg.Args))
	copy(args, cfg.Args)

	return &Fetcher{binary: cfg.Binary, user: user, args: args, command: command}, nil
}

// Fetch runs the save command on the machine and returns its stdout.
//
// The error returned here is only for the failures that happen before the command
// starts. Everything after that — a non-zero exit, a killed process, a machine that
// stopped talking — surfaces from the reader, so that a half-written stream cannot be
// mistaken for a finished one.
func (f *Fetcher) Fetch(ctx context.Context, id firefly.MachineID, name string) (io.ReadCloser, error) {
	if !validDest(name) {
		return nil, fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dest := f.user + "@" + name
	args := append(append([]string{}, f.args...), dest, f.command)

	cmd := exec.CommandContext(ctx, f.binary, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh: piping stdout for %s: %w", id, err)
	}
	errBuf := &tail{limit: maxStderr}
	cmd.Stderr = errBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh: starting %s for %s: %w", dest, id, err)
	}
	return &stream{cmd: cmd, out: out, stderr: errBuf, dest: dest}, nil
}

// stream is the command's stdout, with the exit status checked when it ends.
type stream struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	stderr *tail
	dest   string

	once sync.Once

	wait     error // the command's failure, if it had one
	ended    bool  // Read reached the end of the stream
	reported bool  // Read already returned the failure, so Close stays quiet
}

var _ io.ReadCloser = (*stream)(nil)

// Read returns the machine's output, and reports the command's failure instead of EOF
// when the output ends early.
func (s *stream) Read(p []byte) (int, error) {
	n, err := s.out.Read(p)
	if err == nil {
		return n, nil
	}

	// EOF or a broken pipe: either way the command has finished or been killed, and
	// its status decides whether this stream is a workspace at all. Reaping here is
	// also what makes Wait's closing of the pipe the last thing that happens.
	s.ended = true
	s.reap()
	if s.wait != nil {
		s.reported = true
		return n, fmt.Errorf("ssh: %s: %w%s", s.dest, s.wait, s.stderrSuffix())
	}
	if err != io.EOF {
		s.reported = true
	}
	return n, err
}

// Close releases the pipe whether or not the stream was read to the end.
//
// A save that is abandoned half way leaves a command still writing into a pipe nobody
// reads, so Close waits for it: the process is killed by the context if that is why
// this is being called, and reaped either way.
func (s *stream) Close() error {
	// Once Wait has run, exec has already closed the pipe on this side, so closing
	// it again is "file already closed" rather than a problem.
	var err error
	if !s.ended {
		err = s.out.Close()
		if errors.Is(err, os.ErrClosed) {
			err = nil
		}
	}
	s.reap()
	if err != nil {
		return err
	}
	if s.reported {
		return nil
	}
	if s.wait != nil {
		return fmt.Errorf("ssh: %s: %w%s", s.dest, s.wait, s.stderrSuffix())
	}
	return nil
}

// reap waits for the command once, and remembers why it failed.
func (s *stream) reap() {
	s.once.Do(func() {
		if err := s.cmd.Wait(); err != nil {
			s.wait = err
		}
	})
}

func (s *stream) stderrSuffix() string {
	if line := strings.TrimSpace(s.stderr.String()); line != "" {
		return ": " + line
	}
	return ""
}

// tail keeps the last limit bytes written to it, and discards the rest.
//
// The machine's stderr is untrusted input of unknown size; an error message is worth
// carrying, and an error message that grows with the machine's log is not.
type tail struct {
	limit int
	buf   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.buf) }

// validDest reports whether s can be one destination element: an account or a host.
//
// Same rule as an object key element, for the same reason: the value arrives from
// somewhere this package does not control, and it becomes an argument to a program
// that treats a space or a leading dash as syntax.
func validDest(s string) bool {
	if s == "" || s == "." || s == ".." || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
