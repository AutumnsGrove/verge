//go:build contract

package tailnet_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/net/tailnet"
)

// The real tailnet, asked about a name that is not in it. Read-only by
// construction: Joined, NodeExists and the lookup behind them issue GETs, and Forget
// is deliberately not called here -- a contract test that removed a device would be
// a test that can break a live tailnet when the name is wrong.
//
// A missing credential skips with a stated reason (docs/testing.md).

func client(t *testing.T) *tailnet.Client {
	t.Helper()
	tok := os.Getenv("VERGE_TAILNET_TOKEN")
	if tok == "" {
		t.Skip("contract: VERGE_TAILNET_TOKEN is not set, so there is no tailnet to ask")
	}
	return tailnet.New(tok, tailnet.WithTailnet(os.Getenv("VERGE_TAILNET_NAME")))
}

// TestContractNodeDoesNotExist is the two facts this client exists to report, from
// the real API rather than from httptest: a name that is not in the tailnet is not a
// node, and is therefore not joined.
func TestContractNodeDoesNotExist(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const absent = "verge-contract-probe-does-not-exist"

	exists, err := client(t).NodeExists(ctx, absent)
	if err != nil {
		t.Fatalf("NodeExists against the real API: %v", err)
	}
	if exists {
		t.Fatalf("NodeExists(%q) = true: the contract tailnet has a device with that name", absent)
	}

	joined, err := client(t).Joined(ctx, absent)
	if err != nil {
		t.Fatalf("Joined against the real API: %v", err)
	}
	if joined {
		t.Fatalf("Joined(%q) = true for a name with no device", absent)
	}
}
