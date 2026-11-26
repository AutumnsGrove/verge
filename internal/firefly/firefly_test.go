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
		Store:             NewMemStore(),
		Cloud:             stubCloud{},
		Network:           stubNetwork{},
		Syncer:            stubSyncer{},
		Label:             "verge=machine",
		MaxLifetime:       4 * time.Hour,
		DrainWindow:       15 * time.Minute,
		BootDeadline:      10 * time.Minute,
		SyncAttempts:      3,
		TerminateDeadline: 5 * time.Minute,
		RetryBase:         30 * time.Second,
		RetryMax:          5 * time.Minute,
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
		Name:      "name-" + string(id),
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
		{"sad/label_without_a_value", func(c *Config) { c.Label = "verge=" }, true},
		{"sad/label_without_a_key", func(c *Config) { c.Label = "=machine" }, true},
		{"sad/label_without_an_equals", func(c *Config) { c.Label = "verge" }, true},
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
			ID: machineID("m1"), Name: "name-m1", State: Ready, CreatedAt: regBase,
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

// TestConfigIsReported covers the accessor, which exists so a caller can log the
// policy in force rather than guess it.
func TestConfigIsReported(t *testing.T) {
	f := newFirefly(t)
	if got := f.Config().Label; got != "verge=machine" {
		t.Fatalf("Config().Label = %q", got)
	}
}

// TestFreshStore is the state every deployment starts in, and no other test
// reaches it: they all seed a machine first.
func TestFreshStore(t *testing.T) {
	ctx := context.Background()
	f := newFirefly(t)

	t.Run("happy/snapshot_of_nothing", func(t *testing.T) {
		snap, err := f.Snapshot(ctx, regBase)
		if err != nil {
			t.Fatalf("Snapshot on a fresh store: %v", err)
		}
		if len(snap.Machines) != 0 {
			t.Fatalf("Snapshot returned %d machines, want 0", len(snap.Machines))
		}
	})

	t.Run("happy/explicit_load_succeeds", func(t *testing.T) {
		// The public Load advertises itself as the way to fail early, so it has to
		// work before anything else has touched the store.
		other := newFirefly(t)
		if err := other.Load(ctx); err != nil {
			t.Fatalf("Load on a fresh store: %v", err)
		}
	})

	t.Run("sad/acquire_on_an_empty_document", func(t *testing.T) {
		other := newFirefly(t)
		_, err := other.Acquire(ctx, machineID("m1"), "session:01J8", Session, time.Hour, regBase)
		if !errors.Is(err, ErrUnknownMachine) {
			t.Fatalf("Acquire on a fresh store = %v, want ErrUnknownMachine", err)
		}
	})

	t.Run("happy/release_on_an_empty_document_is_idempotent", func(t *testing.T) {
		other := newFirefly(t)
		if err := other.Release(ctx, "lease-1"); err != nil {
			t.Fatalf("Release on a fresh store = %v, want nil", err)
		}
	})
}

// TestStoreErrorsPropagate is what justifies Snapshot and mutate returning errors
// at all. Without these cases the propagation could be deleted and the suite would
// stay green, which would make the signature change pointless.
func TestStoreErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("object storage is unreachable")

	broken := func(t *testing.T) (*Firefly, MachineID) {
		t.Helper()
		cfg := testConfig(t)
		cfg.Store = &brokenStore{inner: NewMemStore(), loadErr: boom}
		f, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return f, machineID("m1")
	}

	cases := []struct {
		name string
		call func(f *Firefly, m MachineID) error
	}{
		{"sad/load", func(f *Firefly, m MachineID) error { return f.Load(ctx) }},
		{"sad/snapshot", func(f *Firefly, m MachineID) error { _, err := f.Snapshot(ctx, regBase); return err }},
		{"sad/acquire", func(f *Firefly, m MachineID) error {
			_, err := f.Acquire(ctx, m, "session:01J8", Session, time.Hour, regBase)
			return err
		}},
		{"sad/renew", func(f *Firefly, m MachineID) error { return f.Renew(ctx, "lease-1", time.Hour, regBase) }},
		{"sad/release", func(f *Firefly, m MachineID) error { return f.Release(ctx, "lease-1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, m := broken(t)
			if err := tc.call(f, m); !errors.Is(err, boom) {
				t.Fatalf("%s = %v, want the store's error", tc.name, err)
			}
		})
	}
}

