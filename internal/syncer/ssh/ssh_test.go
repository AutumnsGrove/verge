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

	"github.com/AutumnsGrove/verge/internal/syncer/ssh"
)

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

func fetcher(t *testing.T, binary string, opts ...func(*ssh.Config)) *ssh.Fetcher {
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
		f := fetcher(t, script(t, `printf 'a tarball, honestly'`))

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
		f := fetcher(t, script(t, `printf '%s\n' "$@" > `+out))

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
		f := fetcher(t, script(t, `printf '%s\n' "$@" > `+out), func(c *ssh.Config) {
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
		f := fetcher(t, script(t, `printf 'half a tar'; echo 'the disk is full' >&2; exit 3`))

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
		f := fetcher(t, script(t, `sleep 30`))

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
		f := fetcher(t, script(t, `sleep 30`))

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
		f := fetcher(t, script(t, `touch `+marker))

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
		f := fetcher(t, filepath.Join(t.TempDir(), "no-such-binary"))
		if rc, err := f.Fetch(context.Background(), "fake-001", "verge-machine-1"); err == nil {
			rc.Close()
			t.Fatal("Fetch with no ssh binary = nil error, want a failure")
		}
	})
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
	f := fetcher(t, script(t, `sleep 30 & echo ready; wait`))

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
