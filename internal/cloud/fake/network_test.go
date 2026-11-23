package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// create makes a machine and returns both halves of its identity: the id the
// provider knows it by and the name the network knows it by. They are different
// things, and asking the network about the id is exactly the bug the rekey fixed --
// so a test that only had the id could not ask the question the interface asks.
func create(t *testing.T, c *Cloud) (firefly.MachineID, string) {
	t.Helper()
	id, err := c.Create(context.Background(), spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id, nameOf(t, c, id)
}

// nameOf reports the name the provider minted or was given for id.
//
// A machine the provider never had has no name, so its id stands in: the network's
// answer for an unknown name is the same as for an unknown machine, which is the
// case that is being tested when this matters.
func nameOf(t *testing.T, c *Cloud, id firefly.MachineID) string {
	t.Helper()
	if name, ok := c.Name(id); ok {
		return name
	}
	return string(id)
}

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

		_, name := create(t, c)

		if joined, err := n.Joined(ctx, name); err != nil || joined {
			t.Fatalf("Joined before boot = %v, %v; want false, nil", joined, err)
		}

		c.Advance(2 * time.Minute) // running, but the node has not joined yet
		if joined, err := n.Joined(ctx, name); err != nil || joined {
			t.Fatalf("Joined at boot = %v, %v; want false, nil", joined, err)
		}
		if exists, err := n.NodeExists(ctx, name); err != nil || exists {
			t.Fatalf("NodeExists before the join = %v, %v; want false, nil", exists, err)
		}

		c.Advance(30 * time.Second)
		if joined, err := n.Joined(ctx, name); err != nil || !joined {
			t.Fatalf("Joined after the join delay = %v, %v; want true, nil", joined, err)
		}
	})

	t.Run("happy/never_booted_machine_has_no_node", func(t *testing.T) {
		c := New(fakeBase).StallBoot()
		n := NewNetwork(c)

		_, name := create(t, c)
		c.Advance(24 * time.Hour)

		if exists, err := n.NodeExists(ctx, name); err != nil || exists {
			t.Fatalf("NodeExists for a machine that never booted = %v, %v; want false, nil", exists, err)
		}
	})
}

// TestDeletedBeforeBootNeverJoins is the case the previous version got wrong: a
// machine killed mid-boot still grew a node once the clock passed its boot time,
// because membership was decided from readyAt alone.
func TestDeletedBeforeBootNeverJoins(t *testing.T) {
	ctx := context.Background()

	t.Run("sad/deleted_before_boot_has_no_node", func(t *testing.T) {
		c := New(fakeBase).Boot(2 * time.Minute)
		n := NewNetwork(c).JoinDelay(30 * time.Second)

		id, name := create(t, c)
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)

		if _, err := c.Describe(ctx, id); !errors.Is(err, firefly.ErrNoSuchMachine) {
			t.Fatalf("the machine should be gone: %v", err)
		}
		if exists, err := n.NodeExists(ctx, name); err != nil || exists {
			t.Fatalf("NodeExists for a machine deleted before boot = %v, %v; want false, nil", exists, err)
		}
	})

	t.Run("sad/deleted_during_the_join_delay_has_no_node", func(t *testing.T) {
		c := New(fakeBase).Boot(time.Minute)
		n := NewNetwork(c).JoinDelay(time.Minute)

		id, name := create(t, c)
		c.Advance(time.Minute) // running, join due in another minute
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)

		if exists, err := n.NodeExists(ctx, name); err != nil || exists {
			t.Fatalf("NodeExists for a machine deleted during the join delay = %v, %v; want false, nil", exists, err)
		}
	})

	t.Run("happy/deleted_after_it_joined_still_lingers", func(t *testing.T) {
		c := New(fakeBase).Boot(0)
		n := NewNetwork(c)

		id, name := create(t, c)
		c.Advance(time.Minute)
		if joined, err := n.Joined(ctx, name); err != nil || !joined {
			t.Fatalf("the machine never joined: %v, %v", joined, err)
		}
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)

		// This is the lingering node the deletion rule is written for, and the fix
		// above must not have broken it.
		if exists, err := n.NodeExists(ctx, name); err != nil || !exists {
			t.Fatalf("NodeExists for a machine deleted after joining = %v, %v; want true, nil", exists, err)
		}
	})
}

