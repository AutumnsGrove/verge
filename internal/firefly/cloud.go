package firefly

import (
	"context"
	"errors"
	"time"
)

// ProviderStatus is the provider's own vocabulary for what a machine is doing.
//
// It is deliberately not State. The provider reports facts; firefly decides
// states. Collapsing the two would hide the two things that actually go wrong: a
// provider saying running to a machine that never joined, and a provider saying
// deleted to a node record that still exists.
type ProviderStatus string

const (
	ProviderCreated  ProviderStatus = "created"
	ProviderStarting ProviderStatus = "starting"
	ProviderRunning  ProviderStatus = "running"
	ProviderStopping ProviderStatus = "stopping"
)

// Valid reports whether s is a status this build understands. A provider that
// grows a status is a reason to upgrade, not a reason to guess.
func (s ProviderStatus) Valid() bool {
	switch s {
	case ProviderCreated, ProviderStarting, ProviderRunning, ProviderStopping:
		return true
	default:
		return false
	}
}

// Spec is what a caller asks the provider to create.
//
// It carries no provider credential. No path through this package can put a key
// that can spend money onto a machine; the only secret here is a single-use,
// short-lived network enrolment key, which grants membership of a private
// network and nothing else. That distinction is the reason the proxy exists.
type Spec struct {
	// CloudInit is the rendered boot script. Rendering is the caller's job,
	// because the template is policy; firefly only carries the bytes.
	//
	// The network enrolment key belongs in here, not in a field of its own. An
	// earlier version had `NetworkKey` on Spec as well, which nothing read: the
	// caller renders the key into the script, so a second copy would be a second
	// source of truth for a secret.
	CloudInit []byte

	Image  string
	Region string
	Size   string

	// Labels are how firefly finds its own machines again after a restart, and
	// how the orphan sweep tells its machines from someone else's.
	Labels map[string]string

	// Name is the machine's name, which is also what the network knows it by.
	//
	// Callers leave it empty: Provision mints one (see "What a node is called" in
	// docs/firefly.md) and sets it here, because the provider requires a name at
	// creation and the mesh has no other way to be asked about a machine. A
	// caller-supplied name is honoured, and refused if it is already taken.
	Name string
}

// CloudMachine is the provider's view of a machine.
//
// It says nothing about sessions, leases, or whether the machine is usable. A
// machine is usable when this reports Running *and* the network says it joined;
// each half has its own way of lying, which is why neither is trusted alone.
type CloudMachine struct {
	ID     MachineID
	Status ProviderStatus

	// Name is the machine's name as the provider reports it, and is what the
	// network is asked about. It is carried here for one reason: adoption. A
	// machine found in a listing that the document does not know cannot be asked
	// about by name unless the listing says what the name is, and a name firefly
	// invented for an adopted machine would never match the node it actually has.
	Name string

	Labels    map[string]string
	PublicIP  string
	CreatedAt time.Time
}

// Cloud is the provider, reduced to the four things a lifecycle needs.
//
// Every method is a request the loop makes, never a callback: nothing outside
// the loop is allowed to own the loop's timing, because a pushed event cannot be
// played back from a table in a test.
type Cloud interface {
	// Create asks for a machine and returns its identifier. An error means no
	// machine was created, which is why a failed create is ABSENT rather than
	// ERROR — ERROR means the machine still exists.
	Create(ctx context.Context, spec Spec) (MachineID, error)

	// Describe reports one machine. It returns ErrNoSuchMachine when the
	// provider does not have it, which is the only evidence of absence firefly
	// accepts.
	//
	// Describe, not List. A listing is eventually consistent, so a machine
	// missing from one is not a machine that is gone; deriving absence from a
	// listing is how a design provisions a second machine beside a live one and
	// then pays for both.
	Describe(ctx context.Context, id MachineID) (CloudMachine, error)

	// List reports every machine carrying label. Presence is evidence of
	// existence; absence is not evidence of the reverse.
	List(ctx context.Context, label string) ([]CloudMachine, error)

	// Delete asks the provider to destroy a machine. It is idempotent: a machine
	// that is already gone returns nil, because the caller cannot distinguish
	// that from a clean delete and should not try.
	Delete(ctx context.Context, id MachineID) error
}

// ErrNoSuchMachine means the provider does not have a machine with that id.
var ErrNoSuchMachine = errors.New("firefly: no such machine")
