package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// call is one invocation of the tailscale CLI, recorded so a test can assert on it.
type call struct {
	name string
	env  []string
	args []string
}

// recorder stands in for the CLI. It answers `status` with the given state, and
// records everything.
type recorder struct {
	calls  []call
	upErr  error
	status []string // successive answers to `status --json`, last one repeats
	idx    int
}

func (r *recorder) run(ctx context.Context, name string, env []string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, call{name: name, env: env, args: args})
	if len(args) > 0 && args[0] == "up" {
		if r.upErr != nil {
			return nil, r.upErr
		}
		return nil, nil
	}
	// status
	state := "Stopped"
	if len(r.status) > 0 {
		i := r.idx
		if i >= len(r.status) {
			i = len(r.status) - 1
		}
		state = r.status[i]
		if r.idx < len(r.status)-1 {
			r.idx++
		}
	}
	return []byte(`{"BackendState":"` + state + `"}`), nil
}

func (r *recorder) up() call {
	for _, c := range r.calls {
		if len(c.args) > 0 && c.args[0] == "up" {
			return c
		}
	}
	return call{}
}

func (r *recorder) statusCalls() int {
	n := 0
	for _, c := range r.calls {
		if len(c.args) > 0 && c.args[0] == "status" {
			n++
		}
	}
	return n
}

// writeJoin writes a join file and returns its path.
func writeJoin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "join.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the join file: %v", err)
	}
	return path
}

// opts is the test configuration: the timeout is a fake clock, so a poll that never
// succeeds fails instantly instead of waiting five minutes.
func opts(log io.Writer) options {
	o := defaults()
	o.timeout = time.Minute
	o.sleep = func(time.Duration) {}
	o.log = log
	return o
}

const facts = `{"hostname":"verge-machine-1","meshAddr":"https://control.example","authKey":"tskey-abc"}`

