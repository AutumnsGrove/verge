// The tests run a real process. There is no fake ssh client and no mocked
// os/exec -- docs/testing.md rules out mocks at internal seams, and the seam here
// is a pipe, an exit status and a signal, which a fake would not exercise anyway.
package ssh_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/machine/ssh"
	"github.com/AutumnsGrove/verge/internal/syncer"
)

// The save half of this package is firefly's Syncer transport, and the assertion lives
// here so a signature drift is a compile error rather than a save that stops happening.
var _ syncer.Fetcher = (*ssh.Client)(nil)

// script writes an executable stand-in for ssh and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing the stand-in: %v", err)
	}
	return path
}

func client(t *testing.T, binary string, opts ...func(*ssh.Config)) *ssh.Client {
	t.Helper()
	cfg := ssh.Config{Binary: binary}
	for _, o := range opts {
		o(&cfg)
	}
	f, err := ssh.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

func TestFetch(t *testing.T) {
	t.Run("happy/the_command_output_is_the_stream", func(t *testing.T) {
		f := client(t, script(t, `printf 'a tarball, honestly'`))

		rc, err := f.Fetch(context.Background(), "fake-001", "verge-machine-1")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		defer rc.Close()

		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		if string(got) != "a tarball, honestly" {
			t.Fatalf("stream = %q, want the command's output", got)
		}
	})

	t.Run("happy/the_machine_and_command_are_what_the_image_expects", func(t *testing.T) {
		// The remote command is the other half of the image's contract, next to
		// verge-join: a transport that guessed a different path would fail on every
		// save with "command not found" and look like a broken machine.
		out := filepath.Join(t.TempDir(), "args")
		f := client(t, script(t, `printf '%s\n' "$@" > `+out))

		rc, err := f.Fetch(context.Background(), "fake-001", "verge-machine-1")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("reading: %v", err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("reading the recorded args: %v", err)
		}
		got := strings.Fields(string(b))
		if len(got) != 2 || got[0] != "verge@verge-machine-1" || got[1] != ssh.DefaultCommand {
			t.Fatalf("ssh was run with %v, want [verge@verge-machine-1 %s]", got, ssh.DefaultCommand)
		}
	})

	t.Run("happy/extra_arguments_come_before_the_destination", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "args")
		f := client(t, script(t, `printf '%s\n' "$@" > `+out), func(c *ssh.Config) {
			c.Args = []string{"-o", "StrictHostKeyChecking=accept-new"}
			c.User = "runner"
			c.Command = "/opt/verge/save"
		})

		rc, err := f.Fetch(context.Background(), "fake-001", "verge-machine-1")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if _, err := io.ReadAll(rc); err != nil {
			t.Fatalf("reading: %v", err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("reading the recorded args: %v", err)
		}
		got := strings.Fields(string(b))
		want := []string{"-o", "StrictHostKeyChecking=accept-new", "runner@verge-machine-1", "/opt/verge/save"}
		if len(got) != len(want) {
			t.Fatalf("ssh was run with %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ssh was run with %v, want %v", got, want)
			}
		}
	})

	t.Run("sad/a_failed_command_is_not_an_empty_workspace", func(t *testing.T) {
		// The important half: partial output followed by a non-zero exit must be an
		// error, not EOF. An EOF would look like a finished tarball, and firefly
		// would destroy the machine believing the workspace was saved.
		f := client(t, script(t, `printf 'half a tar'; echo 'the disk is full' >&2; exit 3`))

		rc, err := f.Fetch(context.Background(), "fake-001", "verge-machine-1")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		defer rc.Close()

		_, err = io.ReadAll(rc)
		if err == nil {
			t.Fatal("reading a failed command's output = nil, want an error")
		}
		if !strings.Contains(err.Error(), "the disk is full") {
			t.Fatalf("error = %v, want the machine's stderr in it", err)
		}
	})

	t.Run("sad/context_cancelled_unblocks_the_read", func(t *testing.T) {
		// The Fetcher contract says the reader unblocks when the context ends, and
		// this is where that promise is kept: CommandContext kills the process, and
		// the read returns instead of waiting for a machine that has stopped talking.
		f := client(t, script(t, `sleep 30`))

		ctx, cancel := context.WithCancel(context.Background())
		rc, err := f.Fetch(ctx, "fake-001", "verge-machine-1")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		defer rc.Close()

		done := make(chan error, 1)
		go func() {
			_, err := io.ReadAll(rc)
			done <- err
		}()

		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a cancelled fetch read to EOF, want an error")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the read did not unblock after cancellation")
		}
	})

	t.Run("happy/close_from_another_goroutine_interrupts_the_read", func(t *testing.T) {
		// Closing the reader is how a caller gives up on a machine that is not
		// answering, so Read and Close are allowed to be on different goroutines.
		// Under -race this is the case that fails if the bookkeeping is unguarded.
		f := client(t, script(t, `sleep 30`))

		rc, err := f.Fetch(context.Background(), "fake-001", "verge-machine-1")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := io.ReadAll(rc)
			done <- err
		}()

		if err := rc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("the read finished cleanly after Close, want an interruption")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not interrupt the blocked read")
		}

		// Idempotent: the caller that interrupts a read usually also defers Close.
		if err := rc.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})

	t.Run("bad/a_name_that_cannot_be_a_host", func(t *testing.T) {
		// The name is minted by firefly, so this is the second gate -- but it is
		// also the string that becomes an ssh destination, and a name with a space
		// in it is a name that arrives as two arguments.
		marker := filepath.Join(t.TempDir(), "ran")
		f := client(t, script(t, `touch `+marker))

		for _, name := range []string{"", "two words", "-oProxyCommand=evil", "a/b", "..", "host;rm"} {
			if rc, err := f.Fetch(context.Background(), "fake-001", name); err == nil {
				rc.Close()
				t.Fatalf("Fetch(%q) = nil error, want a refusal", name)
			}
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a refused name still started a process")
		}
	})

	t.Run("sad/the_binary_does_not_exist", func(t *testing.T) {
		f := client(t, filepath.Join(t.TempDir(), "no-such-binary"))
		if rc, err := f.Fetch(context.Background(), "fake-001", "verge-machine-1"); err == nil {
			rc.Close()
			t.Fatal("Fetch with no ssh binary = nil error, want a failure")
		}
	})
}

