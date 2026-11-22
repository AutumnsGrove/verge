// Package firefly keeps exactly as many machines alive as the work needs, and
// no more.
//
// It owns when a machine exists; its callers own why. It is handed holder
// names — opaque strings like "session:01J8…" — and never a session ID, so
// nothing in this package joins to anything. See docs/firefly.md for the
// boundary, the lease model, and the semantics deletion guarantees.
//
// This file is the state machine, as data. The design rule: firefly is driven
// by an injected clock and never reads the wall clock itself, so every
// duration here is a parameter.
package firefly

import "fmt"

// State is where a machine is in its lifecycle. These seven values are the
// whole vocabulary of the mechanism; everything else is a lease, a fault
// reported by the cloud, or policy supplied by the caller.
type State string

const (
	// Absent means no machine exists. It is both the start and the end.
	Absent State = "ABSENT"

	// Provisioning means a create was issued and no machine has answered yet.
	//
	// It is deliberately not "created": the provider reporting a machine is
	// not the same as that machine being usable, and the gap between the two
	// is where a stalled boot hides.
	Provisioning State = "PROVISIONING"

	// Ready means the machine booted, answered its metadata service, and
	// joined the private network — both facts, because either alone can lie.
	Ready State = "READY"

	// Draining means no lease is live and the caller's drain window is
	// running. It is not a point of no return; see LeaseArrived.
	Draining State = "DRAINING"

	// Syncing means the machine's state is being saved to object storage.
	Syncing State = "SYNCING"

	// Terminating means a delete was issued and the machine may or may not be
	// gone. Deletion is two facts: the machine half and the network node half.
	Terminating State = "TERMINATING"

	// Error means a step failed and the machine still exists. It is never a
	// resting place and never terminal; each Error carries its origin so that
	// RetryDue can return there.
	Error State = "ERROR"
)

// Trigger is what moves a machine. Every trigger is an observation or a
// deadline reported from outside; none of them is a decision firefly makes on
// its own, which is what keeps the loop testable from a table.
type Trigger string

const (
	// Need means work arrived and there is no machine to run it on.
	Need Trigger = "Need"

	// CreateFailed means the provider refused a create. No machine exists, so
	// this cannot be an Error — see docs/firefly.md.
	CreateFailed Trigger = "CreateFailed"

	// Created means the provider returned an ID that is not answering yet.
	Created Trigger = "Created"

	// Answered means the machine answered its metadata service and joined the
	// private network.
	Answered Trigger = "Answered"

	// Stalled means a boot exceeded the caller's boot deadline.
	Stalled Trigger = "Stalled"

	// LeasesLapsed means the last live lease expired and the drain window
	// started.
	LeasesLapsed Trigger = "LeasesLapsed"

	// LeaseArrived means work arrived during the drain window.
	LeaseArrived Trigger = "LeaseArrived"

	// Drained means the drain window elapsed with no lease.
	Drained Trigger = "Drained"

	// Synced means the machine's state was saved successfully.
	Synced Trigger = "Synced"

	// SyncFailed means the save failed. The machine stays; see docs/firefly.md
	// for why that asymmetry is deliberate.
	SyncFailed Trigger = "SyncFailed"

	// Abandoned means the save has failed past the hard cap and is recorded as
	// lost. It is the only automatic path to Terminating that skips a
	// successful sync, and it is counted.
	Abandoned Trigger = "Abandoned"

	// MachineGone means the provider no longer has the machine. It resolves
	// the machine half of deletion and nothing else.
	MachineGone Trigger = "MachineGone"

	// Confirmed means both halves of deletion are resolved.
	Confirmed Trigger = "Confirmed"

	// Unconfirmed means a delete did not confirm by its deadline. It re-checks
	// by reading on a later tick; it never waits.
	Unconfirmed Trigger = "Unconfirmed"

	// RetryDue means an Error's backoff has elapsed and the originating step
	// should be tried again.
	RetryDue Trigger = "RetryDue"

	// Capped means the caller's hard maximum lifetime elapsed. It routes
	// through a save; see Cappable.
	Capped Trigger = "Capped"
)

// Transition is one row of the table.
//
// The table is data rather than a switch so that the gate in fsm_test.go can
// loop it: a row with no test case fails the build, which is how
// "TERMINATING slept forever once" becomes a compile-time-shaped mistake
// instead of a review comment nobody left.
type Transition struct {
	From State
	On   Trigger
	To   State
}

