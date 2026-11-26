// Package placement is the caller firefly was built for.
//
// firefly answers one question: given the leases held, how many machines should exist.
// It deliberately does not know why anyone wants a machine, how many sessions one should
// hold, or what a machine is built from. Those are policy, and this package is where the
// policy lives: it decides when work needs a new machine, hands out leases on the
// machines that already exist, and refuses when a deployment's cap is reached.
//
// Three decisions this file makes, recorded because they are policy rather than
// mechanism:
//
//   - **A machine holds up to Capacity concurrent sessions** (4 by default). The README's
//     "one shared machine by default" is this number, and a deployment that wants a
//     machine per session sets it to 1.
//   - **A deployment runs at most MaxMachines machines** (4 by default). Past it, Place
//     refuses with ErrAtCapacity rather than provisioning: a cap is a bill guard as much
//     as a policy, and a caller that needs more has to say so explicitly.
//   - **Provisioning is asynchronous.** A boot takes minutes, so Place answers "pending"
//     rather than waiting: the caller retries, which is cheap, while a request pinned
//     open for a boot is not.
//
// What this package does not do: it does not render cloud-init, mint enrolment keys, or
// know what an image is. It asks for a Spec by name -- the name firefly will use -- and
// the composition of key and renderer happens where the credentials are.
package placement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// Defaults are the policy a deployment gets if it says nothing.
const (
	DefaultCapacity    = 4
	DefaultMaxMachines = 4
)

// ErrConfig means the placement cannot be built from its configuration.
var ErrConfig = errors.New("placement: invalid configuration")

// ErrAtCapacity means every machine is full and the deployment is at its cap, so the
// caller has to wait for a session to end rather than for a boot.
//
// It is distinct from "pending" because the two call for different behaviour: pending
// means try again shortly, and this means nothing will change until a lease lapses.
var ErrAtCapacity = errors.New("placement: every machine is full and the deployment is at its cap")

// Request is what a caller wants a machine for.
//
// There is no session id and no repository here: firefly is handed a holder name it never
// parses, and placement passes it through. What a session is belongs to the caller.
type Request struct {
	// Holder is opaque: "session:01J8…", "turn:…/7". Required.
	Holder string

	// Kind decides how the lease behaves when it lapses. Required.
	Kind firefly.Kind

	// TTL is how long the lease lasts. Required and positive.
	TTL time.Duration
}

// Config is the policy and the mechanism.
type Config struct {
	// Firefly is the mechanism. Required.
	Firefly *firefly.Firefly

	// NewSpec builds the machine's description from its name. Required.
	//
	// It is a function rather than a value because the name is only known once firefly
	// has minted it, and the boot script has to carry it: the network is keyed on the
	// name, so a script without it produces a machine nothing can ask about.
	NewSpec func(name string) (firefly.Spec, error)

	// Capacity is how many concurrent sessions one machine may hold. Zero means
	// DefaultCapacity; a deployment that wants a machine per session sets 1.
	Capacity int

	// MaxMachines is how many machines the deployment may run at once. Zero means
	// DefaultMaxMachines.
	MaxMachines int
}

// Placement hands out leases and decides when a machine is needed.
type Placement struct {
	f           *firefly.Firefly
	newSpec     func(string) (firefly.Spec, error)
	capacity    int
	maxMachines int
}

// New validates the configuration.
func New(cfg Config) (*Placement, error) {
	if cfg.Firefly == nil {
		return nil, fmt.Errorf("%w: no Firefly", ErrConfig)
	}
	if cfg.NewSpec == nil {
		return nil, fmt.Errorf("%w: no NewSpec", ErrConfig)
	}

	capacity := cfg.Capacity
	if capacity == 0 {
		capacity = DefaultCapacity
	}
	if capacity < 1 {
		return nil, fmt.Errorf("%w: Capacity must be at least 1", ErrConfig)
	}

	maxMachines := cfg.MaxMachines
	if maxMachines == 0 {
		maxMachines = DefaultMaxMachines
	}
	if maxMachines < 1 {
		return nil, fmt.Errorf("%w: MaxMachines must be at least 1", ErrConfig)
	}

	return &Placement{
		f:           cfg.Firefly,
		newSpec:     cfg.NewSpec,
		capacity:    capacity,
		maxMachines: maxMachines,
	}, nil
}

// Capacity and MaxMachines report the effective policy, for a caller that reports it.
func (p *Placement) Capacity() int    { return p.capacity }
func (p *Placement) MaxMachines() int { return p.maxMachines }

// Place puts a lease on a machine, or starts one, or says why it cannot.
//
// The three answers are deliberately different calls to action:
//
//   - (id, true, nil): the lease is held on id, and work can start now.
//   - (id, false, nil): no machine had room, so one is provisioning; the caller retries.
//     It is not an error -- a boot is minutes.
//   - ("", false, err): nothing will change until a session ends (ErrAtCapacity), or
//     something went wrong.
func (p *Placement) Place(ctx context.Context, req Request, now time.Time) (firefly.MachineID, bool, error) {
	if req.Holder == "" {
		return "", false, errors.New("placement: a request needs a holder")
	}
	if !req.Kind.Valid() {
		return "", false, fmt.Errorf("placement: unknown lease kind %q", req.Kind)
	}
	if req.TTL <= 0 {
		return "", false, firefly.ErrLeaseTTL
	}

	snap, err := p.f.Snapshot(ctx, now)
	if err != nil {
		return "", false, fmt.Errorf("placement: reading the snapshot: %w", err)
	}

	// An existing machine first, and it has to be one a lease can actually be taken on: a
	// machine at or past its deadline is about to be destroyed, and work placed there
	// loses a turn to a race the snapshot could have avoided.
	for _, m := range snap.Machines {
		if m.State != firefly.Ready {
			continue
		}
		if !now.Before(m.Deadline) {
			continue
		}
		if m.Live >= p.capacity {
			continue
		}
		if _, err := p.f.Acquire(ctx, m.ID, req.Holder, req.Kind, req.TTL, now); err != nil {
			return "", false, fmt.Errorf("placement: acquiring on %s: %w", m.ID, err)
		}
		return m.ID, true, nil
	}

	// Nothing fits. A machine may already be booting, and the caller waits for that rather
	// than paying for another; the cap is what stops the waiting from becoming a bill.
	if len(snap.Machines) >= p.maxMachines {
		return "", false, ErrAtCapacity
	}

	name, err := p.f.MintName(ctx)
	if err != nil {
		return "", false, fmt.Errorf("placement: minting a name: %w", err)
	}
	spec, err := p.newSpec(name)
	if err != nil {
		return "", false, fmt.Errorf("placement: building a spec for %s: %w", name, err)
	}
	// Set here as well as rendered into the boot script, so Provision and the script cannot
	// disagree about the name.
	spec.Name = name

	id, err := p.f.Provision(ctx, spec, now)
	if err != nil {
		// Backoff is not a failed placement: a create failed a moment ago and the next
		// attempt is not due yet, so this is a "retry shortly" like a pending boot.
		if errors.Is(err, firefly.ErrProvisioningBackoff) {
			return "", false, nil
		}
		return id, false, fmt.Errorf("placement: provisioning %s: %w", name, err)
	}
	return id, false, nil
}