// TestLingeringNodeWithoutPriming asserts the same fact as TestLingeringNode, but
// never observes the machine while it is alive.
//
// The previous version cached the join instant on first observation, so this case
// reported false for a node that had demonstrably joined. A cached answer made
// the result depend on whether anything happened to ask earlier, which is a fake
// lying in the one direction the deletion rule cares about: TERMINATING would see
// no node, skip Forget, and leave the orphan record behind in production.
func TestLingeringNodeWithoutPriming(t *testing.T) {
	ctx := context.Background()

	t.Run("sad/never_observed_while_alive", func(t *testing.T) {
		c, n := newBoth(t)

		id, name := create(t, c)
		c.Advance(time.Minute) // joined; nothing has asked yet
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)

		if _, err := c.Describe(ctx, id); !errors.Is(err, firefly.ErrNoSuchMachine) {
			t.Fatalf("the machine should be gone: %v", err)
		}
		if exists, err := n.NodeExists(ctx, name); err != nil || !exists {
			t.Fatalf("NodeExists with no priming = %v, %v; want true, nil", exists, err)
		}
	})

	t.Run("sad/asked_once_while_still_booting", func(t *testing.T) {
		c := New(fakeBase).Boot(time.Minute)
		n := NewNetwork(c)

		id, name := create(t, c)
		// Ask while booting: this must not settle the answer.
		if joined, err := n.Joined(ctx, name); err != nil || joined {
			t.Fatalf("Joined while booting = %v, %v; want false, nil", joined, err)
		}

		c.Advance(2 * time.Minute) // running, and past the join instant
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)

		if exists, err := n.NodeExists(ctx, name); err != nil || !exists {
			t.Fatalf("NodeExists after polling during boot = %v, %v; want true, nil", exists, err)
		}
	})

	t.Run("happy/forget_still_clears_it", func(t *testing.T) {
		c, n := newBoth(t)
		id, name := create(t, c)
		c.Advance(time.Minute)
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)

		if err := n.Forget(ctx, name); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if exists, err := n.NodeExists(ctx, name); err != nil || exists {
			t.Fatalf("NodeExists after Forget = %v, %v; want false, nil", exists, err)
		}
	})
}

// TestDeleteReturns404NeverJoins is the same lie as TestDeletedBeforeBootNeverJoins
// through a different door.
//
// A 404 delete reaps the machine immediately and used to record only a flag, so the
// join-instant comparison -- which looked at deleteAt -- never saw it. A machine
// reaped before it booted then grew a node, and TERMINATING was told to Forget one
// that never existed.
func TestDeleteReturns404NeverJoins(t *testing.T) {
	ctx := context.Background()

	t.Run("sad/reaped_before_boot_has_no_node", func(t *testing.T) {
		c := New(fakeBase).Boot(2 * time.Minute).DeleteReturns404()
		n := NewNetwork(c)

		id, name := create(t, c)
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)

		if _, err := c.Describe(ctx, id); !errors.Is(err, firefly.ErrNoSuchMachine) {
			t.Fatalf("the machine should be gone: %v", err)
		}
		if exists, err := n.NodeExists(ctx, name); err != nil || exists {
			t.Fatalf("NodeExists after a 404 reap before boot = %v, %v; want false, nil", exists, err)
		}
	})

	t.Run("happy/reaped_after_it_joined_still_lingers", func(t *testing.T) {
		c := New(fakeBase).Boot(0).DeleteReturns404()
		n := NewNetwork(c)

		id, name := create(t, c)
		c.Advance(time.Minute)
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		c.Advance(time.Hour)

		if exists, err := n.NodeExists(ctx, name); err != nil || !exists {
			t.Fatalf("NodeExists after a 404 reap following a join = %v, %v; want true, nil", exists, err)
		}
	})
}

