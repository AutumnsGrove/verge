package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// fakeBase is a fixed instant so every case reads as an offset from one clock.
var fakeBase = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func spec() firefly.Spec {
	return firefly.Spec{
		CloudInit: []byte("#cloud-config\n"),
		Image:     "ubuntu-24.04",
		Region:    "fsn1",
		Size:      "cx22",
		Labels:    map[string]string{"verge": "machine"},
	}
}

func TestCreateAndBoot(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase).Boot(4 * time.Minute)

	t.Run("happy/created_at_the_instant_of_creation", func(t *testing.T) {
		id, err := c.Create(ctx, spec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := c.Describe(ctx, id)
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		if got.Status != firefly.ProviderCreated {
			t.Fatalf("status at creation = %s, want %s", got.Status, firefly.ProviderCreated)
		}
	})

	t.Run("happy/starting_then_running", func(t *testing.T) {
		c.Advance(time.Minute)
		ids, err := c.List(ctx, "verge=machine")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(ids) != 1 || ids[0].Status != firefly.ProviderStarting {
			t.Fatalf("after a minute: %+v, want one starting machine", ids)
		}

		c.Advance(4 * time.Minute)
		ids, err = c.List(ctx, "verge=machine")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(ids) != 1 || ids[0].Status != firefly.ProviderRunning {
			t.Fatalf("after five minutes: %+v, want one running machine", ids)
		}
	})
}

func TestDescribeUnknownMachine(t *testing.T) {
	c := New(fakeBase)
	if _, err := c.Describe(context.Background(), "nope"); !errors.Is(err, firefly.ErrNoSuchMachine) {
		t.Fatalf("Describe(unknown) = %v, want ErrNoSuchMachine", err)
	}
}

// TestListFiltersByLabel keeps the orphan sweep honest: a machine that is not ours
// must never be adopted, because adopting someone else's machine means deleting it
// later.
func TestListFiltersByLabel(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase)

	if _, err := c.Create(ctx, spec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	theirs := spec()
	theirs.Labels = map[string]string{"outpost": "minecraft"}
	if _, err := c.Create(ctx, theirs); err != nil {
		t.Fatalf("Create: %v", err)
	}

	mine, err := c.List(ctx, "verge=machine")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mine) != 1 {
		t.Fatalf("List(verge=machine) returned %d machines, want 1", len(mine))
	}

	all, err := c.List(ctx, "")
	if err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List without a label returned %d machines, want 2", len(all))
	}
}

func TestFaultFailCreate(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("quota exceeded")
	c := New(fakeBase).FailCreate(boom)

	t.Run("sad/create_reports_the_failure", func(t *testing.T) {
		if _, err := c.Create(ctx, spec()); !errors.Is(err, boom) {
			t.Fatalf("Create = %v, want the injected failure", err)
		}
	})

	t.Run("sad/no_machine_exists_afterwards", func(t *testing.T) {
		got, err := c.List(ctx, "verge=machine")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("a failed create left %d machines behind", len(got))
		}
	})
}

// TestFaultStallBoot is the machine that never finishes cloud-init: it exists and
// bills, and no amount of waiting makes it usable. Firefly must never call it
// ready, and must eventually destroy it.
func TestFaultStallBoot(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase).StallBoot()

	id, err := c.Create(ctx, spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.Advance(24 * time.Hour)

	got, err := c.Describe(ctx, id)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got.Status != firefly.ProviderStarting {
		t.Fatalf("a stalled boot reports %s after a day, want %s", got.Status, firefly.ProviderStarting)
	}
}

