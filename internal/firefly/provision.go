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

// Provision creates a machine and records it as PROVISIONING.
//
// It is the ABSENT --Need--> PROVISIONING row. Placement has already happened:
// the caller decided it needs a machine, so firefly does not ask why, and does
// not decide how many.
//
// The returned id is set even when an error is returned if the machine was
// created but could not be recorded. That is deliberate: the caller has to know
// there is a machine to clean up, and the next Tick adopts it as an orphan.
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
		return "", fmt.Errorf("firefly: provisioning: %w", err)
	}
	f.createFailures = 0
	f.createRetryAt = time.Time{}
	f.createErr = nil

	next := f.doc.Clone()
	next.Machines = append(next.Machines, Machine{
		ID:        id,
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