// TestMetadataMissing is the second half of the readiness rule: the machine runs,
// the provider is happy, and nothing ever joins.
func TestMetadataMissing(t *testing.T) {
	ctx := context.Background()
	c, n := newBoth(t)
	n.MetadataMissing()

	id, name := create(t, c)
	c.Advance(24 * time.Hour)

	got, err := c.Describe(ctx, id)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got.Status != firefly.ProviderRunning {
		t.Fatalf("the provider reports %s, want %s", got.Status, firefly.ProviderRunning)
	}
	if joined, err := n.Joined(ctx, name); err != nil || joined {
		t.Fatalf("Joined with a missing metadata service = %v, %v; want false, nil", joined, err)
	}
	if exists, err := n.NodeExists(ctx, name); err != nil || exists {
		t.Fatalf("NodeExists with a missing metadata service = %v, %v; want false, nil", exists, err)
	}
}

// TestLingeringNode is the reason the two methods are different. The provider has
// forgotten the machine; the network has not.
func TestLingeringNode(t *testing.T) {
	ctx := context.Background()
	c, n := newBoth(t)

	id, name := create(t, c)
	c.Advance(time.Minute)
	if joined, err := n.Joined(ctx, name); err != nil || !joined {
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
		if exists, err := n.NodeExists(ctx, name); err != nil || !exists {
			t.Fatalf("NodeExists after the machine was deleted = %v, %v; want true so the repair has work to do", exists, err)
		}
	})

	t.Run("sad/a_lingering_node_is_not_joined", func(t *testing.T) {
		if joined, err := n.Joined(ctx, name); err != nil || joined {
			t.Fatalf("Joined for a deleted machine = %v, %v; want false, nil", joined, err)
		}
	})

	t.Run("happy/forget_clears_the_record", func(t *testing.T) {
		if err := n.Forget(ctx, name); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if exists, err := n.NodeExists(ctx, name); err != nil || exists {
			t.Fatalf("NodeExists after Forget = %v, %v; want false, nil", exists, err)
		}
	})
}

func TestForgetIsIdempotent(t *testing.T) {
	ctx := context.Background()
	_, n := newBoth(t)

	t.Run("happy/forgetting_an_unknown_name_is_success", func(t *testing.T) {
		if err := n.Forget(ctx, "never-existed"); err != nil {
			t.Fatalf("Forget of an unknown name = %v, want nil", err)
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

	_, name := create(t, c)
	c.Advance(time.Minute)

	for i := 1; i <= 2; i++ {
		if err := n.Forget(ctx, name); err == nil {
			t.Fatalf("Forget #%d succeeded, want a refusal", i)
		}
	}
	if err := n.Forget(ctx, name); err != nil {
		t.Fatalf("Forget after the refusals: %v", err)
	}
	if exists, err := n.NodeExists(ctx, name); err != nil || exists {
		t.Fatalf("the node survived a successful Forget: %v, %v", exists, err)
	}
}

func TestNetworkCancelledContexts(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	c, n := newBoth(t)
	_, name := create(t, c)

	cases := []struct {
		name string
		call func() error
	}{
		{"bad/joined", func() error { _, err := n.Joined(cancelled, name); return err }},
		{"bad/node_exists", func() error { _, err := n.NodeExists(cancelled, name); return err }},
		{"bad/forget", func() error { return n.Forget(cancelled, name) }},
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

	_, name := create(t, c)
	c.Advance(time.Minute)
	if _, err := n.Joined(ctx, name); err != nil {
		t.Fatalf("Joined: %v", err)
	}
	if _, err := n.NodeExists(ctx, name); err != nil {
		t.Fatalf("NodeExists: %v", err)
	}
	if err := n.Forget(ctx, name); err != nil {
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

	_, name := create(t, c)

	// Nothing joins while the cloud's clock says the machine is starting.
	if joined, _ := n.Joined(ctx, name); joined {
		t.Fatal("a node joined without the cloud's clock advancing")
	}
	c.Advance(time.Minute)
	if joined, err := n.Joined(ctx, name); err != nil || !joined {
		t.Fatalf("Joined after advancing the cloud's clock = %v, %v; want true, nil", joined, err)
	}
	if !n.cloud.Now().Equal(c.Now()) {
		t.Fatal("the two halves disagree about the time")
	}
}
