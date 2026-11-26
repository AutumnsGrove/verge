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
	//
	// The name is passed because a transport needs it and nothing else has it: the
	// tailnet is keyed on the machine's name rather than the provider's id, so a
	// fetcher handed only the id would have to ask the provider -- the one system
	// that may be the reason the save is needed -- to translate. firefly minted the
	// name and stored it, so it says it.
	Sync(ctx context.Context, id MachineID, name string) error
}