// brokenStore fails loads when loadErr is set.
type brokenStore struct {
	inner   *MemStore
	loadErr error
}

func (s *brokenStore) Load(ctx context.Context) (Document, error) {
	if s.loadErr != nil {
		return Document{}, s.loadErr
	}
	return s.inner.Load(ctx)
}

func (s *brokenStore) Save(ctx context.Context, doc Document) error {
	return s.inner.Save(ctx, doc)
}

// TestRenewTouchesOnlyTheNamedLease covers the search loop's continue. Renew
// rewrites Start and TTL, so renewing the wrong lease in a list would silently
// extend a session nobody asked about.
func TestRenewTouchesOnlyTheNamedLease(t *testing.T) {
	ctx := context.Background()
	f := newFirefly(t)
	m := seedMachine(t, f)

	// Distinct TTLs, so renewing the wrong one is visible in the expiry.
	first, err := f.Acquire(ctx, m, "session:a", Session, 10*time.Minute, regBase)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	second, err := f.Acquire(ctx, m, "session:b", Session, time.Minute, regBase)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if err := f.Renew(ctx, second, 10*time.Minute, regBase.Add(30*time.Second)); err != nil {
		t.Fatalf("Renew: %v", err)
	}

	doc, err := f.cfg.Store.(*MemStore).Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := map[LeaseID]time.Time{
		first:  regBase.Add(10 * time.Minute),
		second: regBase.Add(30*time.Second + 10*time.Minute),
	}
	for _, l := range doc.Leases {
		if got := l.ExpiresAt(); !got.Equal(want[l.ID]) {
			t.Errorf("lease %s expires at %s, want %s", l.ID, got, want[l.ID])
		}
	}
	if len(doc.Leases) != 2 {
		t.Fatalf("the document has %d leases, want 2", len(doc.Leases))
	}
}

func TestRenewRejectsBadTTL(t *testing.T) {
	ctx := context.Background()
	f := newFirefly(t)
	m := seedMachine(t, f)

	id, err := f.Acquire(ctx, m, "session:a", Session, time.Hour, regBase)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	t.Run("bad/zero", func(t *testing.T) {
		if err := f.Renew(ctx, id, 0, regBase); !errors.Is(err, ErrLeaseTTL) {
			t.Fatalf("Renew with zero ttl = %v, want ErrLeaseTTL", err)
		}
	})
	t.Run("bad/negative", func(t *testing.T) {
		if err := f.Renew(ctx, id, -time.Minute, regBase); !errors.Is(err, ErrLeaseTTL) {
			t.Fatalf("Renew with negative ttl = %v, want ErrLeaseTTL", err)
		}
	})
}

// TestReleaseKeepsOtherLeases covers the branch that keeps a lease belonging to
// another id. A Release that emptied the slice unconditionally would drop every
// live lease on the machine and still pass a single-lease suite.
func TestReleaseKeepsOtherLeases(t *testing.T) {
	ctx := context.Background()
	f := newFirefly(t)
	m := seedMachine(t, f)

	keepID, err := f.Acquire(ctx, m, "session:keep", Session, time.Hour, regBase)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	dropID, err := f.Acquire(ctx, m, "turn:drop", Turn, time.Hour, regBase)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	saves := f.cfg.Store.(*MemStore).Saves()
	if err := f.Release(ctx, dropID); err != nil {
		t.Fatalf("Release: %v", err)
	}

	doc, err := f.cfg.Store.(*MemStore).Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(doc.Leases) != 1 {
		t.Fatalf("after releasing one of two, the document has %d leases, want 1", len(doc.Leases))
	}
	if doc.Leases[0].ID != keepID {
		t.Fatalf("the surviving lease is %s, want %s", doc.Leases[0].ID, keepID)
	}

	snap, err := f.Snapshot(ctx, regBase)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Machines[0].Live != 1 {
		t.Fatalf("Live = %d, want 1", snap.Machines[0].Live)
	}

	if got := f.cfg.Store.(*MemStore).Saves(); got != saves+1 {
		t.Fatalf("Release wrote %d times, want exactly 1", got-saves)
	}
}

