package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// env is a complete environment, so a test that cares about one variable does not
// have to spell out the other three.
func env(overrides map[string]string) func(string) string {
	vals := map[string]string{
		"VERGE_HETZNER_TOKEN": "hetzner-token",
		"VERGE_TAILNET_TOKEN": "tailnet-token",
		"VERGE_S3_ACCESS_KEY": "access-key",
		"VERGE_S3_SECRET_KEY": "secret-key",
	}
	for k, v := range overrides {
		vals[k] = v
	}
	return func(k string) string { return vals[k] }
}

// TestRunRefusesAConfigurationItCannotUse is main's one job before it starts: fail
// on the way in, with the missing thing named, rather than on the first tick with
// nothing to point at.
func TestRunRefusesAConfigurationItCannotUse(t *testing.T) {
	var out bytes.Buffer

	if err := run([]string{"-h"}, env(nil), &out); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("run(-h) = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(out.String(), "VERGE_HETZNER_TOKEN") {
		t.Fatalf("the usage text does not name the environment variables:\n%s", out.String())
	}

	// The bucket is required and has no default: a deployment that did not say
	// where its state lives must not start and invent one.
	out.Reset()
	err := run(nil, env(nil), &out)
	if err == nil {
		t.Fatal("run with no bucket = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "r2-bucket") {
		t.Fatalf("run = %v, want it to name r2-bucket", err)
	}

	// A secret missing is the same class of failure, and the message must not print
	// the secrets that were present.
	out.Reset()
	err = run(nil, env(map[string]string{"VERGE_HETZNER_TOKEN": ""}), &out)
	if err == nil {
		t.Fatal("run with no provider token = nil, want a refusal")
	}
	for _, secret := range []string{"tailnet-token", "access-key", "secret-key"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the refusal printed a secret (%s): %v", secret, err)
		}
	}
}

// TestWorkspacePrefix is the one naming rule this file owns.
func TestWorkspacePrefix(t *testing.T) {
	for _, label := range []string{"verge=machine", "a=b"} {
		got := workspacePrefix(label)
		if !strings.HasPrefix(got, "workspaces/") {
			t.Fatalf("workspacePrefix(%q) = %q, want it under workspaces/", label, got)
		}
		if strings.Contains(got, "//") || strings.HasSuffix(got, "/") {
			t.Fatalf("workspacePrefix(%q) = %q, want a clean prefix", label, got)
		}
	}
	if workspacePrefix("a=b") == workspacePrefix("c=d") {
		t.Fatal("two deployments share a workspace prefix")
	}
}

// TestNoTransportIsAnError is the difference between "the machine had nothing" and
// "we could not ask", which is the distinction that decides whether a machine is
// destroyed.
func TestNoTransportIsAnError(t *testing.T) {
	_, err := noTransport{}.Fetch(t.Context(), "fake-001")
	if !errors.Is(err, ErrNoTransport) {
		t.Fatalf("Fetch = %v, want %v", err, ErrNoTransport)
	}
	if !strings.Contains(err.Error(), "fake-001") {
		t.Fatalf("Fetch = %v, want it to name the machine", err)
	}
}

// TestReleaseOnCancelReleasesTheHandler covers the half of the signal wiring that
// cannot be asserted by delivery without a subprocess: the handler is released when
// the context ends, so the second signal is the default action rather than a
// swallowed one.
func TestReleaseOnCancelReleasesTheHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var calls atomic.Int64
	released := make(chan struct{})
	var once sync.Once

	releaseOnCancel(ctx, func() {
		calls.Add(1)
		once.Do(func() { close(released) })
	})

	cancel()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler was not released after the context ended: a second Ctrl-C would be swallowed")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("stop called %d times, want 1", got)
	}
}
