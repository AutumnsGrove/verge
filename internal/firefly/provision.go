package firefly

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrProvisioningBackoff means a previous create failed recently and the next
// attempt is not due yet.
//
// A create that failed left nothing behind, so this is not an Error in the state
// machine -- the table's ABSENT --CreateFailed--> ABSENT row is what it is. The
// record lives on the request rather than on a machine, because there is no
// machine.
var ErrProvisioningBackoff = errors.New("firefly: provisioning is backing off after a failed create")

// ErrNameTaken means the caller asked for a name that a machine in the document
// already has.
//
// It is refused before the machine is created, not after: two machines sharing a
// name share one node record, so the network could not tell them apart -- and a
// create that succeeded would have to be deleted again to stay consistent.
var ErrNameTaken = errors.New("firefly: a machine with that name already exists")

// MintName reports the name the next Provision would take, without recording anything.
//
// It exists because the boot script has to carry the name and Provision mints it: the
// caller that renders cloud-init needs the name before the machine exists. This is a
// read, and a hint rather than a reservation -- two callers that ask before either
// provisions get the same answer, and the second Provision fails with ErrNameTaken
// rather than creating a machine with a name that is already on the network.
func (f *Firefly) MintName(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.load(ctx); err != nil {
		return "", err
	}
	return f.mintName(&f.doc), nil
}

// Provision creates a machine and records it as PROVISIONING.
//
// It is the ABSENT --Need--> PROVISIONING row. Placement has already happened:
// the caller decided it needs a machine, so firefly does not ask why, and does
// not decide how many.
//
// The returned id is set even when an error is returned if the machine was
// created but could not be recorded. That is deliberate: the caller has to know
// there is a machine to clean up, and the next Tick adopts it as an orphan.
//
// A failed create still spends its name. The provider enforces uniqueness across
// the whole project while the document only knows this deployment's machines, so a
// name can be refused for a reason nothing here can see — and a counter that only
// moved on success would ask for that name again on every retry, forever.
func (f *Firefly) Provision(ctx context.Context, spec Spec, now time.Time) (MachineID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.load(ctx); err != nil {
		return "", err
	}
	if now.Before(f.createRetryAt) {
		return "", fmt.Errorf("%w: next attempt after %s (%v)",
			ErrProvisioningBackoff, f.createRetryAt.Format(time.RFC3339), f.createErr)
	}

	// The name is decided here, before Create, because the provider has to be told
	// it at creation and the network is keyed on it afterwards. The counter and the
	// machines it must not collide with are both in the document, so the mint works
	// on the copy that will be saved rather than on the live one.
	next := f.doc.Clone()
	name := spec.Name
	if name == "" {
		name = f.mintName(&next)
	} else if next.nameTaken(name) {
		return "", fmt.Errorf("%w: %q", ErrNameTaken, name)
	}
	spec.Name = name

	// The label is firefly's, not the caller's. A machine created without it is
	// invisible to List, which means the orphan sweep cannot find it and the
	// document cannot be rebuilt from the provider after a lost write -- so it
	// would bill forever, unmanaged. Overwriting rather than merging is the point:
	// a caller that passes the wrong label must not be able to create a machine
	// nobody can see. New has already parsed the label, so this cannot fail here.
	labels := make(map[string]string, len(spec.Labels)+1)
	for k, v := range spec.Labels {
		labels[k] = v
	}
	labels[f.labelKey] = f.labelValue
	spec.Labels = labels

	id, err := f.cfg.Cloud.Create(ctx, spec)
	if err != nil {
		f.createFailures++
		f.createRetryAt = now.Add(f.retryDelay(f.createFailures))
		f.createErr = err

		// The name is spent even though the machine is not.
		//
		// A create can fail because the provider refuses a name this deployment
		// cannot see it holds: names are unique across the whole project, while the
		// document and the label-filtered listing only ever know about our own
		// machines. A counter that advanced only on success would mint that same
		// refused name on every attempt, back off forever, and never reach a name it
		// was allowed to use. Advancing here costs a gap in the sequence when the
		// failure was transient instead, and a gap costs nothing: names are for
		// telling machines apart, not for counting them.
		//
		// It is saved as well as advanced, so a restart retries past the refused name
		// rather than rediscovering it. A save that fails here leaves the in-memory
		// counter ahead of the store, which is the same shape as every other failed
		// save in this package: the next attempt in this process is correct, and a
		// restart is bounded by the same backoff.
		f.doc = next
		if saveErr := f.save(ctx, next); saveErr != nil {
			return "", fmt.Errorf("firefly: provisioning: %w", errors.Join(err, saveErr))
		}
		return "", fmt.Errorf("firefly: provisioning: %w", err)
	}
	f.createFailures = 0
	f.createRetryAt = time.Time{}
	f.createErr = nil

	next.Machines = append(next.Machines, Machine{
		ID:        id,
		Name:      name,
		State:     Provisioning,
		CreatedAt: now,
		Deadline:  now.Add(f.cfg.MaxLifetime),
		Since:     now,
	})
	if err := f.save(ctx, next); err != nil {
		// The machine exists and the document does not know. Adoption repairs it
		// on the next tick, which is why this returns the id with the error.
		return id, fmt.Errorf("firefly: recording %s: %w", id, err)
	}
	f.doc = next
	return id, nil
}

// splitLabel splits "key=value".
func splitLabel(label string) (key, value string, ok bool) {
	k, v, found := strings.Cut(label, "=")
	if !found || k == "" || v == "" {
		return "", "", false
	}
	return k, v, true
}
