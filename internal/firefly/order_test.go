// The ordering property, in the file docs/testing.md names for it.
//
// Over any generated history, a DELETE for a machine is preceded by a successful
// save for it, or by a recorded abandonment. It is the reason firefly exists at
// all -- a machine is cheap and an afternoon of uncommitted work is not -- and it
// is stated over *any* history rather than one golden log, because the sequences
// that break it are the ones nobody writes down.
//
// Nothing sleeps here. firefly is driven by an injected clock and every duration
// it obeys is a parameter, so the generator draws work arriving, work ending,
// saves failing and time passing, and the fake world advances with it.
package firefly_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
	"pgregory.net/rapid"
)

// ledger is what one generated history is checked against as it runs.
//
// It records facts the running world has shown us -- what was saved, what was
// deleted, what loss was acknowledged -- and deliberately not a model of firefly.
// A model would let the test agree with itself; the point is to compare firefly's
// reported transitions against the parts of the world it does not control: the
// syncer's record of successful saves and the provider's record of deletes.
//
// It is per-history rather than shared, because "no save failed in this history"
// is a claim about this history.
//
// One limit worth naming: synced is what the fake syncer has saved *so far*, not
// what it saved on this teardown. That is sound because a machine enters SYNCING
// once and tears down once, so an earlier save for it can only be the save that
// led here -- and there is no second save for a mutation to skip. The histories
// also hold one machine each, so a mix-up between machines is not what this
// catches; the maps are keyed by id and would, if a history ever needed two.
type ledger struct {
	synced    map[firefly.MachineID]bool
	abandoned map[firefly.MachineID]bool
	deleted   map[firefly.MachineID]bool

	faults    int // sync failures injected so far in this history
	deletes   int // deletes issued so far
	confirmed int // machines that finished leaving
}

func newLedger() *ledger {
	return &ledger{
		synced:    make(map[firefly.MachineID]bool),
		abandoned: make(map[firefly.MachineID]bool),
		deleted:   make(map[firefly.MachineID]bool),
	}
}

// observe folds one tick into the record and fails the iteration on the first
// DELETE it cannot justify.
func (l *ledger) observe(t tb, h *harness, res firefly.Result) {
	t.Helper()

	for _, id := range h.sync.Synced() {
		l.synced[id] = true
	}
	for _, id := range res.Abandoned {
		l.abandoned[id] = true
		if l.faults == 0 {
			// docs/testing.md: the abandonment counter's steady state is zero. A
			// machine abandoned in a history where no save ever failed would mean
			// something gave up on a save that had worked.
			t.Fatalf("machine %s was abandoned with no sync failure injected", id)
		}
	}

	for _, ev := range res.Transitions {
		// Entering Terminating is the moment a delete is issued, and therefore the
		// moment the machine's state must already be saved. The Terminating -->
		// Terminating self-loop is the node-record repair, not a second delete.
		if ev.To != firefly.Terminating || ev.From == firefly.Terminating {
			continue
		}
		l.deletes++

		switch {
		case ev.Trigger == firefly.Synced && l.synced[ev.Machine]:
			// The ordinary path: this tick's save succeeded, then the delete.

		case ev.Trigger == firefly.Abandoned && l.abandoned[ev.Machine]:
			// The one stated exception, recorded where a caller sees it.
			// docs/firefly.md, "Where the ordering property yields".

		default:
			t.Fatalf("DELETE for %s (%s --%s--> %s) with no successful save and no recorded abandonment",
				ev.Machine, ev.From, ev.Trigger, ev.To)
		}

		if l.deleted[ev.Machine] {
			t.Fatalf("a second delete was issued for %s", ev.Machine)
		}
		l.deleted[ev.Machine] = true
	}

	for _, id := range res.Confirmed {
		// A machine that left the document without a delete would be bookkeeping
		// losing a machine, which costs a person exactly what deleting an unsaved
		// one does.
		if !l.deleted[id] {
			t.Fatalf("machine %s was removed from the document without a delete", id)
		}
		l.confirmed++
	}
}