func TestFaultCreateIsSlow(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase).CreateIsSlow(2 * time.Minute)

	if _, err := c.Create(ctx, spec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := c.Now(); !got.Equal(fakeBase.Add(2 * time.Minute)) {
		t.Fatalf("a slow create advanced the clock to %s, want %s", got, fakeBase.Add(2*time.Minute))
	}

	// The caller giving up is the other half of "slow", and it is asserted with a
	// context that is already done rather than by waiting.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Create(cancelled, spec()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestDeleteLifecycle(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase).Boot(0).DeleteAfter(10 * time.Second)

	t.Run("happy/stopping_then_gone", func(t *testing.T) {
		id, err := c.Create(ctx, spec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		got, err := c.Describe(ctx, id)
		if err != nil {
			t.Fatalf("Describe during delete: %v", err)
		}
		if got.Status != firefly.ProviderStopping {
			t.Fatalf("status during delete = %s, want %s", got.Status, firefly.ProviderStopping)
		}

		c.Advance(10 * time.Second)
		if _, err := c.Describe(ctx, id); !errors.Is(err, firefly.ErrNoSuchMachine) {
			t.Fatalf("Describe after the delete completed = %v, want ErrNoSuchMachine", err)
		}
	})

	t.Run("happy/delete_is_idempotent", func(t *testing.T) {
		id, err := c.Create(ctx, spec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := c.Delete(ctx, id); err != nil {
				t.Fatalf("Delete #%d: %v", i+1, err)
			}
		}
	})
}

func TestFaultDeleteRefuses(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase).Boot(0).DeleteRefuses(2)

	id, err := c.Create(ctx, spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("sad/refuses_twice_then_succeeds", func(t *testing.T) {
		for i := 1; i <= 2; i++ {
			if err := c.Delete(ctx, id); err == nil {
				t.Fatalf("Delete #%d succeeded, want a refusal", i)
			}
		}
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete after the refusals: %v", err)
		}
	})
}

// TestFaultDeleteReturns404 is the case firefly must treat as success: nothing to
// do is not a failure, and treating it as one is how a machine is retried forever.
func TestFaultDeleteReturns404(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase).Boot(0).DeleteReturns404()

	t.Run("sad/deleting_something_that_never_existed_is_success", func(t *testing.T) {
		if err := c.Delete(ctx, "never-existed"); err != nil {
			t.Fatalf("Delete of an unknown machine = %v, want nil", err)
		}
	})

	t.Run("sad/deleting_a_live_machine_reports_success", func(t *testing.T) {
		id, err := c.Create(ctx, spec())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := c.Delete(ctx, id); err != nil {
			t.Fatalf("Delete = %v, want nil", err)
		}
		if _, err := c.Describe(ctx, id); !errors.Is(err, firefly.ErrNoSuchMachine) {
			t.Fatalf("the machine survived a 404 delete: %v", err)
		}
	})
}

// TestFaultListDrops is the one that punishes deriving absence from a listing: the
// machine is live and billing, and the listing does not mention it.
func TestFaultListDrops(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase).Boot(0)

	id, err := c.Create(ctx, spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c.ListDrops(id)

	got, err := c.List(ctx, "verge=machine")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List returned %d machines, want 0 (the id was dropped)", len(got))
	}

	// Describe still finds it. That asymmetry is the whole fault, and it is why
	// only Describe may report absence.
	if _, err := c.Describe(ctx, id); err != nil {
		t.Fatalf("Describe of a dropped-but-live machine: %v", err)
	}
}

func TestCancelledContexts(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	c := New(fakeBase).Boot(0)
	id, err := c.Create(context.Background(), spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cases := []struct {
		name string
		call func() error
	}{
		{"bad/create", func() error { _, err := c.Create(cancelled, spec()); return err }},
		{"bad/describe", func() error { _, err := c.Describe(cancelled, id); return err }},
		{"bad/list", func() error { _, err := c.List(cancelled, ""); return err }},
		{"bad/delete", func() error { return c.Delete(cancelled, id) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s with a cancelled context = %v, want context.Canceled", tc.name, err)
			}
		})
	}
}

// TestCreateCopiesTheSpec stops a test from mutating the fake's record through the
// map it passed in: a shared map is the same aliasing bug the store had.
func TestCreateCopiesTheSpec(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase).Boot(0)

	s := spec()
	if _, err := c.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.Labels["verge"] = "mutated"

	got, err := c.List(ctx, "verge=machine")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("mutating the spec changed the stored machine: %d matches", len(got))
	}
}

func TestCounts(t *testing.T) {
	ctx := context.Background()
	c := New(fakeBase).Boot(0)

	id, err := c.Create(ctx, spec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.Describe(ctx, id); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := c.List(ctx, ""); err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := c.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	creates, describes, lists, deletes := c.Calls()
	if creates != 1 || describes != 1 || lists != 1 || deletes != 1 {
		t.Fatalf("Calls() = %d/%d/%d/%d, want 1/1/1/1", creates, describes, lists, deletes)
	}
}