// TestRun is the session half of the transport: a long-lived command with both
// directions wired. The save's stricter rule is TestFetch's; here the two facts — the
// stream ended, the command exited — are deliberately separate, because pi's last record
// and pi's exit status are separate things.
func TestRun(t *testing.T) {
	t.Run("happy/stdin_reaches_the_command_and_stdout_comes_back", func(t *testing.T) {
		c := client(t, script(t, `cat`))

		p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", "cat")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer p.Close()

		in := "{\"type\":\"prompt\"}\n"
		if _, err := io.WriteString(p.Stdin(), in); err != nil {
			t.Fatalf("writing to the command: %v", err)
		}
		if err := p.Stdin().Close(); err != nil {
			t.Fatalf("closing stdin: %v", err)
		}

		got, err := io.ReadAll(p.Stdout())
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		if string(got) != in {
			t.Fatalf("stream = %q, want %q", got, in)
		}
		if err := p.Wait(context.Background()); err != nil {
			t.Fatalf("Wait on an orderly shutdown: %v", err)
		}
	})

	t.Run("happy/the_command_is_the_last_argument", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "args")
		c := client(t, script(t, `printf '%s\n' "$@" > `+out), func(cfg *ssh.Config) {
			cfg.Args = []string{"-o", "StrictHostKeyChecking=accept-new"}
		})

		const command = "/usr/local/bin/pi --mode rpc --no-session"
		p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", command)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer p.Close()
		if err := p.Stdin().Close(); err != nil {
			t.Fatalf("closing stdin: %v", err)
		}
		if err := p.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("reading the recorded args: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		want := []string{"-o", "StrictHostKeyChecking=accept-new", "verge@verge-machine-1", command}
		if len(lines) != len(want) {
			t.Fatalf("ssh was run with %q, want %q", lines, want)
		}
		for i := range want {
			if lines[i] != want[i] {
				t.Fatalf("ssh was run with %q, want %q", lines, want)
			}
		}
	})

	t.Run("sad/a_failed_exit_is_waits_answer_not_the_streams", func(t *testing.T) {
		// The session's half of the save's strictness: two records then a crash is a
		// stream that ended cleanly from the reader's side and a command that failed
		// from the exit status's. Collapsing the two is how a session would look
		// settled when its agent died.
		c := client(t, script(t, `printf 'one\ntwo\n'; echo 'the model is gone' >&2; exit 7`))

		p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", "pi --mode rpc")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer p.Close()

		got, err := io.ReadAll(p.Stdout())
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		if string(got) != "one\ntwo\n" {
			t.Fatalf("stream = %q, want the records that arrived", got)
		}

		err = p.Wait(context.Background())
		if err == nil {
			t.Fatal("Wait after a failed exit = nil, want an error")
		}
		if !strings.Contains(err.Error(), "the model is gone") {
			t.Fatalf("Wait = %v, want the machine's stderr in it", err)
		}
	})

	t.Run("sad/a_command_that_has_not_finished_reports_the_deadline", func(t *testing.T) {
		// A shutdown that waits for pi forever is the shutdown that gets SIGKILLed. The
		// deadline is the caller's, and the process group dies with it.
		c := client(t, script(t, `sleep 30`))

		p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", "pi --mode rpc")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer p.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		start := time.Now()
		err = p.Wait(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait = %v, want the deadline", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Wait took %s, want it bounded by the deadline", elapsed)
		}
	})

	t.Run("sad/kill_stops_the_command", func(t *testing.T) {
		c := client(t, script(t, `sleep 30`))

		p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", "pi --mode rpc")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer p.Close()

		if err := p.Kill(); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		if err := p.Wait(context.Background()); err == nil {
			t.Fatal("Wait after Kill = nil, want the signal that stopped it")
		}
		// Idempotent: a cancellation and a cleanup both arrive here.
		if err := p.Kill(); err != nil {
			t.Fatalf("second Kill: %v", err)
		}
	})

	t.Run("sad/kill_after_the_command_exited_signals_nothing", func(t *testing.T) {
		// Kill on a command the kernel has already reaped must be a no-op. The return
		// value cannot show the difference -- a kill to a free process id looks like a
		// success until the id belongs to someone else -- so this pins the path, and the
		// check that closes the window is the reaped flag in Proc.terminate.
		c := client(t, script(t, `exit 0`))

		p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", "pi --mode rpc")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		defer p.Close()
		if err := p.Stdin().Close(); err != nil {
			t.Fatalf("closing stdin: %v", err)
		}
		if err := p.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if err := p.Kill(); err != nil {
			t.Fatalf("Kill after the command exited: %v", err)
		}
	})

	// TestRun's bad names come after the sad cases: a refused name must not start a
	// process, which is also why every case above closes its stdin before waiting.
	t.Run("bad/a_name_that_cannot_be_a_host", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "ran")
		c := client(t, script(t, `touch `+marker))

		for _, name := range []string{"", "two words", "-oProxyCommand=evil", "a/b", "..", "host;rm"} {
			if p, err := c.Run(context.Background(), "fake-001", name, "pi --mode rpc"); err == nil {
				p.Close()
				t.Fatalf("Run(%q) = nil error, want a refusal", name)
			}
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a refused name still started a process")
		}
	})

	t.Run("bad/a_command_that_is_not_one_command", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "ran")
		c := client(t, script(t, `touch `+marker))

		for _, command := range []string{"", "   ", "pi\nrm -rf /", "pi\x00"} {
			if p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", command); err == nil {
				p.Close()
				t.Fatalf("Run(%q) = nil error, want a refusal", command)
			}
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a refused command still started a process")
		}
	})

	t.Run("sad/the_binary_does_not_exist", func(t *testing.T) {
		c := client(t, filepath.Join(t.TempDir(), "no-such-binary"))
		if p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", "pi --mode rpc"); err == nil {
			p.Close()
			t.Fatal("Run with no ssh binary = nil error, want a failure")
		}
	})
}