func TestJoin(t *testing.T) {
	t.Run("happy/asks_the_cli_to_join_with_the_facts", func(t *testing.T) {
		r := &recorder{status: []string{"Running"}}
		if err := join(t.Context(), writeJoin(t, facts), opts(io.Discard), r.run); err != nil {
			t.Fatalf("join: %v", err)
		}

		up := r.up()
		if up.name != "tailscale" {
			t.Fatalf("ran %q, want tailscale", up.name)
		}
		want := []string{
			"up",
			"--hostname=verge-machine-1",
			"--ssh",
			"--accept-dns=false",
			"--timeout=2m",
			"--login-server=https://control.example",
		}
		if strings.Join(up.args, " ") != strings.Join(want, " ") {
			t.Fatalf("up was run with %v, want %v", up.args, want)
		}
	})

	t.Run("happy/the_key_is_in_the_environment_and_not_in_the_arguments", func(t *testing.T) {
		// A key on a command line is a key in `ps`, which is the reason the join file
		// exists at all.
		r := &recorder{status: []string{"Running"}}
		if err := join(t.Context(), writeJoin(t, facts), opts(io.Discard), r.run); err != nil {
			t.Fatalf("join: %v", err)
		}
		up := r.up()
		if len(up.env) != 1 || up.env[0] != "TS_AUTHKEY=tskey-abc" {
			t.Fatalf("env = %v, want the key in TS_AUTHKEY", up.env)
		}
		for _, arg := range up.args {
			if strings.Contains(arg, "tskey-abc") {
				t.Fatalf("the key appears in the command line: %q", arg)
			}
		}
	})

	t.Run("happy/no_mesh_address_means_the_default_control_plane", func(t *testing.T) {
		r := &recorder{status: []string{"Running"}}
		if err := join(t.Context(), writeJoin(t, `{"hostname":"m1","authKey":"k"}`), opts(io.Discard), r.run); err != nil {
			t.Fatalf("join: %v", err)
		}
		for _, arg := range r.up().args {
			if strings.HasPrefix(arg, "--login-server") {
				t.Fatalf("up was run with %q for an empty mesh address", arg)
			}
		}
	})

	t.Run("happy/waits_until_the_node_is_running", func(t *testing.T) {
		// `tailscale up` returning zero means the request was accepted, not that the
		// node is connected; the join is not done until the backend says so.
		r := &recorder{status: []string{"Stopped", "Starting", "Running"}}
		if err := join(t.Context(), writeJoin(t, facts), opts(io.Discard), r.run); err != nil {
			t.Fatalf("join: %v", err)
		}
		if got := r.statusCalls(); got != 3 {
			t.Fatalf("polled status %d times, want 3", got)
		}
	})

	t.Run("sad/the_cli_refuses_to_join", func(t *testing.T) {
		boom := errors.New("invalid key")
		r := &recorder{upErr: boom, status: []string{"Running"}}
		err := join(t.Context(), writeJoin(t, facts), opts(io.Discard), r.run)
		if !errors.Is(err, boom) {
			t.Fatalf("join = %v, want it to wrap %v", err, boom)
		}
		if got := r.statusCalls(); got != 0 {
			t.Fatalf("polled status %d times after a refused join, want 0", got)
		}
	})

	t.Run("sad/never_connects", func(t *testing.T) {
		// The timeout is a minute of fake time and the poll does not sleep, so this
		// fails as soon as the clock passes the deadline rather than after five minutes.
		o := opts(io.Discard)
		o.timeout = 0
		r := &recorder{status: []string{"Stopped"}}
		err := join(t.Context(), writeJoin(t, facts), o, r.run)
		if err == nil || !strings.Contains(err.Error(), "did not report a connected node") {
			t.Fatalf("join = %v, want a message about the node never connecting", err)
		}
	})

	t.Run("sad/the_context_ends_first", func(t *testing.T) {
		o := opts(io.Discard)
		o.timeout = time.Hour
		r := &recorder{status: []string{"Stopped"}}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := join(ctx, writeJoin(t, facts), o, r.run); !errors.Is(err, context.Canceled) {
			t.Fatalf("join = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("sad/a_status_the_cli_cannot_produce", func(t *testing.T) {
		// Garbage from the CLI is "not running", not a panic and not success.
		r := &recorder{}
		r.status = nil
		o := opts(io.Discard)
		o.timeout = 0
		done := make(chan error, 1)
		go func() {
			done <- join(t.Context(), writeJoin(t, facts), o, func(ctx context.Context, name string, env []string, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "up" {
					return nil, nil
				}
				return []byte("not json"), nil
			})
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("join with an unparseable status = nil, want an error")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("join did not return")
		}
	})

	t.Run("bad/join_files_that_cannot_be_used", func(t *testing.T) {
		cases := map[string]string{
			"empty":         "",
			"not json":      "authKey",
			"no hostname":   `{"authKey":"k"}`,
			"blank name":    `{"hostname":"   ","authKey":"k"}`,
			"no key":        `{"hostname":"m1"}`,
			"hostile name":  `{"hostname":"two words","authKey":"k"}`,
			"slash in name": `{"hostname":"a/b","authKey":"k"}`,
			"dot first":     `{"hostname":".hidden","authKey":"k"}`,
			"dash first":    `{"hostname":"-flag","authKey":"k"}`,
		}
		for name, body := range cases {
			t.Run(name, func(t *testing.T) {
				r := &recorder{status: []string{"Running"}}
				err := join(t.Context(), writeJoin(t, body), opts(io.Discard), r.run)
				if !errors.Is(err, errJoinFile) {
					t.Fatalf("join = %v, want %v", err, errJoinFile)
				}
				if len(r.calls) != 0 {
					t.Fatalf("the CLI was run %d times for an unusable join file", len(r.calls))
				}
			})
		}
	})

	t.Run("bad/a_missing_join_file", func(t *testing.T) {
		r := &recorder{}
		err := join(t.Context(), filepath.Join(t.TempDir(), "absent.json"), opts(io.Discard), r.run)
		if !errors.Is(err, errJoinFile) {
			t.Fatalf("join = %v, want %v", err, errJoinFile)
		}
	})
}

// TestJoinLogsTheNameAndNeverTheKey is the one logging rule here: the success line is
// what an operator greps for, and it must not be the line that leaks the credential.
func TestJoinLogsTheNameAndNeverTheKey(t *testing.T) {
	var log bytes.Buffer
	r := &recorder{status: []string{"Running"}}
	if err := join(t.Context(), writeJoin(t, facts), opts(&log), r.run); err != nil {
		t.Fatalf("join: %v", err)
	}
	if !strings.Contains(log.String(), "verge-machine-1") {
		t.Fatalf("log = %q, want it to name the machine", log.String())
	}
	if strings.Contains(log.String(), "tskey-abc") {
		t.Fatalf("log printed the enrolment key: %q", log.String())
	}
}