// transitions is docs/firefly.md's table, transcribed. Every row here has a
// case in fsmCases, and TestEveryTransitionHasACase fails if that stops being
// true. Adding a row without a case is the failure mode this file exists to
// prevent, so add the row and the case in the same commit.
//
// Note what is absent: there is no row from Terminating into Syncing. A cap
// that could interrupt a delete in flight would un-issue it.
var transitions = []Transition{
	{Absent, Need, Provisioning},
	{Absent, CreateFailed, Absent},
	{Provisioning, Created, Provisioning},
	{Provisioning, Answered, Ready},
	{Provisioning, Stalled, Error},
	{Ready, LeasesLapsed, Draining},
	{Draining, LeaseArrived, Ready},
	{Draining, Drained, Syncing},
	{Syncing, Synced, Terminating},
	{Syncing, SyncFailed, Error},
	{Syncing, Abandoned, Terminating},
	{Terminating, MachineGone, Terminating},
	{Terminating, Confirmed, Absent},
	{Terminating, Unconfirmed, Error},

	// Error is the one state whose destination depends on its history rather
	// than only on the trigger, so the table holds one concrete row per origin
	// instead of a wildcard the gate could not check.
	{Error, RetryDue, Provisioning},
	{Error, RetryDue, Syncing},
	{Error, RetryDue, Terminating},

	{Provisioning, Capped, Syncing},
	{Ready, Capped, Syncing},
	{Draining, Capped, Syncing},
	{Error, Capped, Syncing},
}

// Valid reports whether s is one of the seven states.
//
// It exists for the same reason Kind.Valid does: the state is persisted, so a
// document read back with an unknown one would otherwise be accepted and then
// match no row in the transition table — a machine that firefly owns and cannot
// advance.
func (s State) Valid() bool {
	switch s {
	case Absent, Provisioning, Ready, Draining, Syncing, Terminating, Error:
		return true
	default:
		return false
	}
}

// errorOrigins returns the states a machine can enter Error *from*, derived from
// the table.
//
// This is what a stored Machine.Origin must be a member of: Origin records where
// the machine was before it failed. It is deliberately not the same computation
// as retryDestinations, which the previous version of this function conflated.
// The two sets coincide for the current table, which is exactly why the mistake
// was invisible: a future row into Error without a matching RetryDue row would
// have produced a document that validates and can never be retried.
func errorOrigins() map[State]struct{} {
	out := make(map[State]struct{})
	for _, tr := range transitions {
		if tr.To == Error {
			out[tr.From] = struct{}{}
		}
	}
	return out
}

// retryDestinations returns where an Error may go on RetryDue, derived from the
// table.
//
// TestOriginsAndRetriesAgree asserts it covers every entry in errorOrigins, so
// the two cannot drift: a state that can fail must also be one an Error can
// return to.
func retryDestinations() map[State]struct{} {
	out := make(map[State]struct{})
	for _, tr := range transitions {
		if tr.From == Error && tr.On == RetryDue {
			out[tr.To] = struct{}{}
		}
	}
	return out
}

// Cappable reports whether the hard cap may push a machine into Syncing.
//
// It is a predicate over state *and* origin, not a set of states, because one
// state does not mean one thing: an Error whose origin is Terminating is a
// delete in flight, and capping it into Syncing could then reach Abandoned —
// logging a data loss and incrementing the abandonment counter for a machine
// whose save had already succeeded. One such false positive is enough to turn
// that counter from an alarm into noise.
//
// A delete in flight needs nothing from the cap: it has its own deadline and
// its own exit, Unconfirmed then RetryDue.
func Cappable(state, origin State) bool {
	switch state {
	case Provisioning, Ready, Draining:
		return true
	case Error:
		return origin != Terminating
	default:
		return false
	}
}

// ErrNoTransition is returned when a state and trigger have no row. It is not
// a bug in callers by itself — the fake cloud and the real one can both report
// something unexpected — but it is always worth surfacing rather than
// guessing, because guessing here is how a machine quietly stops being
// managed.
type ErrNoTransition struct {
	From   State
	On     Trigger
	Origin State
}

func (e ErrNoTransition) Error() string {
	if e.On == RetryDue || e.From == Error {
		return fmt.Sprintf("firefly: no transition from %s on %s (origin %s)", e.From, e.On, e.Origin)
	}
	return fmt.Sprintf("firefly: no transition from %s on %s", e.From, e.On)
}

// Next returns the state a machine in from moves to on trigger.
//
// origin is the state the machine was in before entering Error. It is
// consulted only when from is Error; for every other state it is ignored, so
// callers that do not track an origin can pass Absent.
//
// Next is pure: no clock, no cloud, no mutation. That is what lets every
// transition in the table be a table-driven case.
func Next(from State, on Trigger, origin State) (State, error) {
	if from == Error && on == Capped && !Cappable(from, origin) {
		return "", ErrNoTransition{From: from, On: on, Origin: origin}
	}
	for _, tr := range transitions {
		if tr.From != from || tr.On != on {
			continue
		}
		// The Error/RetryDue rows share a key and differ only by destination,
		// so the origin chooses between them.
		if from == Error && on == RetryDue && tr.To != origin {
			continue
		}
		return tr.To, nil
	}
	return "", ErrNoTransition{From: from, On: on, Origin: origin}
}
