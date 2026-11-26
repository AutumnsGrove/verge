package firefly

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TransitionEvent is one machine moving one row of the table, for the caller to
// log. It is the only record of why anything changed, so it carries the trigger
// rather than just the two states.
type TransitionEvent struct {
	Machine MachineID
	From    State
	To      State
	Trigger Trigger
}

// Result is what one tick did.
//
// It is a report, not a control channel: the caller logs it and nothing more.
// Every field is a list rather than a count because "which machine" is the
// question an operator asks, and a number cannot answer it.
type Result struct {
	// Adopted are machines found in the cloud that the document did not know
	// about, now under management.
	Adopted []MachineID

	// Swept are machines deleted without ever being adopted, because they were
	// past the hard cap. They were ours and are not any more.
	Swept []MachineID

	// Confirmed are machines that finished deleting and were removed from the
	// document.
	Confirmed []MachineID

	// Abandoned are machines whose save could not succeed before the cap, so they
	// were destroyed with the loss recorded. This must be empty outside a fault
	// test, and it is the same counter the operator override increments.
	Abandoned []MachineID

	// Transitions is every row walked this tick.
	Transitions []TransitionEvent

	// Saved reports whether the document was written.
	Saved bool
}

// Tick reconciles once, at now.
//
// It is the whole mechanism, and it obeys four rules that the rest of this
// package exists to make checkable:
//
//  1. The provider is asked, never listened to.
//  2. Presence in a listing is evidence; absence is not. Only Describe reports a
//     machine gone.
//  3. Every machine moves at most one row.
//  4. The document is written once, if anything changed.
//
// A cloud error aborts the tick rather than being recorded per machine: a tick
// that half-reconciles against a provider that is failing is worse than one that
// did nothing, because the next tick has to reason about a half-applied pass.
func (f *Firefly) Tick(ctx context.Context, now time.Time) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.load(ctx); err != nil {
		return Result{}, err
	}

	next := f.doc.Clone()
	var res Result
	changed := false

	// --- orphans ---------------------------------------------------------
	//
	// A machine in the listing that the document does not know is either ours
	// from a previous life (a restart lost the document, a write was rejected) or
	// someone else's that happens to carry the label. Adoption is the repair;
	// sweeping is the cost of being wrong about age.
	listed, err := f.cfg.Cloud.List(ctx, f.cfg.Label)
	if err != nil {
		return Result{}, fmt.Errorf("firefly: listing machines: %w", err)
	}

	known := make(map[MachineID]struct{}, len(next.Machines))
	for _, m := range next.Machines {
		known[m.ID] = struct{}{}
	}
	for _, cm := range listed {
		if _, ok := known[cm.ID]; ok {
			continue
		}
		known[cm.ID] = struct{}{}

		if now.Sub(cm.CreatedAt) >= f.cfg.MaxLifetime {
			// Past the cap before we ever knew it: delete it and say so. Nothing
			// is synced first, because a machine we have never managed has no
			// state of ours to save -- and this is the one deletion path that does
			// not route through a save.
			if err := f.cfg.Cloud.Delete(ctx, cm.ID); err != nil && !errors.Is(err, ErrNoSuchMachine) {
				return Result{}, fmt.Errorf("firefly: sweeping %s: %w", cm.ID, err)
			}
			res.Swept = append(res.Swept, cm.ID)
			continue
		}

		// Adoption is the one path where the name comes from the provider rather
		// than from a mint, and it has to: the node this machine actually has is
		// keyed on the name it was created with, so a name invented here would be a
		// machine firefly could never ask the network about.
		//
		// An unnamed or duplicated machine is therefore a fact to report rather than
		// to paper over. Guessing would produce a document Validate refuses, every
		// tick, forever; a duplicate name means two machines sharing one node record
		// and a delete that looks like it deleted both. The check sits after the cap
		// above on purpose: a machine past its deadline is swept by id, which needs no
		// name, and a tick that refused to sweep would be the one thing the cap exists
		// to prevent.
		if cm.Name == "" {
			return Result{}, fmt.Errorf("firefly: the provider reported machine %s without a name, and the network is keyed on it", cm.ID)
		}
		if next.nameTaken(cm.Name) {
			return Result{}, fmt.Errorf("firefly: the provider reported machine %s named %q, which the document already uses for another machine",
				cm.ID, cm.Name)
		}

		next.Machines = append(next.Machines, Machine{
			ID:        cm.ID,
			Name:      cm.Name,
			State:     Provisioning,
			CreatedAt: cm.CreatedAt,
			Deadline:  cm.CreatedAt.Add(f.cfg.MaxLifetime),
			Since:     now,
		})
		res.Adopted = append(res.Adopted, cm.ID)
		changed = true
	}

	// --- one row per machine --------------------------------------------
	kept := next.Machines[:0]
	for i := range next.Machines {
		m := next.Machines[i]

		ev, err := f.step(ctx, &next, &m, now)
		if err != nil {
			return res, err
		}
		if ev.Trigger != "" {
			res.Transitions = append(res.Transitions, ev)
			changed = true
			// Abandonment is the one transition that reports data loss, so it is
			// recorded where a caller will see it and not only in the log.
			if ev.Trigger == Abandoned {
				res.Abandoned = append(res.Abandoned, m.ID)
			}
		}

		// A machine that has finished deleting carries no information; keeping it
		// would grow the document by one row per machine ever created.
		if m.State == Absent {
			res.Confirmed = append(res.Confirmed, m.ID)
			changed = true
			continue
		}
		kept = append(kept, m)
	}
	next.Machines = kept

	// --- leases -----------------------------------------------------------
	//
	// Two things have to be true before the document can be saved: no lease may
	// name a machine that is not in it (Validate rejects that, and the rejection
	// would repeat every tick forever), and lapsed leases should not accumulate.
	// Both are handled here rather than at each site that removes something.
	liveMachines := make(map[MachineID]struct{}, len(next.Machines))
	for _, m := range next.Machines {
		liveMachines[m.ID] = struct{}{}
	}
	keptLeases := next.Leases[:0]
	for _, l := range next.Leases {
		if _, ok := liveMachines[l.Machine]; !ok {
			changed = true
			continue
		}
		if !l.Live(now) {
			changed = true
			continue
		}
		keptLeases = append(keptLeases, l)
	}
	next.Leases = keptLeases

	// --- one write -------------------------------------------------------
	if !changed {
		return res, nil
	}
	if err := f.save(ctx, next); err != nil {
		return res, err
	}
	f.doc = next
	res.Saved = true
	return res, nil
}

