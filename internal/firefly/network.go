package firefly

import "context"

// Network is the private mesh the machines join, reduced to the three things a
// lifecycle needs: is it there, is it usable, and make it go away.
//
// It exists because readiness and deletion are each two facts. The provider can
// say a machine is running while nothing has joined, and it can say a machine is
// deleted while its node record lingers — and a tailnet accumulates those
// quietly. One interface per fact keeps each side's failure honest.
//
// Like Cloud, everything here is a request the loop makes. Nothing is pushed.
type Network interface {
	// Joined reports whether a node exists for the machine *and* the machine is
	// usable right now: present, and running.
	//
	// It is the second half of READY. A machine that answers its metadata service
	// and never joins is not ready, and a node that lingers after its machine is
	// gone is not joined, however present the record is.
	Joined(ctx context.Context, id MachineID) (bool, error)

	// NodeExists reports whether a node record exists for the machine, whether or
	// not the machine does.
	//
	// This is the second half of deletion: a machine can be gone from the provider
	// and still be a member of the network. Only when this returns false, after a
	// Forget if one was needed, may a machine reach ABSENT.
	NodeExists(ctx context.Context, id MachineID) (bool, error)

	// Forget removes a node record.
	//
	// Observation alone can detect a lingering node forever without resolving it,
	// which is why this is here: the alternative is a machine that cycles between
	// TERMINATING and ERROR while a human watches a bill.
	//
	// It is idempotent. Forgetting something that is already gone returns nil,
	// because the caller cannot tell that from success and should not try.
	Forget(ctx context.Context, id MachineID) error
}
