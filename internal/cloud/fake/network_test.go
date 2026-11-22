package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

func newBoth(t *testing.T) (*Cloud, *Network) {
	t.Helper()
	c := New(fakeBase).Boot(0)
	return c, NewNetwork(c)
}

func TestNodeJoinsAfterBoot(t *testing.T) {
	ctx := context.Background()

	t.Run("happy/node_appears_once_the_machine_runs", func(t *testing.T) {
		c := New(fakeBase).Boot(2 * time.Minute)
		n := NewNetwork(c).JoinDelay(30 * time.Second)

		id, err := c.Create(ctx, spec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		if joined, err := n.Joined(ctx, id); err != nil || joined {
			t.Fatalf("Joined before boot = %v, %v; want false, nil", joined, err)
		}

		c.Advance(2 * time.Minute) // running, but the node has not joined yet
		if joined, err := n.Joined(ctx, id); err != nil || joined {
			t.Fatalf("Joined at boot = %v, %v; want false, nil", joined, err)
		}
		if exists, err := n.NodeExists(ctx, id); err != nil || exists {
			t.Fatalf("NodeExists before the join = %v, %v; want false, nil", exists, err)
		}

		c.Advance(30 * time.Second)
		if joined, err := n.Joined(ctx, id); err != nil || !joined {
			t.Fatalf("Joined after the join delay = %v, %v; want true, nil", joined, err)
		}
	})

	t.Run("happy/never_booted_machine_has_no_node", func(t *testing.T) {
		c := New(fakeBase).StallBoot()
		n := NewNetwork(c)

		id, err := c.Create(ctx, spec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		c.Advance(24 * time.Hour)

		if exists, err := n.NodeExists(ctx, id); err != nil || exists {
			t.Fatalf("NodeExists for a machine that never booted = %v, %v; want false, nil", exists, err)
		}
	})
}

// TestMetadataMissing is the second half of the readiness rule: the machine runs,
// the provider is happy, and nothing ever joins.
func TestMetadataMissing(t *testing.T) {
	ctx := context.Background()
	c, n := newBoth(t)
	n.MetadataMissing()

	id, err := c.Create(ctx, spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.Advance(24 * time.Hour)

	got, err := c.Describe(ctx, id)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got.Status != firefly.ProviderRunning {
		t.Fatalf("the provider reports %s, want %s", got.Status, firefly.ProviderRunning)
	}
	if joined, err := n.Joined(ctx, id); err != nil || joined {
		t.Fatalf("Joined with a missing metadata service = %v, %v; want false, nil", joined, err)
	}
	if exists, err := n.NodeExists(ctx, id); err != nil || exists {
		t.Fatalf("NodeExists with a missing metadata service = %v, %v; want false, nil", exists, err)
	}
}

// TestLingeringNode is the reason the two methods are different. The provider has
// forgotten the machine; the network has not.
func TestLingeringNode(t *testing.T) {
	ctx := context.Background()
	c, n := newBoth(t)

	id, err := c.Create(ctx, spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.Advance(time.Minute)
	if joined, err := n.Joined(ctx, id); err != nil || !joined {
		t.Fatalf("the machine never joined, so the rest of this test means nothing")
	}

	if err := c.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	c.Advance(time.Minute)
	if _, err := c.Describe(ctx, id); !errors.Is(err, firefly.ErrNoSuchMachine) {
		t.Fatalf("the machine is still there: %v", err)
	}

	t.Run("sad/node_outlives_its_machine", func(t *testing.T) {
		if exists, err := n.NodeExists(ctx, id); err != nil || !exists {
			t.Fatalf("NodeExists after the machine was deleted = %v, %v; want true so the repair has work to do", exists, err)
		}
	})

	t.Run("sad/a_lingering_node_is_not_joined", func(t *testing.T) {
		if joined, err := n.Joined(ctx, id); err != nil || joined {
			t.Fatalf("Joined for a deleted machine = %v, %v; want false, nil", joined, err)
		}
	})

	t.Run("happy/forget_clears_the_record", func(t *testing.T) {
		if err := n.Forget(ctx, id); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if exists, err := n.NodeExists(ctx, id); err != nil || exists {
			t.Fatalf("NodeExists after Forget = %v, %v; want false, nil", exists, err)
		}
	})
}

func TestForgetIsIdempotent(t *testing.T) {
	ctx := context.Background()
	_, n := newBoth(t)

	t.Run("happy/forgetting_an_unknown_id_is_success", func(t *testing.T) {
		if err := n.Forget(ctx, "never-existed"); err != nil {
			t.Fatalf("Forget of an unknown id = %v, want nil", err)
		}
	})

	t.Run("happy/forgetting_twice_is_success", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if err := n.Forget(ctx, "never-existed"); err != nil {
				t.Fatalf("Forget #%d = %v, want nil", i+1, err)
			}
		}
	})
}

func TestForgetRefuses(t *testing.T) {
	ctx := context.Background()
	c, n := newBoth(t)
	n.ForgetRefuses(2)

	id, err := c.Create(ctx, spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.Advance(time.Minute)

	for i := 1; i <= 2; i++ {
		if err := n.Forget(ctx, id); err == nil {
			t.Fatalf("Forget #%d succeeded, want a refusal", i)
		}
	}
	if err := n.Forget(ctx, id); err != nil {
		t.Fatalf("Forget after the refusals: %v", err)
	}
	if exists, err := n.NodeExists(ctx, id); err != nil || exists {
		t.Fatalf("the node survived a successful Forget: %v, %v", exists, err)
	}
}

func TestNetworkCancelledContexts(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	c, n := newBoth(t)
	id, err := c.Create(context.Background(), spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cases := []struct {
		name string
		call func() error
	}{
		{"bad/joined", func() error { _, err := n.Joined(cancelled, id); return err }},
		{"bad/node_exists", func() error { _, err := n.NodeExists(cancelled, id); return err }},
		{"bad/forget", func() error { return n.Forget(cancelled, id) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s with a cancelled context = %v, want context.Canceled", tc.name, err)
			}
		})
	}
}

func TestNetworkCounts(t *testing.T) {
	ctx := context.Background()
	c, n := newBoth(t)

	id, err := c.Create(ctx, spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.Advance(time.Minute)
	if _, err := n.Joined(ctx, id); err != nil {
		t.Fatalf("Joined: %v", err)
	}
	if _, err := n.NodeExists(ctx, id); err != nil {
		t.Fatalf("NodeExists: %v", err)
	}
	if err := n.Forget(ctx, id); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	joined, exists, forgets := n.Calls()
	if joined != 1 || exists != 1 || forgets != 1 {
		t.Fatalf("Calls() = %d/%d/%d, want 1/1/1", joined, exists, forgets)
	}
}

// TestSharingOneClock is the property that justified one package: advancing the
// cloud's clock is the only thing that moves either half.
func TestSharingOneClock(t *testing.T) {
	ctx := context.Background()
	c, n := newBoth(t)

	id, err := c.Create(ctx, spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Nothing joins while the cloud's clock says the machine is starting.
	if joined, _ := n.Joined(ctx, id); joined {
		t.Fatal("a node joined without the cloud's clock advancing")
	}
	c.Advance(time.Minute)
	if joined, err := n.Joined(ctx, id); err != nil || !joined {
		t.Fatalf("Joined after advancing the cloud's clock = %v, %v; want true, nil", joined, err)
	}
	if !n.cloud.Now().Equal(c.Now()) {
		t.Fatal("the two halves disagree about the time")
	}
}
