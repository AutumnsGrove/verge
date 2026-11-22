package firefly

import (
	"errors"
	"fmt"
	"testing"
)

// fsmCase is one asserted transition.
//
// Case names carry the category — happy/, sad/, bad/ — per docs/testing.md, so
// `go test -run 'TestFSM/bad'` works and so a missing category is visible in
// review rather than argued about in it.
type fsmCase struct {
	name   string
	from   State
	on     Trigger
	origin State
	want   State
	isErr  bool
}

// fsmCases has one case per row of the transitions table, plus the cases that
// assert what the table does *not* allow. TestEveryTransitionHasACase checks
// both directions: a row without a case, and a case without a row.
var fsmCases = []fsmCase{
	// happy/: the normal path, provision through to absent.
	{"happy/need_provisions", Absent, Need, Absent, Provisioning, false},
	{"happy/created_records_id", Provisioning, Created, Absent, Provisioning, false},
	{"happy/answered_becomes_ready", Provisioning, Answered, Absent, Ready, false},
	{"happy/leases_lapse_drains", Ready, LeasesLapsed, Absent, Draining, false},
	{"happy/lease_during_drain_readies", Draining, LeaseArrived, Absent, Ready, false},
	{"happy/drained_syncs", Draining, Drained, Absent, Syncing, false},
	{"happy/synced_terminates", Syncing, Synced, Absent, Terminating, false},
	{"happy/machine_gone_stays_terminating", Terminating, MachineGone, Absent, Terminating, false},
	{"happy/confirmed_absorbs", Terminating, Confirmed, Absent, Absent, false},

	// sad/: an expected failure the design handles.
	{"sad/create_failed_stays_absent", Absent, CreateFailed, Absent, Absent, false},
	{"sad/stalled_is_an_error_with_the_machine", Provisioning, Stalled, Absent, Error, false},
	{"sad/sync_failed_keeps_the_machine", Syncing, SyncFailed, Absent, Error, false},
	{"sad/abandoned_terminates_at_the_cap", Syncing, Abandoned, Absent, Terminating, false},
	{"sad/unconfirmed_goes_to_error", Terminating, Unconfirmed, Absent, Error, false},
	{"sad/retry_returns_to_provisioning", Error, RetryDue, Provisioning, Provisioning, false},
	{"sad/retry_returns_to_syncing", Error, RetryDue, Syncing, Syncing, false},
	{"sad/retry_returns_to_terminating", Error, RetryDue, Terminating, Terminating, false},

	// The cap, which routes through a save in every cappable state.
	{"happy/capped_provisioning_syncs", Provisioning, Capped, Absent, Syncing, false},
	{"happy/capped_ready_syncs", Ready, Capped, Absent, Syncing, false},
	{"happy/capped_draining_syncs", Draining, Capped, Absent, Syncing, false},
	{"happy/capped_error_syncs", Error, Capped, Syncing, Syncing, false},

	// bad/: what the table must refuse. These are the assertions that stop a
	// future edit from making a delete in flight interruptible, or from
	// inventing a transition nobody designed.
	{"bad/terminating_is_never_capped", Terminating, Capped, Absent, "", true},
	{"bad/error_from_terminating_is_never_capped", Error, Capped, Terminating, "", true},
	{"bad/absent_has_no_retry_row", Error, RetryDue, Absent, "", true},
	{"bad/no_transition_from_absent_on_synced", Absent, Synced, Absent, "", true},
	{"bad/ready_does_not_terminate_directly", Ready, Confirmed, Absent, "", true},
	{"bad/unknown_state", State("BOGUS"), Need, Absent, "", true},
	{"bad/unknown_trigger", Ready, Trigger("Exploded"), Absent, "", true},
}

func TestFSM(t *testing.T) {
	for _, c := range fsmCases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Next(c.from, c.on, c.origin)
			if c.isErr {
				if err == nil {
					t.Fatalf("Next(%s, %s, origin %s) = %s, want an error",
						c.from, c.on, c.origin, got)
				}
				var noTransition ErrNoTransition
				if !errors.As(err, &noTransition) {
					t.Fatalf("Next(%s, %s, origin %s) error = %T (%v), want ErrNoTransition",
						c.from, c.on, c.origin, err, err)
				}
				if noTransition.From != c.from || noTransition.On != c.on {
					t.Fatalf("ErrNoTransition identifies %s on %s, want %s on %s",
						noTransition.From, noTransition.On, c.from, c.on)
				}
				return
			}
			if err != nil {
				t.Fatalf("Next(%s, %s, origin %s) = error %v, want %s",
					c.from, c.on, c.origin, err, c.want)
			}
			if got != c.want {
				t.Fatalf("Next(%s, %s, origin %s) = %s, want %s",
					c.from, c.on, c.origin, got, c.want)
			}
		})
	}
}

// TestEveryTransitionHasACase is the gate docs/testing.md describes: the table
// is data, this test loops it, and a row with no case fails the build.
//
// It also checks the other direction. A case asserting a transition the table
// does not contain is a stale test that will pass forever and prove nothing,
// which is worse than no test at all.
func TestEveryTransitionHasACase(t *testing.T) {
	for _, tr := range transitions {
		tr := tr
		t.Run(fmt.Sprintf("row/%s_on_%s_to_%s", tr.From, tr.On, tr.To), func(t *testing.T) {
			for _, c := range fsmCases {
				if c.isErr || c.from != tr.From || c.on != tr.On {
					continue
				}
				// The Error/RetryDue rows share a key and differ only by
				// destination, so the case's origin must select this row.
				if tr.From == Error && tr.On == RetryDue && c.origin != tr.To {
					continue
				}
				if c.want != tr.To {
					t.Fatalf("case %q asserts %s --%s--> %s, but the table says %s",
						c.name, c.from, c.on, c.want, tr.To)
				}
				return
			}
			t.Fatalf("transition %s --%s--> %s has no case in fsmCases", tr.From, tr.On, tr.To)
		})
	}

	for _, c := range fsmCases {
		if c.isErr {
			continue
		}
		found := false
		for _, tr := range transitions {
			if tr.From != c.from || tr.On != c.on || tr.To != c.want {
				continue
			}
			if tr.From == Error && tr.On == RetryDue && c.origin != tr.To {
				continue
			}
			found = true
			break
		}
		if !found {
			t.Errorf("case %q asserts %s --%s--> %s, which is not in the table", c.name, c.from, c.on, c.want)
		}
	}
}

// TestCappable pins the predicate that the Capped row depends on, separately
// from Next, because it is the rule that keeps a delete in flight from being
// interrupted and a table test alone would not make that obvious.
func TestCappable(t *testing.T) {
	cases := []struct {
		name   string
		state  State
		origin State
		want   bool
	}{
		{"happy/provisioning", Provisioning, Absent, true},
		{"happy/ready", Ready, Absent, true},
		{"happy/draining", Draining, Absent, true},
		{"happy/error_from_syncing", Error, Syncing, true},
		{"happy/error_from_provisioning", Error, Provisioning, true},
		{"bad/terminating", Terminating, Absent, false},
		{"bad/error_from_terminating", Error, Terminating, false},
		{"bad/absent", Absent, Absent, false},
		{"bad/syncing", Syncing, Absent, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Cappable(c.state, c.origin); got != c.want {
				t.Fatalf("Cappable(%s, origin %s) = %v, want %v", c.state, c.origin, got, c.want)
			}
		})
	}
}
