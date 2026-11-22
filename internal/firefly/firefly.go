package firefly

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is the policy firefly needs in order to run, supplied once at
// construction.
//
// Everything here is a duration or a limit rather than a behaviour: firefly
// enforces them and never decides them. That split is what lets the whole
// package be tested with a fake clock and a fake cloud, and it is why a Machine
// record stores resolved instants (CreatedAt, Deadline) instead of durations --
// a stored duration read back after a restart would silently disagree with the
// configuration now in force.
type Config struct {
	// Store is where the document lives. Required.
	Store Store

	// Cloud is the provider. Required.
	Cloud Cloud

	// Network is the private mesh. Required.
	Network Network

	// Syncer saves a machine's own state, and is what the ordering property is
	// about. Required.
	Syncer Syncer

	// Label identifies this deployment's machines, as "key=value". It is how the
	// orphan sweep tells our machines from someone else's, so a wrong label is
	// how a design adopts a machine it did not create and then deletes it.
	Label string

	// MaxLifetime is the hard cap. No lease survives it, and it routes through a
	// save rather than around one.
	MaxLifetime time.Duration

	// DrainWindow is how long a machine lingers after its last lease lapses, so
	// that a session between turns does not pay for a teardown and a fresh clone.
	DrainWindow time.Duration

	// BootDeadline is how long a machine may stay in PROVISIONING before it is an
	// error rather than a boot.
	BootDeadline time.Duration

	// SyncAttempts is how many times a save is retried before the cap is allowed
	// to abandon it.
	SyncAttempts int

	// TerminateDeadline is how long a delete may stay unconfirmed before the
	// machine is an error rather than a delete in flight.
	TerminateDeadline time.Duration

	// RetryBase is the backoff for the first consecutive failure, growing
	// linearly to RetryMax.
	RetryBase time.Duration
	RetryMax  time.Duration
}

// ErrConfig means the configuration cannot support a working firefly. It is
// returned by New rather than discovered on the first tick, because a bad drain
// window is a machine that fades immediately and nobody notices until a bill.
var ErrConfig = errors.New("firefly: invalid configuration")

// Firefly is the mechanism. It owns the document, mints leases, and reconciles.
//
// It is safe for concurrent use: the phone-facing surface and the tick loop both
// touch it, and a document edited by two goroutines is the corruption Validate
// exists to catch after the fact rather than before.
type Firefly struct {
	cfg Config

	mu     sync.Mutex
	doc    Document
	loaded bool

	// The failed-create record. It is not in the document because a failed create
	// leaves nothing to record: there is no machine, and the table's row is
	// ABSENT --CreateFailed--> ABSENT. A restart retrying immediately is fine.
	createFailures int
	createRetryAt  time.Time
	createErr      error
}

// New validates the configuration and returns a firefly with no document loaded.
//
// Nothing is read here: the store is touched on the first call that needs it, so
// constructing a firefly cannot fail because object storage is briefly
// unreachable. A caller that wants to fail early can call Load.
func New(cfg Config) (*Firefly, error) {
	switch {
	case cfg.Store == nil:
		return nil, fmt.Errorf("%w: no Store", ErrConfig)
	case cfg.Label == "":
		return nil, fmt.Errorf("%w: no Label", ErrConfig)
	default:
		if _, _, ok := splitLabel(cfg.Label); !ok {
			return nil, fmt.Errorf("%w: Label must be key=value, got %q", ErrConfig, cfg.Label)
		}
	}
	switch {
	case cfg.MaxLifetime <= 0:
		return nil, fmt.Errorf("%w: MaxLifetime must be positive", ErrConfig)
	case cfg.DrainWindow < 0:
		return nil, fmt.Errorf("%w: DrainWindow cannot be negative", ErrConfig)
	case cfg.BootDeadline <= 0:
		return nil, fmt.Errorf("%w: BootDeadline must be positive", ErrConfig)
	case cfg.SyncAttempts < 1:
		return nil, fmt.Errorf("%w: SyncAttempts must be at least 1", ErrConfig)
	case cfg.Cloud == nil:
		return nil, fmt.Errorf("%w: no Cloud", ErrConfig)
	case cfg.Network == nil:
		return nil, fmt.Errorf("%w: no Network", ErrConfig)
	case cfg.Syncer == nil:
		return nil, fmt.Errorf("%w: no Syncer", ErrConfig)
	case cfg.TerminateDeadline <= 0:
		return nil, fmt.Errorf("%w: TerminateDeadline must be positive", ErrConfig)
	case cfg.RetryBase <= 0:
		return nil, fmt.Errorf("%w: RetryBase must be positive", ErrConfig)
	case cfg.RetryMax < cfg.RetryBase:
		return nil, fmt.Errorf("%w: RetryMax must be at least RetryBase", ErrConfig)
	}
	return &Firefly{cfg: cfg}, nil
}

