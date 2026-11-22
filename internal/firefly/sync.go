package firefly

import "context"

// Syncer saves a machine's own state off the machine.
//
// It is deliberately not Store. Store is firefly's bookkeeping -- which machines
// exist and who holds them -- and losing it costs a reconcile. This is the
// work: the session, the workspace, the uncommitted patch. Losing it costs a
// person their afternoon, which is why the ordering property is about this and
// not about the document.
//
// The gap is worth recording, because the design went three increments without
// it: Cloud, Network, and Store were all specified and the one dependency the
// ordering guarantee actually depends on was missing. A guarantee whose work has
// no method is a guarantee about nothing.
type Syncer interface {
	// Sync saves the machine's state. It returns nil when there was nothing to
	// save, which is the ordinary case for a machine that never became ready.
	//
	// The implementation decides what "saved" means; firefly decides when it must
	// have happened.
	Sync(ctx context.Context, id MachineID) error
}