// TestOrderingProperty generates histories and checks the property on every tick.
//
// The generator excludes exactly the fault docs/testing.md says to exclude: a
// save that can never succeed. Transient failures are drawn, because a machine
// that fails a save twice and succeeds on the third must still be saved before it
// is destroyed -- and because enough consecutive failures may abandon it, which
// the property permits only when the loss is recorded.
//
// Whether a drawn history happens to abandon a save is up to the draw, so the
// exception's branch is not asserted from here. It is asserted from the sad/ case
// below, which is generated too: every save fails there, so the branch is reached
// on every history whatever the clock does.
func TestOrderingProperty(t *testing.T) {
	var deletes, confirmed atomic.Int64

	rapid.Check(t, func(t *rapid.T) {
		h := newHarness(t)
		id := h.provision(firefly.Ready)
		l := newLedger()

		now := base
		var leases []firefly.LeaseID

		steps := rapid.IntRange(1, 12).Draw(t, "steps")
		for i := 0; i < steps; i++ {
			switch rapid.IntRange(0, 4).Draw(t, "move") {
			case 0:
				// A save that fails. Drawing up to SyncAttempts leaves the generator
				// able to reach an abandonment on its own, and the ledger will hold the
				// loop to it if it does: recorded, or the delete is a failure.
				failures := rapid.IntRange(1, h.f.Config().SyncAttempts).Draw(t, "failures")
				h.sync.FailNext(failures)
				l.faults++

			case 1:
				// Work arrives. A lease on a machine that has already faded is a
				// caller racing the fade, which placement handles; it is not a
				// failure of the ordering property.
				ttl := time.Duration(rapid.IntRange(1, 120).Draw(t, "ttl")) * time.Minute
				holder := rapid.SampledFrom([]string{"session:one", "session:two"}).Draw(t, "holder")
				if lease, err := h.f.Acquire(context.Background(), id, holder, firefly.Session, ttl, now); err == nil {
					leases = append(leases, lease)
				}

			case 2:
				// Work ends. Release is idempotent, so releasing twice is a
				// history rather than a bug.
				if len(leases) > 0 {
					i := rapid.IntRange(0, len(leases)-1).Draw(t, "which")
					if err := h.f.Release(context.Background(), leases[i]); err != nil {
						t.Fatalf("Release: %v", err)
					}
					leases = append(leases[:i], leases[i+1:]...)
				}
			}

			// Time passes, and never backwards: the fake's clock is the world's,
			// and a history that ran it backwards would be testing a fiction.
			now = now.Add(time.Duration(rapid.IntRange(0, 90*60).Draw(t, "seconds")) * time.Second)

			l.observe(t, h, h.tick(now))
		}

		deletes.Add(int64(l.deletes))
		confirmed.Add(int64(l.confirmed))
	})

	// A property that never saw a delete is true and worthless. These are the
	// minimal witnesses that the generated histories reach the interesting part.
	if deletes.Load() == 0 {
		t.Fatalf("no delete was issued in any generated history: the property held vacuously")
	}
	if confirmed.Load() == 0 {
		t.Fatalf("no machine finished leaving in any generated history: the property held vacuously")
	}
}

// TestOrderingAbandonmentIsTheRecordedException is the one sad/ case the property
// allows: a save that can never succeed would bill forever if the hard cap also
// refused to abandon it.
//
// It is checked through the same ledger as the property, which is what makes it
// the exception's own test rather than a state assertion: the delete must be
// preceded by an abandonment that was reported to the caller, exactly once, and
// by no successful save at all. TestAbandonmentIsRecorded in tick_test.go
// asserts the transition; this asserts what the transition is allowed to excuse.
//
// The clock is drawn rather than fixed, so the exception branch of the ledger is
// exercised over generated histories and not one walk. Abandonment stays certain
// whatever the draw: every save fails, so the machine cannot reach a delete
// without the loss being recorded first.
func TestOrderingAbandonmentIsTheRecordedException(t *testing.T) {
	t.Run("sad/abandonment_excuses_the_delete", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			h := newHarness(t)
			id := h.provision(firefly.Ready)
			h.sync.FailAlways()

			l := newLedger()
			// FailAlways is a fault, and the ledger's rule is that abandonment is
			// only legal in a history where a save failed. Being unable to fail
			// differently is the eventual case of that rule, not an exemption.
			l.faults = 1

			// Every tick advances time, by at least a minute, so the retry backoff
			// is always due and the loop cannot spin on a frozen clock.
			now := base
			for i := 0; i < 100 && len(l.abandoned) == 0; i++ {
				now = now.Add(time.Duration(rapid.IntRange(1, 90).Draw(t, "minutes")) * time.Minute)
				l.observe(t, h, h.tick(now))
			}

			if len(l.abandoned) != 1 || !l.abandoned[id] {
				t.Fatalf("abandoned = %v, want exactly [%s]", l.abandoned, id)
			}
			if len(l.synced) != 0 {
				t.Fatalf("the syncer recorded successful saves %v for a save that cannot succeed", l.synced)
			}

			// Abandoned on paper is worthless unless the machine actually leaves,
			// so keep the clock running until the provider's half resolves.
			for i := 0; i < 100 && l.confirmed == 0; i++ {
				now = now.Add(time.Duration(rapid.IntRange(1, 90).Draw(t, "minutes")) * time.Minute)
				l.observe(t, h, h.tick(now))
			}
			if l.confirmed != 1 {
				t.Fatalf("the machine was abandoned but never left: confirmed=%d", l.confirmed)
			}
			if l.deletes != 1 {
				t.Fatalf("deletes = %d, want 1: an abandonment issues exactly one delete", l.deletes)
			}
		})
	})
}