// step walks one machine at most one row and reports what it did.
//
// The transition is written to m before this returns, so the caller's slice holds
// the new state whether or not the tick is later saved -- and if the save fails,
// the in-memory document is discarded wholesale, so a half-walk cannot leak into
// the next tick.
func (f *Firefly) step(ctx context.Context, doc *Document, m *Machine, now time.Time) (TransitionEvent, error) {
	live := liveLeases(*doc, m.ID, now)

	// move walks a row and clears the error record, because arriving anywhere
	// successfully means the failure is over. It refuses to enter Error: that
	// transition carries a payload, and a row that entered Error without an
	// origin would produce a document Validate rejects.
	move := func(on Trigger) (TransitionEvent, error) {
		to, err := Next(m.State, on, m.Origin)
		if err != nil {
			return TransitionEvent{}, err
		}
		if to == Error {
			return TransitionEvent{}, fmt.Errorf("firefly: %s --%s--> ERROR needs a cause; use fail", m.State, on)
		}
		ev := TransitionEvent{Machine: m.ID, From: m.State, To: to, Trigger: on}
		m.State = to
		m.Since = now
		m.Origin = ""
		m.Attempt = 0
		m.LastErr = ""
		m.RetryAt = time.Time{}
		// Failures is cleared by success, not by movement. The cap moves a machine
		// into SYNCING whether or not the save has been working, so clearing it
		// here would reset the count every time the cap fired -- making
		// exhausted-retries abandonment unreachable, which is precisely the hole
		// the counter was added to close.
		if on != Capped {
			m.Failures = 0
		}
		if to == Ready {
			m.ReachedReady = true
		}
		return ev, nil
	}

	// fail records an error. Attempt belongs to the Error record and is cleared
	// with it; Failures counts consecutive failures across retries, which is what
	// the cap needs and what a per-error attempt counter cannot express.
	fail := func(origin State, cause error) (TransitionEvent, error) {
		ev := TransitionEvent{Machine: m.ID, From: m.State, To: Error, Trigger: triggerFor(m.State)}
		m.State = Error
		m.Origin = origin
		m.Since = now
		m.Attempt++
		m.Failures++
		m.LastErr = cause.Error()
		m.RetryAt = now.Add(f.retryDelay(m.Attempt))
		return ev, nil
	}

	// The cap comes first for every state it may act on, so that no state can
	// outlive its deadline by being busy failing.
	if !now.Before(m.Deadline) && Cappable(m.State, m.Origin) {
		return move(Capped)
	}

	switch m.State {
	case Provisioning:
		cm, err := f.cfg.Cloud.Describe(ctx, m.ID)
		switch {
		case errors.Is(err, ErrNoSuchMachine):
			return fail(Provisioning, fmt.Errorf("the provider does not have it: %w", err))
		case err != nil:
			return TransitionEvent{}, fmt.Errorf("firefly: describing %s: %w", m.ID, err)
		}

		// Ready is two facts, and the network one is what a stalled cloud-init
		// cannot fake. A provider saying running is not a machine you can use.
		joined, err := f.cfg.Network.Joined(ctx, m.Name)
		if err != nil {
			return TransitionEvent{}, fmt.Errorf("firefly: asking whether %s joined: %w", m.Name, err)
		}
		if cm.Status == ProviderRunning && joined {
			return move(Answered)
		}
		if now.Sub(m.CreatedAt) >= f.cfg.BootDeadline {
			return fail(Provisioning, errors.New("the boot exceeded its deadline"))
		}
		return TransitionEvent{}, nil

	case Ready:
		if live == 0 {
			return move(LeasesLapsed)
		}
		return TransitionEvent{}, nil

	case Draining:
		// The drain window is a courtesy: work arriving during it costs nothing to
		// accept, and refusing it would turn a busy session into a cold clone.
		if live > 0 {
			return move(LeaseArrived)
		}
		if now.Sub(m.Since) >= f.cfg.DrainWindow {
			return move(Drained)
		}
		return TransitionEvent{}, nil

	case Syncing:
		// A machine that never became ready has nothing on it, so its save is a
		// successful no-op. Without this, a boot that never answers could never be
		// destroyed, which is the opposite of what the cap is for.
		//
		// Whatever happens here, entering Terminating issues the delete: a machine
		// that reached TERMINATING and never called Delete would sit in the
		// document forever while the provider kept billing for it.
		if !m.ReachedReady {
			return f.beginTeardown(ctx, m, Synced, now)
		}
		if err := f.cfg.Syncer.Sync(ctx, m.ID, m.Name); err != nil {
			// The cap can abandon a save, but only after the attempts are spent --
			// and it says so, because this is data loss.
			if m.Failures+1 >= f.cfg.SyncAttempts {
				return f.beginTeardown(ctx, m, Abandoned, now)
			}
			return fail(Syncing, err)
		}
		return f.beginTeardown(ctx, m, Synced, now)

	case Terminating:
		_, err := f.cfg.Cloud.Describe(ctx, m.ID)
		machineGone := errors.Is(err, ErrNoSuchMachine)
		if err != nil && !machineGone {
			return TransitionEvent{}, fmt.Errorf("firefly: describing %s: %w", m.ID, err)
		}

		nodeExists, err := f.cfg.Network.NodeExists(ctx, m.Name)
		if err != nil {
			return TransitionEvent{}, fmt.Errorf("firefly: asking about the node for %s: %w", m.Name, err)
		}

		switch {
		case machineGone && !nodeExists:
			return move(Confirmed)

		case machineGone && nodeExists:
			// The machine half is done and the node record outlived it. Repair it,
			// rather than waiting for a record that will never clear itself -- a
			// design that only observes here cycles TERMINATING and ERROR forever.
			if err := f.cfg.Network.Forget(ctx, m.Name); err != nil {
				return fail(Terminating, err)
			}
			// Still Terminating; the next tick sees the record gone and confirms.
			return TransitionEvent{Machine: m.ID, From: Terminating, To: Terminating, Trigger: MachineGone}, nil

		case now.Sub(m.Since) >= f.cfg.TerminateDeadline:
			return fail(Terminating, errors.New("the delete was not confirmed by its deadline"))

		default:
			return TransitionEvent{}, nil
		}

	case Error:
		if now.Before(m.RetryAt) {
			return TransitionEvent{}, nil
		}
		return retry(m, now)

	case Absent:
		return TransitionEvent{}, nil
	}

	return TransitionEvent{}, fmt.Errorf("firefly: %s is in unknown state %q", m.ID, m.State)
}

