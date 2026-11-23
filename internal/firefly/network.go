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
//
// Every method is keyed on the machine's *name*, not its provider id. A mesh
// knows devices by name and the provider knows machines by an id it mints at
// creation; asking a tailnet about "hetzner:42" is a question it cannot answer.
// So firefly mints the name at provisioning time, records it on the Machine, and
// the provider is told the same name at creation — which is why a node needs no
// cooperation from the machine to be matchable. See docs/firefly.md, "What a
// node is called".
type Network interface {
	// Joined reports whether a node exists for the named machine *and* that node is
	// usable right now.
	//
	// It is the second half of READY. A machine that answers its metadata service
	// and never joins is not ready, and a node that lingers after its machine is
	// gone is not joined, however present the record is.
	//
	// What "usable" means is the network's own vocabulary, and it is not the
	// provider's: a tailnet device is usable when it is registered, authorized, and
	// talking to the control plane. A network cannot see whether a server is
	// running, which is why Tick asks both interfaces and trusts neither alone.
	Joined(ctx context.Context, name string) (bool, error)

	// NodeExists reports whether a node record exists for the named machine,
	// whether or not the machine does.
	//
	// This is the second half of deletion: a machine can be gone from the provider
	// and still be a member of the network. Only when this returns false, after a
	// Forget if one was needed, may a machine reach ABSENT.
	NodeExists(ctx context.Context, name string) (bool, error)

	// Forget removes a node record.
	//
	// Observation alone can detect a lingering node forever without resolving it,
	// which is why this is here: the alternative is a machine that cycles between
	// TERMINATING and ERROR while a human watches a bill.
	//
	// It is idempotent. Forgetting something that is already gone returns nil,
	// because the caller cannot tell that from success and should not try.
	Forget(ctx context.Context, name string) error
}
