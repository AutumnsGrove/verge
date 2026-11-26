// Package ssh is the transport: it runs a command on a machine and hands the caller
// what that command said.
//
// Two shapes of command use it, and they differ in which way the data runs:
//
//   - A **save**: `verge-save`, the sibling of the `verge-join` the cloud-init renderer
//     already assumes, tars the workspace to stdout. Fetch returns that stream, and
//     turns a non-zero exit into an error rather than a short archive.
//   - A **session**: `pi --mode rpc` speaks JSONL in both directions, so Run returns the
//     process with stdin and stdout wired. The caller frames the records; this package
//     knows nothing about them.
//
// Both start through one path, so the care around a cancelled command is written once.
//
// # Why ssh, and the judgement calls in it
//
// The machines are on a tailnet, and Tailscale SSH is the one way to reach them that
// needs no new listener, no port on the public internet, and no second credential:
// the tailnet token already decides who may connect. So the transport shells out to
// ssh, which means the control plane needs the ssh binary and nothing else.
//
// Four things here are decisions the specification did not settle:
//
//  1. **A destination is validated, not trusted.** The name is minted by firefly, but
//     it is the string that becomes an ssh argument, and a name with a space in it is
//     a name that arrives as two arguments. The same rule as an object key applies.
//  2. **A non-zero exit is an error, even with output.** A partial tarball that reads
//     to EOF is indistinguishable from a complete one to whoever restores it, so Fetch
//     checks the exit status when the stream ends and carries stderr into the error.
//     Run does not: a session's stdout is a record stream, and the end of the stream
//     and the end of the command are two facts, so Wait reports the second.
//  3. **Cancellation kills the process.** exec.CommandContext is what makes the
//     Fetcher contract true — the reader unblocks when the context ends — rather than
//     a promise the machine would have to keep.
//  4. **A command is a string, not a list of words.** ssh joins its arguments into one
//     remote command line, so this package never splits or quotes: the command belongs
//     to whoever wrote it, and a layer here that re-quoted it would be a layer to get
//     quoting wrong in.
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
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

const (
	// DefaultCommand is what Fetch runs, and what the image must provide: a program
	// that tars the workspace to stdout. It is the sibling of verge-join, and the
	// same contract applies — if it is missing, every save fails loudly rather than
	// silently.
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

// ErrCommand means the command Run was handed cannot be one remote command.
var ErrCommand = errors.New("ssh: the command is not a command")

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

	// Command is what Fetch runs on the machine. Empty means DefaultCommand.
	Command string
}

// Client runs commands on machines over ssh.
type Client struct {
	binary  string
	user    string
	args    []string
	command string
}

// New validates the configuration and returns the transport.
//
// Nothing is executed here: whether ssh exists is discovered on the first command,
// which is where it matters.
func New(cfg Config) (*Client, error) {
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
	if !validCommand(command) {
		return nil, fmt.Errorf("%w: Command %q is not a command", ErrConfig, cfg.Command)
	}

	args := make([]string, len(cfg.Args))
	copy(args, cfg.Args)

	return &Client{binary: cfg.Binary, user: user, args: args, command: command}, nil
}

// Fetch runs the save command on the machine and returns its stdout.
//
// The error returned here is only for the failures that happen before the command
// starts. Everything after that — a non-zero exit, a killed process, a machine that
// stopped talking — surfaces from the reader, so that a half-written stream cannot be
// mistaken for a finished one.
func (c *Client) Fetch(ctx context.Context, id firefly.MachineID, name string) (io.ReadCloser, error) {
	p, err := c.Run(ctx, id, name, c.command)
	if err != nil {
		return nil, err
	}
	// The save command reads no input, and closing its stdin is part of saying so:
	// a command that waits on a terminal it will never get is a save that hangs.
	if err := p.Stdin().Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		_ = p.Close()
		return nil, fmt.Errorf("ssh: closing stdin for %s: %w", id, err)
	}
	return &save{p: p}, nil
}

// Run starts command on the machine and returns the process.
//
// The command is passed through as ssh's remote command, so it is one string: the
// caller decides its words, and quoting it here would be a second opinion. A name or a
// command that cannot be one argument is refused before anything starts.
//
// The returned process's stdout is a plain reader. Reaching EOF means the command
// closed its output, not that it succeeded; Wait is what reports the exit status, and
// the caller decides whether that matters.
func (c *Client) Run(ctx context.Context, id firefly.MachineID, name, command string) (*Proc, error) {
	if !validDest(name) {
		return nil, fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if !validCommand(command) {
		return nil, fmt.Errorf("%w: %q", ErrCommand, command)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dest := c.user + "@" + name
	args := append(append([]string{}, c.args...), dest, command)

	cmd := exec.CommandContext(ctx, c.binary, args...)
	// The command leads its own process group, and cancellation kills the group: a
	// command that spawns helpers would otherwise leave them holding stdout open, and
	// the reader would wait for output nobody is going to write.
	configure(cmd)
	cmd.Cancel = func() error { return terminate(cmd) }
	// Belt and braces for the same failure: if anything still holds the pipe, Wait stops
	// waiting for it rather than blocking the caller for as long as the child lives.
	cmd.WaitDelay = 5 * time.Second

	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh: piping stdin for %s: %w", id, err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh: piping stdout for %s: %w", id, err)
	}
	errBuf := &tail{limit: maxStderr}
	cmd.Stderr = errBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh: starting %s for %s: %w", dest, id, err)
	}
	return &Proc{dest: dest, cmd: cmd, stdin: in, stdout: out, stderr: errBuf}, nil
}