// retry returns an Error to where it came from, clearing the Error record but not
// the consecutive-failure count: a machine that fails three times in a row has
// failed three times, whether or not it was allowed a pause between them.
func retry(m *Machine, now time.Time) (TransitionEvent, error) {
	to, err := Next(Error, RetryDue, m.Origin)
	if err != nil {
		return TransitionEvent{}, err
	}
	ev := TransitionEvent{Machine: m.ID, From: Error, To: to, Trigger: RetryDue}
	m.State = to
	m.Since = now
	m.Origin = ""
	m.Attempt = 0
	m.LastErr = ""
	m.RetryAt = time.Time{}
	return ev, nil
}

// beginTeardown enters Terminating and asks the provider to destroy the machine.
//
// The delete is issued here rather than in the Terminating case, because
// Terminating means "a delete was issued" -- the state is a record of an action,
// and a step that entered it without acting would be a state that lies.
func (f *Firefly) beginTeardown(ctx context.Context, m *Machine, on Trigger, now time.Time) (TransitionEvent, error) {
	to, err := Next(m.State, on, m.Origin)
	if err != nil {
		return TransitionEvent{}, err
	}
	if to != Terminating {
		return TransitionEvent{}, fmt.Errorf("firefly: teardown from %s went to %s", m.State, to)
	}

	ev := TransitionEvent{Machine: m.ID, From: m.State, To: to, Trigger: on}
	m.State = to
	m.Since = now
	m.Origin = ""
	m.Attempt = 0
	m.LastErr = ""
	m.RetryAt = time.Time{}
	m.Failures = 0

	// A 404 is success: the caller cannot tell a repeat delete from a clean one.
	if err := f.cfg.Cloud.Delete(ctx, m.ID); err != nil && !errors.Is(err, ErrNoSuchMachine) {
		return ev, fmt.Errorf("firefly: deleting %s: %w", m.ID, err)
	}
	return ev, nil
}

// triggerFor names the trigger that failed on the way into Error, for the log.
func triggerFor(from State) Trigger {
	switch from {
	case Provisioning:
		return Stalled
	case Syncing:
		return SyncFailed
	case Terminating:
		return Unconfirmed
	default:
		return ""
	}
}

// retryDelay is the backoff for the nth consecutive failure.
//
// Linear rather than exponential, deliberately: the failures this retries are a
// provider blip, and an exponential curve mostly encodes a guess about how long
// a blip lasts. RetryMax is the part that matters, because it is what stops a
// machine from retrying past its own deadline.
func (f *Firefly) retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Duration(attempt) * f.cfg.RetryBase
	if d > f.cfg.RetryMax {
		d = f.cfg.RetryMax
	}
	return d
}

// liveLeases counts the leases on one machine that are live at now.
func liveLeases(doc Document, id MachineID, now time.Time) int {
	n := 0
	for _, l := range doc.Leases {
		if l.Machine == id && l.Live(now) {
			n++
		}
	}
	return n
}
