package placement_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/cloud/fake"
	"github.com/AutumnsGrove/verge/internal/firefly"
	"github.com/AutumnsGrove/verge/internal/placement"
)

var base = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// world is one firefly over the shared fakes, with a placement above it.
type world struct {
	cloud *fake.Cloud
	net   *fake.Network
	sync  *fake.Syncer
	store *firefly.MemStore
	f     *firefly.Firefly
	p     *placement.Placement

	specs []string // names NewSpec was asked for, in order
}

func newWorld(t *testing.T, opts ...func(*placement.Config)) *world {
	t.Helper()

	w := &world{
		cloud: fake.New(base).Boot(30 * time.Second),
		store: firefly.NewMemStore(),
		sync:  fake.NewSyncer(),
	}
	w.net = fake.NewNetwork(w.cloud)

	f, err := firefly.New(firefly.Config{
		Store:             w.store,
		Cloud:             w.cloud,
		Network:           w.net,
		Syncer:            w.sync,
		Label:             "verge=machine",
		MaxLifetime:       4 * time.Hour,
		DrainWindow:       15 * time.Minute,
		BootDeadline:      10 * time.Minute,
		SyncAttempts:      3,
		TerminateDeadline: 5 * time.Minute,
		RetryBase:         30 * time.Second,
		RetryMax:          5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("firefly.New: %v", err)
	}
	w.f = f

	cfg := placement.Config{
		Firefly: f,
		NewSpec: func(name string) (firefly.Spec, error) {
			w.specs = append(w.specs, name)
			return firefly.Spec{
				CloudInit: []byte("#cloud-config\n# hostname: " + name + "\n"),
				Image:     "ubuntu-24.04",
				Region:    "fsn1",
				Size:      "cpx11",
			}, nil
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	p, err := placement.New(cfg)
	if err != nil {
		t.Fatalf("placement.New: %v", err)
	}
	w.p = p
	return w
}

// tick walks the loop once, which is how a provisioned machine becomes READY: the fake
// cloud's boot is a simulated 30 seconds.
func (w *world) tick(t *testing.T, now time.Time) firefly.Result {
	t.Helper()
	if d := now.Sub(w.cloud.Now()); d > 0 {
		w.cloud.Advance(d)
	}
	res, err := w.f.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("Tick at %s: %v", now.Sub(base), err)
	}
	return res
}

// bootAndReady provisions a machine, drives it to READY, and takes one lease on it --
// which is what a caller does: the first Place answers "pending", the caller retries, and
// the retry is what leases the machine it waited for. A machine nobody leases drains,
// which is firefly behaving correctly rather than a test inconvenience.
func (w *world) bootAndReady(t *testing.T, now time.Time, holder string) firefly.MachineID {
	t.Helper()

	id, ok, err := w.p.Place(context.Background(), placement.Request{
		Holder: holder, Kind: firefly.Session, TTL: 2 * time.Hour,
	}, now)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if ok {
		t.Fatalf("Place returned a ready machine when none existed")
	}

	// Tick until the machine answers, and lease it at that instant: a READY machine with
	// no lease starts draining on the next tick, so the retry belongs at the moment it
	// became ready rather than after a fixed wait.
	for i := 1; i <= 10; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		for _, ev := range w.tick(t, at).Transitions {
			if ev.Machine != id || ev.To != firefly.Ready {
				continue
			}
			got, ok, err := w.p.Place(context.Background(), placement.Request{
				Holder: holder, Kind: firefly.Session, TTL: 2 * time.Hour,
			}, at)
			if err != nil || !ok || got != id {
				t.Fatalf("the retry = (%s, %v, %v), want the machine it waited for (%s)", got, ok, err, id)
			}
			return id
		}
	}
	t.Fatalf("machine %s never became READY", id)
	return ""
}

func TestPlace(t *testing.T) {
	t.Run("happy/provisions_when_nothing_exists", func(t *testing.T) {
		w := newWorld(t)
		id, ok, err := w.p.Place(context.Background(), placement.Request{
			Holder: "session:one", Kind: firefly.Session, TTL: time.Hour,
		}, base)
		if err != nil {
			t.Fatalf("Place: %v", err)
		}
		if ok {
			t.Fatal("Place reported a ready machine before one was provisioned")
		}
		if id == "" {
			t.Fatal("Place returned no machine id for the machine it provisioned")
		}
		if len(w.specs) != 1 {
			t.Fatalf("asked for %d specs, want 1", len(w.specs))
		}
	})

	t.Run("happy/the_boot_script_is_built_from_the_name_provision_will_use", func(t *testing.T) {
		// The name is what the network is asked about, and the script sets the hostname:
		// two names is a machine nothing can find.
		w := newWorld(t)
		id, _, err := w.p.Place(context.Background(), placement.Request{
			Holder: "session:one", Kind: firefly.Session, TTL: time.Hour,
		}, base)
		if err != nil {
			t.Fatalf("Place: %v", err)
		}
		if len(w.specs) != 1 {
			t.Fatalf("asked for %d specs, want 1", len(w.specs))
		}
		name, okName := w.cloud.Name(id)
		if !okName {
			t.Fatalf("the fake has no name for %s", id)
		}
		if w.specs[0] != name {
			t.Fatalf("the spec was built for %q and the provider was told %q", w.specs[0], name)
		}
	})

	t.Run("happy/reuses_a_ready_machine_with_room", func(t *testing.T) {
		w := newWorld(t)
		id := w.bootAndReady(t, base, "session:boot")
		now := base.Add(11 * time.Minute)

		got, ok, err := w.p.Place(context.Background(), placement.Request{
			Holder: "session:two", Kind: firefly.Session, TTL: time.Hour,
		}, now)
		if err != nil {
			t.Fatalf("Place: %v", err)
		}
		if !ok || got != id {
			t.Fatalf("Place = (%s, %v), want the ready machine %s", got, ok, id)
		}
		if len(w.specs) != 1 {
			t.Fatalf("provisioned again (%d specs) when a machine had room", len(w.specs))
		}
	})

	t.Run("happy/fills_a_machine_then_provisions_another", func(t *testing.T) {
		w := newWorld(t)
		w.bootAndReady(t, base, "session:boot")

		now := base.Add(11 * time.Minute)
		// One lease is already held by the boot helper, so capacity-1 more fill it.
		for i := 0; i < w.p.Capacity()-1; i++ {
			if _, ok, err := w.p.Place(context.Background(), placement.Request{
				Holder: "session:" + string(rune('a'+i)), Kind: firefly.Session, TTL: time.Hour,
			}, now); err != nil || !ok {
				t.Fatalf("Place %d = (_, %v, %v), want the ready machine", i, ok, err)
			}
		}

		// One more than capacity: a second machine, and the caller is told to retry.
		got, ok, err := w.p.Place(context.Background(), placement.Request{
			Holder: "session:full", Kind: firefly.Session, TTL: time.Hour,
		}, now)
		if err != nil {
			t.Fatalf("Place: %v", err)
		}
		if ok {
			t.Fatal("Place reused a machine that was at capacity")
		}
		if got == "" || len(w.specs) != 2 {
			t.Fatalf("Place = (%q, %v) with %d specs, want a second machine", got, ok, len(w.specs))
		}
	})

	t.Run("sad/every_machine_full_and_at_the_cap", func(t *testing.T) {
		w := newWorld(t, func(c *placement.Config) { c.Capacity = 1; c.MaxMachines = 1 })
		w.bootAndReady(t, base, "session:boot")

		_, _, err := w.p.Place(context.Background(), placement.Request{
			Holder: "session:two", Kind: firefly.Session, TTL: time.Hour,
		}, base.Add(11*time.Minute))
		if !errors.Is(err, placement.ErrAtCapacity) {
			t.Fatalf("Place = %v, want %v", err, placement.ErrAtCapacity)
		}
	})

	t.Run("sad/a_machine_at_its_deadline_is_not_placed_on", func(t *testing.T) {
		// A machine past its cap is about to be destroyed; placing work there loses a turn
		// to a race the snapshot could have seen.
		w := newWorld(t, func(c *placement.Config) { c.MaxMachines = 1 })
		w.bootAndReady(t, base, "session:boot")

		// Past MaxLifetime (4h), which is the machine's deadline.
		now := base.Add(4*time.Hour + time.Minute)
		_, _, err := w.p.Place(context.Background(), placement.Request{
			Holder: "session:late", Kind: firefly.Session, TTL: time.Hour,
		}, now)
		if !errors.Is(err, placement.ErrAtCapacity) {
			t.Fatalf("Place = %v, want %v: the machine is past its deadline", err, placement.ErrAtCapacity)
		}
	})

	t.Run("sad/the_create_backs_off", func(t *testing.T) {
		// A failed create is not a failed placement: the next attempt is not due yet, so
		// the caller is told to retry rather than given an error it cannot act on.
		w := newWorld(t)
		w.cloud.FailCreate(errors.New("quota exceeded"))

		if _, _, err := w.p.Place(context.Background(), placement.Request{
			Holder: "session:one", Kind: firefly.Session, TTL: time.Hour,
		}, base); err == nil {
			t.Fatal("the first create failure was hidden")
		}

		// Second attempt, inside the backoff window: pending, not an error.
		_, ok, err := w.p.Place(context.Background(), placement.Request{
			Holder: "session:one", Kind: firefly.Session, TTL: time.Hour,
		}, base.Add(time.Second))
		if err != nil {
			t.Fatalf("Place during backoff = %v, want a pending answer", err)
		}
		if ok {
			t.Fatal("Place reported a ready machine during backoff")
		}
	})

	t.Run("bad/requests_that_cannot_be_honoured", func(t *testing.T) {
		cases := map[string]placement.Request{
			"no holder":  {Kind: firefly.Session, TTL: time.Hour},
			"no kind":    {Holder: "session:one", TTL: time.Hour},
			"zero ttl":   {Holder: "session:one", Kind: firefly.Session},
			"bad kind":   {Holder: "session:one", Kind: firefly.Kind("nonsense"), TTL: time.Hour},
			"negative t": {Holder: "session:one", Kind: firefly.Session, TTL: -time.Minute},
		}
		for name, req := range cases {
			t.Run(name, func(t *testing.T) {
				w := newWorld(t)
				if _, _, err := w.p.Place(context.Background(), req, base); err == nil {
					t.Fatalf("Place(%+v) = nil, want a refusal", req)
				}
				if len(w.specs) != 0 {
					t.Fatalf("a refused request provisioned %d machines", len(w.specs))
				}
			})
		}
	})
}

func TestNew(t *testing.T) {
	t.Run("bad/no_firefly", func(t *testing.T) {
		if _, err := placement.New(placement.Config{NewSpec: func(string) (firefly.Spec, error) { return firefly.Spec{}, nil }}); !errors.Is(err, placement.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, placement.ErrConfig)
		}
	})

	t.Run("bad/no_spec_builder", func(t *testing.T) {
		if _, err := placement.New(placement.Config{Firefly: &firefly.Firefly{}}); !errors.Is(err, placement.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, placement.ErrConfig)
		}
	})

	t.Run("bad/negative_policy", func(t *testing.T) {
		f := &firefly.Firefly{}
		for _, cfg := range []placement.Config{
			{Firefly: f, NewSpec: func(string) (firefly.Spec, error) { return firefly.Spec{}, nil }, Capacity: -1},
			{Firefly: f, NewSpec: func(string) (firefly.Spec, error) { return firefly.Spec{}, nil }, MaxMachines: -1},
		} {
			if _, err := placement.New(cfg); !errors.Is(err, placement.ErrConfig) {
				t.Fatalf("New(%+v) = %v, want %v", cfg, err, placement.ErrConfig)
			}
		}
	})

	t.Run("happy/defaults", func(t *testing.T) {
		w := newWorld(t)
		if got := w.p.Capacity(); got != placement.DefaultCapacity {
			t.Fatalf("Capacity = %d, want %d", got, placement.DefaultCapacity)
		}
		if got := w.p.MaxMachines(); got != placement.DefaultMaxMachines {
			t.Fatalf("MaxMachines = %d, want %d", got, placement.DefaultMaxMachines)
		}
	})
}