// TestRunKillsTheWholeProcessGroup is the session's version of the save's group case: pi
// spawns tools, and a turn that is killed while one of them runs must not leave the tool
// holding the pipe. The stand-in proves the fork happened before anything is killed.
func TestRunKillsTheWholeProcessGroup(t *testing.T) {
	c := client(t, script(t, `sleep 30 & echo ready; wait`))

	p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", "pi --mode rpc")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer p.Close()

	line, err := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the ready line: %v", err)
	}
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("first line = %q, want the stand-in's ready line", line)
	}

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- p.Wait(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Wait after killing a command that had started a child = nil, want the signal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait held on to a child of the command, not the command")
	}
}

func TestNew(t *testing.T) {
	t.Run("bad/no_binary", func(t *testing.T) {
		if _, err := ssh.New(ssh.Config{}); !errors.Is(err, ssh.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, ssh.ErrConfig)
		}
	})

	t.Run("bad/command_that_is_not_a_command", func(t *testing.T) {
		if _, err := ssh.New(ssh.Config{Binary: "ssh", Command: "  "}); !errors.Is(err, ssh.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, ssh.ErrConfig)
		}
	})

	t.Run("bad/user_that_is_not_a_user", func(t *testing.T) {
		for _, user := range []string{"two words", "-oX", "a@b", ""} {
			if user == "" {
				continue // empty means the default
			}
			if _, err := ssh.New(ssh.Config{Binary: "ssh", User: user}); !errors.Is(err, ssh.ErrConfig) {
				t.Fatalf("New(User=%q) = %v, want %v", user, err, ssh.ErrConfig)
			}
		}
	})

	t.Run("happy/defaults", func(t *testing.T) {
		f, err := ssh.New(ssh.Config{Binary: "ssh"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if f == nil {
			t.Fatal("New returned nil without an error")
		}
	})
}

// TestFetchCancelsTheWholeProcessGroup is the case CI found on Linux: a command that
// starts something of its own holds stdout open after the process itself is killed, so the
// reader waits for output nobody is going to write. The stand-in spawns a child
// deliberately; the fix is that the whole group dies.
func TestFetchCancelsTheWholeProcessGroup(t *testing.T) {
	// The stand-in starts a child that inherits stdout and only then says it is ready, so
	// reading that line proves the fork happened. Cancelling before it would pass without
	// the child ever existing -- which is the case this is named for, and the first version
	// of the script printed the line first and made the test flaky for exactly that reason.
	f := client(t, script(t, `sleep 30 & echo ready; wait`))

	ctx, cancel := context.WithCancel(context.Background())
	rc, err := f.Fetch(ctx, "fake-001", "verge-machine-1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()

	br := bufio.NewReader(rc)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the ready line: %v", err)
	}
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("first line = %q, want the stand-in's ready line", line)
	}

	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(br)
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled fetch read to EOF, want an error")
		}
	case <-time.After(2 * time.Second):
		// Under WaitDelay (5s), which would eventually close the pipe: the point is that
		// cancelling a save stops what it started promptly.
		t.Fatal("the read waited for a child of the command, not for the command")
	}
}
