package ssh

// This file is in-package, unlike the rest of the tests here, for one reason: the
// reaped-flag guard in Proc.terminate cannot be observed from outside. A kill sent to a
// process id nobody holds any more succeeds -- ESRCH is mapped to nil because the caller
// wanted the command stopped and it is -- so a test that asserted on the return value
// would pass with the guard removed. That is the shape the reviewer found, and the fix is
// a seam rather than a stronger assertion.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// killScript writes an executable stand-in for ssh that runs body and returns its path.
func killScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing the stand-in: %v", err)
	}
	return path
}

// counting replaces the process's kill with a counter, so "was a signal sent" is a fact
// rather than an inference from a return value.
func (p *Proc) counting(calls *int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signal = func(*exec.Cmd) error {
		*calls++
		return nil
	}
}

func TestTerminate(t *testing.T) {
	run := func(t *testing.T, body string) *Proc {
		t.Helper()
		c, err := New(Config{Binary: killScript(t, body)})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		p, err := c.Run(context.Background(), "fake-001", "verge-machine-1", "pi --mode rpc")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}

	t.Run("happy/a_command_that_is_still_running_is_signalled_once", func(t *testing.T) {
		p := run(t, "sleep 30")
		var calls int
		p.counting(&calls)

		if err := p.Kill(); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		if calls != 1 {
			t.Fatalf("%d signals, want 1", calls)
		}
		// The second kill is the cancellation and the cleanup both arriving.
		if err := p.Kill(); err != nil {
			t.Fatalf("second Kill: %v", err)
		}
		if calls != 1 {
			t.Fatalf("%d signals after a second Kill, want 1", calls)
		}
		_ = p.cmd.Process.Kill()
	})

	t.Run("sad/a_command_that_has_been_reaped_is_never_signalled", func(t *testing.T) {
		// The finding this case exists for: after Wait, the process id may belong to
		// someone else, and a kill there is a kill into an unrelated process group.
		p := run(t, "exit 0")
		var calls int
		p.counting(&calls)

		if err := p.Stdin().Close(); err != nil {
			t.Fatalf("closing stdin: %v", err)
		}
		if err := p.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if err := p.Kill(); err != nil {
			t.Fatalf("Kill after Wait: %v", err)
		}
		if calls != 0 {
			t.Fatalf("%d signals after the command was reaped, want none", calls)
		}
	})
}