// Proc is a command running on a machine.
//
// Its three facts are deliberately separate calls, because the callers need them
// separately: a session reads records until its stream ends, then asks what the exit
// status was, and a save treats the two as one answer.
//
// A Proc that is abandoned must be killed or waited for. Close releases the pipes and
// nothing else, so it cannot block on a machine that stopped answering — which is also
// why it is not enough on its own.
type Proc struct {
	dest   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tail

	once    sync.Once
	waitErr error

	mu     sync.Mutex
	closed bool
	killed bool
}

// Stdin is the command's input. Closing it is how a well-behaved command is told there
// is nothing more to read; a session closes it to ask for an orderly shutdown.
func (p *Proc) Stdin() io.WriteCloser { return p.stdin }

// Stdout is the command's output. It is a plain reader on purpose: EOF here is the end
// of the output, and Wait says whether the command ended well.
func (p *Proc) Stdout() io.Reader { return p.stdout }

// Wait waits for the command to exit and reports why it failed.
//
// If ctx ends first the command's whole process group is killed and the deadline is
// what is returned: a caller that waits on a machine has to be able to stop waiting, and
// the alternative is a shutdown pinned open by a machine that is already gone.
func (p *Proc) Wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- p.reap() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = p.terminate()
		<-done
		return fmt.Errorf("ssh: %s: %w", p.dest, ctx.Err())
	}
}

// Kill stops the command and everything it started. It is safe on a command that has
// already exited.
func (p *Proc) Kill() error { return p.terminate() }

// Close releases the pipes.
//
// It does not wait and does not kill: closing stdout while a read is blocked is how a
// caller interrupts that read, and bundling a kill into it would make the interruption
// a decision about the command rather than about the reader. A caller that is done with
// the command kills it; a caller that wants its verdict calls Wait.
//
// It is idempotent, and a pipe exec has already closed is not an error.
func (p *Proc) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	var err error
	for _, c := range []io.Closer{p.stdin, p.stdout} {
		if cerr := c.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) && err == nil {
			err = cerr
		}
	}
	return err
}

// reap waits for the command once and remembers what it said.
func (p *Proc) reap() error {
	p.once.Do(func() {
		if err := p.cmd.Wait(); err != nil {
			p.waitErr = fmt.Errorf("ssh: %s: %w%s", p.dest, err, p.stderrSuffix())
		}
	})
	return p.waitErr
}

// terminate kills the command's process group once, and remembers that it was asked to.
//
// Killing a command that has already exited is not a failure: the caller wanted it
// stopped, and it is.
func (p *Proc) terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.killed {
		return nil
	}
	p.killed = true
	return terminate(p.cmd)
}

// stderrSuffix is the machine's own words about the failure.
//
// It is only read after the command has exited, which is what keeps it out of a race
// with the goroutine writing stderr.
func (p *Proc) stderrSuffix() string {
	if line := strings.TrimSpace(p.stderr.String()); line != "" {
		return ": " + line
	}
	return ""
}

// save is the reader Fetch returns: a command's stdout with the exit status folded in.
//
// The strictness is the point. A save's output is a tarball, so a partial one that reads
// to EOF is indistinguishable from a complete one, and the only thing that says
// otherwise is the exit status.
type save struct {
	p *Proc

	mu       sync.Mutex
	ended    bool // Read reached the end of the stream
	reported bool // Read already returned the failure, so Close stays quiet
	closed   bool // Close has run; a second one is a no-op
}

var _ io.ReadCloser = (*save)(nil)

// Read returns the machine's output, and reports the command's failure instead of EOF
// when the output ends early.
func (s *save) Read(p []byte) (int, error) {
	n, err := s.p.stdout.Read(p)
	if err == nil {
		return n, nil
	}

	// EOF or a broken pipe: either way the command has finished or been killed, and
	// its status decides whether this stream is a workspace at all.
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()

	werr := s.p.Wait(context.Background())

	s.mu.Lock()
	defer s.mu.Unlock()
	if werr != nil {
		s.reported = true
		return n, werr
	}
	if err != io.EOF {
		s.reported = true
	}
	return n, err
}

// Close releases the stream whether or not it was read to the end.
//
// A save that is abandoned half way leaves a command still writing into a pipe nobody
// reads, so Close stops it: the process is killed by the context if that is why this is
// being called, and reaped either way.
func (s *save) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ended := s.ended
	s.mu.Unlock()

	// Closing the pipe while a read is blocked is the interruption, so this happens
	// before anything that could wait.
	err := s.p.Close()
	if !ended {
		// Closing early means "I do not want the rest of this output", so the
		// command is stopped rather than waited for: reaping alone would block the
		// caller for as long as the machine keeps writing, and the machine is
		// usually the thing that stopped answering.
		_ = s.p.Kill()
	}
	werr := s.p.Wait(context.Background())

	if err != nil {
		return err
	}
	if !ended || werr == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// An interrupted stream is not a failed one: the caller asked for this, so the
	// kill that implemented it is not reported back as the machine's failure.
	if s.reported || !ended {
		return nil
	}
	return werr
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

// validCommand reports whether s can be one remote command.
//
// A NUL cannot cross ssh's command line at all, and a newline would let one command
// become several in a place this package cannot see. Both are refused here rather than
// discovered as a remote shell error.
func validCommand(s string) bool {
	return strings.TrimSpace(s) != "" && !strings.ContainsAny(s, "\n\x00")
}