// TestSaveRejectsAnInvalidDocument covers the validation branch of save, which is
// the second line of defence: mutate builds documents through the API, so only a
// programming error or a corrupt load reaches it.
func TestSaveRejectsAnInvalidDocument(t *testing.T) {
	ctx := context.Background()
	f := newFirefly(t)

	err := f.save(ctx, Document{Version: DocumentVersion + 1})
	if !errors.Is(err, ErrDocumentVersion) {
		t.Fatalf("save of a future document = %v, want ErrDocumentVersion", err)
	}
}

// TestSnapshotReportsTheDeadline is the placement question: Live still counts
// leases that Tick has not acted on, so a caller needs to see the cap to avoid
// placing a session on a machine that is about to be destroyed.
func TestSnapshotReportsTheDeadline(t *testing.T) {
	ctx := context.Background()
	f := newFirefly(t)
	seedMachine(t, f)

	snap, err := f.Snapshot(ctx, regBase)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Machines) != 1 {
		t.Fatalf("Snapshot returned %d machines", len(snap.Machines))
	}
	if got, want := snap.Machines[0].Deadline, regBase.Add(f.cfg.MaxLifetime); !got.Equal(want) {
		t.Fatalf("Deadline = %s, want %s", got, want)
	}
	// The name is in the snapshot because it is what an operator types into a console
	// or a tailnet to find the machine the id belongs to.
	if got, want := snap.Machines[0].Name, "name-m1"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
}

// --- stubs ---------------------------------------------------------------
//
// The registry tests do not exercise the provider, the mesh, or the syncer, so
// they use the smallest thing that satisfies each interface. Tick's tests live in
// the external test package and use the shared fake instead, because a second
// fake would be the divergent-fake problem testing.md warns about.

type stubCloud struct{}

func (stubCloud) Create(context.Context, Spec) (MachineID, error) { return "", nil }
func (stubCloud) Describe(context.Context, MachineID) (CloudMachine, error) {
	return CloudMachine{}, nil
}
func (stubCloud) List(context.Context, string) ([]CloudMachine, error) { return nil, nil }
func (stubCloud) Delete(context.Context, MachineID) error              { return nil }

type stubNetwork struct{}

func (stubNetwork) Joined(context.Context, string) (bool, error)     { return false, nil }
func (stubNetwork) NodeExists(context.Context, string) (bool, error) { return false, nil }
func (stubNetwork) Forget(context.Context, string) error             { return nil }

type stubSyncer struct{}

func (stubSyncer) Sync(context.Context, MachineID, string) error { return nil }

// TestRetryDelayIsCapped covers the curve and its ceiling directly: the cap is the
// part that matters, because it is what stops a retry from being scheduled after
// the machine's own deadline, where nothing could act on it.
func TestRetryDelayIsCapped(t *testing.T) {
	f := newFirefly(t) // RetryBase 30s, RetryMax 5m

	cases := []struct {
		name    string
		attempt int
		want    time.Duration
	}{
		{"happy/first_failure", 1, 30 * time.Second},
		{"happy/second_failure", 2, time.Minute},
		{"happy/below_the_ceiling", 5, 150 * time.Second},
		{"sad/at_the_ceiling", 10, 5 * time.Minute},
		{"sad/well_past_the_ceiling", 1000, 5 * time.Minute},
		{"bad/zero_attempt_is_treated_as_one", 0, 30 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := f.retryDelay(c.attempt); got != c.want {
				t.Fatalf("retryDelay(%d) = %s, want %s", c.attempt, got, c.want)
			}
		})
	}
}