// Config returns the configuration, for reporting.
func (f *Firefly) Config() Config { return f.cfg }

// Load reads the document from the store, if it has not been read yet.
//
// An absent document is not an error: a fresh deployment has no state and should
// reconcile against the cloud instead.
func (f *Firefly) Load(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.load(ctx)
}

// Lease errors that a caller has to distinguish.
var (
	// ErrNoSuchLease means the id is not one this firefly ever minted, which is a
	// bug in the caller rather than a lapse.
	//
	// It is distinct from ErrLeaseExpired, and the distinction survives pruning:
	// ids are minted from a counter, so an id at or below NextLeaseNumber existed
	// and is gone (lapsed, or released), while one above it was never issued. The
	// id is opaque to callers; it is not opaque to the thing that minted it.
	ErrNoSuchLease = errors.New("firefly: no such lease")

	// ErrUnknownMachine means a lease was requested on a machine the document does
	// not have. It is deliberately distinct from ErrNoSuchMachine, which means the
	// provider does not have it: one is our bookkeeping, the other is the world.
	// Placement reads the snapshot, so this is a race with a fade rather than a
	// programming error.
	ErrUnknownMachine = errors.New("firefly: machine not in the document")
)

// Acquire takes a lease on a machine and returns its id.
//
// It is the only way to make a machine stay, other than renewing an existing
// lease. The id is minted from a counter in the document rather than from the
// clock or from randomness, so that a restart continues the sequence instead of
// reissuing an id that a caller still holds.
func (f *Firefly) Acquire(ctx context.Context, machine MachineID, holder string, kind Kind, ttl time.Duration, now time.Time) (LeaseID, error) {
	if holder == "" {
		return "", errors.New("firefly: a lease needs a holder")
	}
	if !kind.Valid() {
		return "", fmt.Errorf("firefly: unknown lease kind %q", kind)
	}
	if ttl <= 0 {
		return "", ErrLeaseTTL
	}

	var id LeaseID
	err := f.mutate(ctx, func(doc *Document) error {
		if _, ok := doc.Machine(machine); !ok {
			return fmt.Errorf("%w: %s", ErrUnknownMachine, machine)
		}
		doc.NextLeaseNumber++
		id = LeaseID(leasePrefix + strconv.FormatUint(doc.NextLeaseNumber, 10))
		doc.Leases = append(doc.Leases, Lease{
			ID:      id,
			Machine: machine,
			Holder:  holder,
			Kind:    kind,
			Start:   now,
			TTL:     ttl,
		})
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// Renew extends a lease from now.
//
// A lapsed lease returns ErrLeaseExpired rather than being silently revived: the
// machine may already be draining, and a supervisor that re-acquired on the
// caller's behalf would hold a machine open on a lease the caller believes it
// lost.
func (f *Firefly) Renew(ctx context.Context, id LeaseID, ttl time.Duration, now time.Time) error {
	if ttl <= 0 {
		return ErrLeaseTTL
	}
	return f.mutate(ctx, func(doc *Document) error {
		for i := range doc.Leases {
			if doc.Leases[i].ID != id {
				continue
			}
			renewed, err := doc.Leases[i].Renewed(ttl, now)
			if err != nil {
				return err
			}
			doc.Leases[i] = renewed
			return nil
		}

		// Not in the document. It may have lapsed and been pruned, which is the
		// ordinary path a supervisor takes, or it may never have existed. The
		// counter is what tells them apart after the record is gone.
		if n, ok := leaseNumber(id); ok && n <= doc.NextLeaseNumber {
			return fmt.Errorf("%w: %s", ErrLeaseExpired, id)
		}
		return fmt.Errorf("%w: %s", ErrNoSuchLease, id)
	})
}

// Release drops a lease.
//
// It is idempotent: releasing an unknown or already-released id is not an error,
// because a crashed turn and a supervisor racing to clean up both call it and one
// of them is not a bug.
func (f *Firefly) Release(ctx context.Context, id LeaseID) error {
	return f.mutate(ctx, func(doc *Document) error {
		kept := doc.Leases[:0]
		for _, l := range doc.Leases {
			if l.ID != id {
				kept = append(kept, l)
			}
		}
		doc.Leases = kept
		return nil
	})
}

// leasePrefix is the one place the id format appears. Acquire mints with it and
// leaseNumber reads with it, so the two cannot drift into a state where every
// pruned renewal silently reports "no such lease" instead of "expired".
const leasePrefix = "lease-"

// leaseNumber reads the counter out of an id this package minted.
//
// The format is internal: a caller treats the id as opaque, which is why this is
// allowed to parse it and nothing else is. It is what keeps ErrLeaseExpired
// meaning "it lapsed" after pruning has removed the record, instead of collapsing
// into an error that callers are told to treat as a bug.
func leaseNumber(id LeaseID) (uint64, bool) {
	s := string(id)
	if !strings.HasPrefix(s, leasePrefix) {
		return 0, false
	}
	n, err := strconv.ParseUint(s[len(leasePrefix):], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Snapshot is a read-only view of the machines and their live lease counts.
//
// Capacity is deliberately absent: how many sessions a machine should hold is
// the caller's decision, and a number firefly invented would be a policy in the
// mechanism. The caller compares Live against its own limit.
func (f *Firefly) Snapshot(ctx context.Context, now time.Time) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(ctx); err != nil {
		return Snapshot{}, err
	}

	out := Snapshot{At: now}
	for _, m := range f.doc.Machines {
		status := MachineStatus{
			ID:       m.ID,
			State:    m.State,
			Origin:   m.Origin,
			Since:    m.Since,
			Deadline: m.Deadline,
		}
		for _, l := range f.doc.Leases {
			if l.Machine == m.ID && l.Live(now) {
				status.Live++
			}
		}
		out.Machines = append(out.Machines, status)
	}
	return out, nil
}

// Snapshot is what a caller reads to decide placement and to render state.
type Snapshot struct {
	At       time.Time
	Machines []MachineStatus
}

// MachineStatus is one machine's line in a snapshot.
type MachineStatus struct {
	ID     MachineID
	State  State
	Origin State
	Since  time.Time

	// Deadline is the hard cap, resolved at creation. It is here rather than left
	// to the caller because placement has to be able to see that a machine is at
	// or past its cap: Live still counts leases that Tick has not yet acted on, so
	// without this a caller could place work on a machine that is about to be
	// destroyed and lose a turn to a race it could have avoided.
	Deadline time.Time

	// Live is how many leases are live at the snapshot's instant. Zero means the
	// machine is draining or about to be.
	Live int
}

// mutate applies fn to a copy of the document and commits it only if the store
// accepts it.
//
// Copy-then-commit is the whole reason a failed save cannot leave memory ahead of
// the store: the caller's view is the document that was actually persisted. The
// alternative -- mutate in place and roll back on failure -- is the same logic
// with a chance of getting the rollback wrong.
func (f *Firefly) mutate(ctx context.Context, fn func(doc *Document) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(ctx); err != nil {
		return err
	}

	next := f.doc.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	if err := f.save(ctx, next); err != nil {
		return err
	}
	f.doc = next
	return nil
}

// load reads the document once, translating an absent one into an empty document
// of the current version.
func (f *Firefly) load(ctx context.Context) error {
	if f.loaded {
		return nil
	}
	doc, err := f.cfg.Store.Load(ctx)
	switch {
	case errors.Is(err, ErrNoDocument):
		f.doc = Document{Version: DocumentVersion}
	case err != nil:
		return err
	default:
		f.doc = doc
	}
	f.loaded = true
	return nil
}

// save validates and persists, refusing to write a document that could not
// describe a real world.
func (f *Firefly) save(ctx context.Context, doc Document) error {
	if err := doc.Validate(); err != nil {
		return err
	}
	return f.cfg.Store.Save(ctx, doc)
}
