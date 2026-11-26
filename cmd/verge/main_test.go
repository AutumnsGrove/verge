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

	"github.com/AutumnsGrove/verge/internal/config"
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
		// The store validates its keys, and a label is "key=value", so anything the
		// label carries that a key element cannot must be mapped rather than passed
		// through: a prefix that fails validation would fail every save.
		for _, r := range got {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '/'
			if !ok {
				t.Fatalf("workspacePrefix(%q) = %q, which is not a valid key prefix", label, got)
			}
		}
	}
	if workspacePrefix("a=b") == workspacePrefix("c=d") {
		t.Fatal("two deployments share a workspace prefix")
	}
}

// TestBuildNeedsNoNetwork is the rule build's doc comment states: constructing the
// clients validates them without dialling, so a bad configuration is a refusal at
// startup rather than a failure on the first tick. The endpoint below is unroutable
// on purpose -- if anything dialled, this would hang or time out.
func TestBuildNeedsNoNetwork(t *testing.T) {
	cfg := &config.Config{
		HetznerToken:      "provider-token",
		TailnetToken:      "tailnet-token",
		S3AccessKey:       "access-key",
		S3SecretKey:       "secret-key",
		TailnetName:       "-",
		R2Endpoint:        "http://127.0.0.1:1",
		R2Bucket:          "verge-state",
		R2Key:             "state.json",
		Label:             "verge=machine",
		Image:             "ubuntu-24.04",
		Region:            "fsn1",
		Size:              "cpx11",
		Interval:          time.Minute,
		DrainWindow:       15 * time.Minute,
		MaxLifetime:       4 * time.Hour,
		BootDeadline:      10 * time.Minute,
		TerminateDeadline: 5 * time.Minute,
		RetryBase:         30 * time.Second,
		RetryMax:          5 * time.Minute,
		SyncAttempts:      3,
	}

	f, err := build(cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := f.Config()
	if got.Syncer == nil || got.Cloud == nil || got.Network == nil || got.Store == nil {
		t.Fatalf("build left a dependency nil: %+v", got)
	}
	if got.Label != cfg.Label || got.MaxLifetime != cfg.MaxLifetime {
		t.Fatalf("build did not carry the configuration through: %+v", got)
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