// TestRenewAfterPruningStillSaysExpired is the contract pruning nearly broke.
//
// Tick removes a lapsed lease, so Renew no longer finds the record and falls
// through to the not-found path. Without a distinction there, a supervisor that
// treats ErrNoSuchLease as a bug would alert on the ordinary lapse path.
func TestRenewAfterPruningStillSaysExpired(t *testing.T) {
	ctx := context.Background()

	t.Run("sad/previously_minted_is_expired", func(t *testing.T) {
		// NextLeaseNumber is 5, so lease-3 existed and is gone.
		cfg := testConfig(t)
		doc := Document{Version: DocumentVersion, NextLeaseNumber: 5}
		if err := cfg.Store.Save(ctx, doc); err != nil {
			t.Fatalf("Save: %v", err)
		}
		g, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := g.Renew(ctx, "lease-3", time.Hour, regBase); !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("Renew(lease-3) = %v, want ErrLeaseExpired", err)
		}
	})

	t.Run("bad/never_minted_is_no_such_lease", func(t *testing.T) {
		cfg := testConfig(t)
		doc := Document{Version: DocumentVersion, NextLeaseNumber: 5}
		if err := cfg.Store.Save(ctx, doc); err != nil {
			t.Fatalf("Save: %v", err)
		}
		g, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := g.Renew(ctx, "lease-99", time.Hour, regBase); !errors.Is(err, ErrNoSuchLease) {
			t.Fatalf("Renew(lease-99) = %v, want ErrNoSuchLease", err)
		}
	})

	t.Run("bad/not_even_our_format", func(t *testing.T) {
		cfg := testConfig(t)
		doc := Document{Version: DocumentVersion, NextLeaseNumber: 5}
		if err := cfg.Store.Save(ctx, doc); err != nil {
			t.Fatalf("Save: %v", err)
		}
		g, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := g.Renew(ctx, "not-a-lease", time.Hour, regBase); !errors.Is(err, ErrNoSuchLease) {
			t.Fatalf("Renew(garbage) = %v, want ErrNoSuchLease", err)
		}
	})
}

// TestRenewAfterPruningThroughATick is the same contract, reached the way a
// supervisor would reach it: acquire, let a tick prune the lapsed record, renew.
func TestRenewAfterPruningThroughATick(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	f, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := seedMachine(t, f)

	id, err := f.Acquire(ctx, m, "turn:01J8", Turn, time.Minute, regBase)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := f.Tick(ctx, regBase.Add(2*time.Minute)); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	doc, err := cfg.Store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(doc.Leases) != 0 {
		t.Fatalf("the lapsed lease was not pruned: %d leases", len(doc.Leases))
	}

	if err := f.Renew(ctx, id, time.Hour, regBase.Add(3*time.Minute)); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("Renew after pruning = %v, want ErrLeaseExpired", err)
	}
}

// lyingStore returns a document the caller chose, whatever was saved. It models a
// store that hands back bytes this binary should not act on: a document from a
// future version, or one that describes a world that could not exist.
type lyingStore struct {
	doc Document
}

func (s *lyingStore) Load(context.Context) (Document, error) { return s.doc, nil }
func (s *lyingStore) Save(context.Context, Document) error   { return nil }

// TestLoadRefusesADocumentThatDoesNotValidate.
//
// Validation on save is not enough on its own. A store is the one place untrusted
// bytes arrive from — a stale object, a half-written one, a document from a
// different build — and a tick that acted on one would walk machines through the
// transition table, including the rows that delete them, and only fail when it tried
// to write the result back.
func TestLoadRefusesADocumentThatDoesNotValidate(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		doc     Document
		wantErr error // when set, the refusal must wrap this
	}{
		{
			// The version bump in this change is only a compatibility gate if the load
			// path enforces it.
			name:    "bad/a_document_from_a_future_build",
			doc:     Document{Version: DocumentVersion + 1},
			wantErr: ErrDocumentVersion,
		},
		{
			name: "bad/a_machine_with_no_name",
			doc: Document{
				Version: DocumentVersion,
				Machines: []Machine{{
					ID: machineID("m1"), State: Ready, CreatedAt: regBase,
					Deadline: regBase.Add(time.Hour), Since: regBase,
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Store = &lyingStore{doc: tc.doc}
			f, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			loadErr := f.Load(ctx)
			if loadErr == nil {
				t.Fatal("Load accepted a document that does not validate")
			}
			if tc.wantErr != nil && !errors.Is(loadErr, tc.wantErr) {
				t.Fatalf("Load = %v, want %v", loadErr, tc.wantErr)
			}

			// The refusal has to be sticky: a load that failed must not leave a
			// half-loaded document behind for the next call to act on.
			if _, err := f.Snapshot(ctx, regBase); err == nil {
				t.Fatal("Snapshot used a document that failed to load")
			}
		})
	}
}
