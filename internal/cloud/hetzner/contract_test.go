//go:build contract

package hetzner_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/cloud/hetzner"
	"github.com/AutumnsGrove/verge/internal/firefly"
)

// These are the tests that can tell us the client is right, and the only ones that
// have ever talked to the real API. They are deliberately read-only and free: a
// listing and a describe against a project that holds nothing. Provisioning a real
// machine belongs to the e2e tier, where a machine booting and joining is the thing
// under test rather than a side effect of checking a JSON decoder.
//
// A missing credential skips with a stated reason rather than failing, because a
// red X from a fork should read as "skipped", not "broken" (docs/testing.md).

// token returns the provider token, or skips and says which variable is missing.
func token(t *testing.T) string {
	t.Helper()
	tok := os.Getenv("VERGE_HETZNER_TOKEN")
	if tok == "" {
		t.Skip("contract: VERGE_HETZNER_TOKEN is not set, so there is no project to talk to")
	}
	return tok
}

func client(t *testing.T) *hetzner.Client {
	t.Helper()
	return hetzner.New(token(t))
}

// TestContractListAgainstTheRealAPI is the decode path, end to end: a real listing
// of a project that matches nothing, which still proves the request, the auth, the
// pagination loop and the JSON shape.
func TestContractListAgainstTheRealAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A label no deployment uses: this must not see, or be confused by, real
	// machines. An empty result is the expected answer and the assertion.
	machines, err := client(t).List(ctx, "verge=contract-probe-does-not-exist")
	if err != nil {
		t.Fatalf("List against the real API: %v", err)
	}
	if len(machines) != 0 {
		t.Fatalf("List returned %d machines for a label nothing uses; the contract project is not empty", len(machines))
	}
}

// TestContractDescribeGoneMachine is the 404 mapping, against a real 404 rather than
// an httptest one: the whole point of the sentinel is that the provider's "no such
// server" becomes ErrNoSuchMachine and nothing else does.
func TestContractDescribeGoneMachine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// An id no server can have. Hetzner has never issued one this large in a
	// project; if it ever does, the test reads as a machine this project owns and
	// fails loudly rather than quietly asserting the wrong thing.
	_, err := client(t).Describe(ctx, firefly.MachineID("hetzner:99999999999"))
	if !errors.Is(err, firefly.ErrNoSuchMachine) {
		t.Fatalf("Describe of an id that cannot exist = %v, want %v", err, firefly.ErrNoSuchMachine)
	}
}
