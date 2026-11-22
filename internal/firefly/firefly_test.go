package firefly

import (
	"context"
	"errors"
	"testing"
	"time"
)

// regBase is a fixed instant, so every case reads as an offset from one clock.
var regBase = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Store:        NewMemStore(),
		Label:        "verge=machine",
		MaxLifetime:  4 * time.Hour,
		DrainWindow:  15 * time.Minute,
		BootDeadline: 10 * time.Minute,
		SyncAttempts: 3,
	}
}

// seedMachine puts one machine in the store so leases have something to attach
// to. It goes through the store rather than the document so that the test
// exercises the same validation a restart would.
func seedMachine(t *testing.T, f *Firefly) MachineID {
	t.Helper()
	ctx := context.Background()

	doc, err := f.cfg.Store.Load(ctx)
	if errors.Is(err, ErrNoDocument) {
		doc = Document{Version: DocumentVersion}
	} else if err != nil {
		t.Fatalf("Load: %v", err)
	}

	id := machineID("m1")
	doc.Machines = append(doc.Machines, Machine{
		ID:        id,
		State:     Ready,
		CreatedAt: regBase,
		Deadline:  regBase.Add(f.cfg.MaxLifetime),
		Since:     regBase,
	})
	if err := f.cfg.Store.Save(ctx, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return id
}

func newFirefly(t *testing.T) *Firefly {
	t.Helper()
	f, err := New(testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

func TestNewValidatesConfig(t *testing.T) {
	base := testConfig(t)

	cases := []struct {
		name   string
		mutate func(*Config)
		isErr  bool
	}{
		{"happy/complete", func(*Config) {}, false},
		{"happy/zero_drain_window", func(c *Config) { c.DrainWindow = 0 }, false},
		{"sad/no_store", func(c *Config) { c.Store = nil }, true},
		{"sad/no_label", func(c *Config) { c.Label = "" }, true},
		{"sad/zero_max_lifetime", func(c *Config) { c.MaxLifetime = 0 }, true},
		{"sad/negative_max_lifetime", func(c *Config) { c.MaxLifetime = -time.Hour }, true},
		{"sad/negative_drain_window", func(c *Config) { c.DrainWindow = -time.Minute }, true},
		{"sad/zero_boot_deadline", func(c *Config) { c.BootDeadline = 0 }, true},
		{"sad/zero_sync_attempts", func(c *Config) { c.SyncAttempts = 0 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			_, err := New(cfg)
			if tc.isErr {
				if !errors.Is(err, ErrConfig) {
					t.Fatalf("New = %v, want ErrConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New = %v, want nil", err)
			}
		})
	}
}

func TestAcquire(t *testing.T) {
	ctx := context.Background()

	t.Run("happy/acquire_returns_a_lease", func(t *testing.T) {
		f := newFirefly(t)
		m := seedMachine(t, f)

		id, err := f.Acquire(ctx, m, "session:01J8", Session, time.Hour, regBase)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if id == "" {
			t.Fatal("Acquire returned an empty id")
		}

		snap, err := f.Snapshot(ctx, regBase)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if len(snap.Machines) != 1 || snap.Machines[0].Live != 1 {
			t.Fatalf("Snapshot = %+v, want one machine with one live lease", snap)
		}
	})

	t.Run("happy/ids_do_not_repeat", func(t *testing.T) {
		f := newFirefly(t)
		m := seedMachine(t, f)

		seen := map[LeaseID]bool{}
		for i := 0; i < 5; i++ {
			id, err := f.Acquire(ctx, m, "turn:01J8/"+string(rune('a'+i)), Turn, time.Minute, regBase)
			if err != nil {
				t.Fatalf("Acquire %d: %v", i, err)
			}
			if seen[id] {
				t.Fatalf("lease id %q was minted twice", id)
			}
			seen[id] = true
		}
	})

	t.Run("happy/ids_survive_a_restart", func(t *testing.T) {
		cfg := testConfig(t)
		first, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		m := seedMachine(t, first)

		id, err := first.Acquire(ctx, m, "session:01J8", Session, time.Hour, regBase)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		// A second firefly over the same store must not reissue the id.
		second, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		again, err := second.Acquire(ctx, m, "session:01J8", Session, time.Hour, regBase)
		if err != nil {
			t.Fatalf("Acquire after restart: %v", err)
		}
		if again == id {
			t.Fatalf("the id %q was reissued after a restart", id)
		}
	})

	t.Run("sad/unknown_machine", func(t *testing.T) {
		f := newFirefly(t)
		seedMachine(t, f)

		_, err := f.Acquire(ctx, machineID("nope"), "session:01J8", Session, time.Hour, regBase)
		if !errors.Is(err, ErrUnknownMachine) {
			t.Fatalf("Acquire on an unknown machine = %v, want ErrUnknownMachine", err)
		}
	})

	t.Run("bad/empty_holder", func(t *testing.T) {
		f := newFirefly(t)
		m := seedMachine(t, f)
		if _, err := f.Acquire(ctx, m, "", Session, time.Hour, regBase); err == nil {
			t.Fatal("Acquire accepted an empty holder")
		}
	})

	t.Run("bad/unknown_kind", func(t *testing.T) {
		f := newFirefly(t)
		m := seedMachine(t, f)
		if _, err := f.Acquire(ctx, m, "session:01J8", Kind("haunting"), time.Hour, regBase); err == nil {
			t.Fatal("Acquire accepted an unknown kind")
		}
	})

	t.Run("bad/zero_ttl", func(t *testing.T) {
		f := newFirefly(t)
		m := seedMachine(t, f)
		if _, err := f.Acquire(ctx, m, "session:01J8", Session, 0, regBase); !errors.Is(err, ErrLeaseTTL) {
			t.Fatalf("Acquire with zero ttl = %v, want ErrLeaseTTL", err)
		}
	})

	t.Run("bad/a_rejected_acquire_writes_nothing", func(t *testing.T) {
		f := newFirefly(t)
		seedMachine(t, f)
		saves := f.cfg.Store.(*MemStore).Saves()

		if _, err := f.Acquire(ctx, machineID("nope"), "w", Session, time.Hour, regBase); err == nil {
			t.Fatal("Acquire succeeded on an unknown machine")
		}
		if got := f.cfg.Store.(*MemStore).Saves(); got != saves {
			t.Fatalf("a rejected acquire wrote to the store: %d saves, want %d", got, saves)
		}
	})
}

func TestRenewAndRelease(t *testing.T) {
	ctx := context.Background()

	t.Run("happy/renew_extends_from_now", func(t *testing.T) {
		f := newFirefly(t)
		m := seedMachine(t, f)

		id, err := f.Acquire(ctx, m, "turn:01J8", Turn, time.Minute, regBase)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if err := f.Renew(ctx, id, time.Minute, regBase.Add(30*time.Second)); err != nil {
			t.Fatalf("Renew: %v", err)
		}

		// Still live a full minute after the original expiry would have passed.
		snap, err := f.Snapshot(ctx, regBase.Add(80*time.Second))
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if snap.Machines[0].Live != 1 {
			t.Fatalf("after renewing, Live = %d, want 1", snap.Machines[0].Live)
		}
	})

	t.Run("sad/renewing_a_lapsed_lease_is_refused", func(t *testing.T) {
		f := newFirefly(t)
		m := seedMachine(t, f)

		id, err := f.Acquire(ctx, m, "turn:01J8", Turn, time.Minute, regBase)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if err := f.Renew(ctx, id, time.Minute, regBase.Add(2*time.Minute)); !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("Renew on a lapsed lease = %v, want ErrLeaseExpired", err)
		}
	})

	t.Run("sad/renewing_an_unknown_lease", func(t *testing.T) {
		f := newFirefly(t)
		seedMachine(t, f)
		if err := f.Renew(ctx, "lease-999", time.Minute, regBase); !errors.Is(err, ErrNoSuchLease) {
			t.Fatalf("Renew on an unknown lease = %v, want ErrNoSuchLease", err)
		}
	})

	t.Run("happy/release_drops_the_lease", func(t *testing.T) {
		f := newFirefly(t)
		m := seedMachine(t, f)

		id, err := f.Acquire(ctx, m, "session:01J8", Session, time.Hour, regBase)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if err := f.Release(ctx, id); err != nil {
			t.Fatalf("Release: %v", err)
		}
		snap, err := f.Snapshot(ctx, regBase)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if snap.Machines[0].Live != 0 {
			t.Fatalf("after release, Live = %d, want 0", snap.Machines[0].Live)
		}
	})

	t.Run("happy/release_is_idempotent", func(t *testing.T) {
		f := newFirefly(t)
		m := seedMachine(t, f)

		id, err := f.Acquire(ctx, m, "session:01J8", Session, time.Hour, regBase)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := f.Release(ctx, id); err != nil {
				t.Fatalf("Release #%d = %v, want nil", i+1, err)
			}
		}
		if err := f.Release(ctx, "lease-999"); err != nil {
			t.Fatalf("releasing an unknown id = %v, want nil", err)
		}
	})
}

func TestSnapshotCountsOnlyLiveLeases(t *testing.T) {
	ctx := context.Background()
	f := newFirefly(t)
	m := seedMachine(t, f)

	if _, err := f.Acquire(ctx, m, "turn:01J8", Turn, time.Minute, regBase); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := f.Acquire(ctx, m, "session:01J8", Session, time.Hour, regBase); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	cases := []struct {
		name string
		now  time.Time
		want int
	}{
		{"happy/both_live", regBase, 2},
		{"happy/turn_lapsed", regBase.Add(2 * time.Minute), 1},
		{"sad/none_live", regBase.Add(2 * time.Hour), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := f.Snapshot(ctx, tc.now)
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if snap.Machines[0].Live != tc.want {
				t.Fatalf("Live = %d, want %d", snap.Machines[0].Live, tc.want)
			}
		})
	}
}

// TestFailedSaveLeavesMemoryBehind is the reason mutate copies before committing:
// a store that rejects a write must not leave the caller's view ahead of what was
// persisted, because the next tick would reconcile against a document that does
// not exist.
func TestFailedSaveLeavesMemoryBehind(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.Store = &rejectingStore{inner: NewMemStore()}
	f, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	doc := Document{
		Version: DocumentVersion,
		Machines: []Machine{{
			ID: machineID("m1"), State: Ready, CreatedAt: regBase,
			Deadline: regBase.Add(time.Hour), Since: regBase,
		}},
	}
	if err := cfg.Store.Save(ctx, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The store starts rejecting writes.
	cfg.Store.(*rejectingStore).fail = true

	if _, err := f.Acquire(ctx, machineID("m1"), "session:01J8", Session, time.Hour, regBase); err == nil {
		t.Fatal("Acquire succeeded while the store was rejecting writes")
	}

	snap, err := f.Snapshot(ctx, regBase)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Machines[0].Live != 0 {
		t.Fatalf("a failed save left %d live leases in memory, want 0", snap.Machines[0].Live)
	}
}

// rejectingStore fails every save once fail is set, and passes loads through.
type rejectingStore struct {
	inner *MemStore
	fail  bool
}

func (s *rejectingStore) Load(ctx context.Context) (Document, error) {
	return s.inner.Load(ctx)
}

func (s *rejectingStore) Save(ctx context.Context, doc Document) error {
	if s.fail {
		return errors.New("store: write rejected")
	}
	return s.inner.Save(ctx, doc)
}
