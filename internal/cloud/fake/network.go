package fake

import (
	"context"
	"fmt"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// Network is the other half of the fake: a mesh whose node records outlive the
// machines that made them.
//
// It shares the *Cloud's clock and machine records rather than keeping its own,
// and that is a decision rather than convenience. Two clocks could not express
// "the machine is deleted but its node record is still there", which is the exact
// interleaving the deletion rule exists for; with one clock, advancing time moves
// both halves together the way they move in the world.
type Network struct {
	cloud *Cloud

	// joinDelay is how long after a machine is running its node joins. Simulated,
	// like every other duration here.
	joinDelay time.Duration

	// metadataMissing models a machine whose metadata service never answered: it
	// runs, and no node ever appears.
	metadataMissing bool

	// forgetRefuses makes the next n forgets fail before one succeeds.
	forgetRefuses int

	// forgotten holds the names whose records were removed. It is the only mutable
	// state in this half: everything else is derived from the cloud's records.
	//
	// Names, not ids, because that is what a tailnet knows: the fake used to derive
	// the node from the machine record it already held, which hid the fact that
	// nothing mapped a provider id to a device. See docs/firefly.md, "What a node
	// is called".
	forgotten map[string]bool

	joinedCalls int
	existsCalls int
	forgetCalls int

	// asked is every name these methods were handed, in order.
	//
	// The fake resolves a name against the cloud's own records, so it cannot notice
	// a name that does not match the one the provider was told -- it would just
	// answer "no such machine" for a machine that is running. A test compares this
	// against Cloud.Specs(), which is the one join nothing else in the design makes
	// checkable.
	asked []string
}

// NewNetwork returns the network half, driven by the same clock as c.
func NewNetwork(c *Cloud) *Network {
	return &Network{
		cloud:     c,
		forgotten: make(map[string]bool),
	}
}

// The fake must satisfy the interface firefly depends on.
var _ firefly.Network = (*Network)(nil)

// --- knobs ---------------------------------------------------------------

// JoinDelay sets how long after a machine is running its node joins.
func (n *Network) JoinDelay(d time.Duration) *Network {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	n.joinDelay = d
	return n
}

// MetadataMissing makes a machine that runs and never joins: its metadata service
// never came up, so the mesh agent never started. Firefly must never call it
// ready, and must eventually destroy it.
func (n *Network) MetadataMissing() *Network {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	n.metadataMissing = true
	return n
}

// ForgetRefuses makes the next n forgets fail before one succeeds.
func (n *Network) ForgetRefuses(n2 int) *Network {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	n.forgetRefuses = n2
	return n
}

// --- provider ------------------------------------------------------------

// Joined reports a node that exists *and* whose machine is usable right now.
func (n *Network) Joined(ctx context.Context, name string) (bool, error) {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	n.joinedCalls++
	n.asked = append(n.asked, name)

	if err := ctx.Err(); err != nil {
		return false, err
	}
	return n.nodeExists(name) && n.machineUsable(name), nil
}

// NodeExists reports a node record, whether or not its machine is still there.
func (n *Network) NodeExists(ctx context.Context, name string) (bool, error) {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	n.existsCalls++
	n.asked = append(n.asked, name)

	if err := ctx.Err(); err != nil {
		return false, err
	}
	return n.nodeExists(name), nil
}

// Forget removes the node record. A node that is already gone is not an error.
func (n *Network) Forget(ctx context.Context, name string) error {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	n.forgetCalls++
	n.asked = append(n.asked, name)

	if err := ctx.Err(); err != nil {
		return err
	}
	if n.forgetRefuses > 0 {
		n.forgetRefuses--
		return fmt.Errorf("fake: the network refused the forget (%d refusals remaining)", n.forgetRefuses)
	}
	// The tombstone is written whether or not a record existed yet. Without that,
	// forgetting a machine that has not joined would do nothing, and a later
	// NodeExists would report a node -- a repair that silently fails to repair.
	n.forgotten[name] = true
	return nil
}

// Asked reports every name the three methods were handed, in order.
func (n *Network) Asked() []string {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	return append([]string(nil), n.asked...)
}

// Calls reports how many times each network method has been called.
func (n *Network) Calls() (joined, exists, forgets int) {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	return n.joinedCalls, n.existsCalls, n.forgetCalls
}

// --- internals ---
//
// Everything below is called with the cloud's lock held, and nothing here
// mutates. That is deliberate: the previous version cached a join instant on
// first observation, which made the answer depend on when it was asked. A node
// that exists is a historical fact about the machine's boot and delete times,
// so it is derived fresh every time and cannot be order-dependent.

// joinInstant returns when this machine's node appears, and whether it ever does.
//
// The node appears at readyAt+joinDelay, which means it only ever exists if the
// machine was still there at that instant: deleting before boot, or during the
// join delay, means no agent ever ran.
func (n *Network) joinInstant(name string) (time.Time, bool) {
	if n.metadataMissing {
		return time.Time{}, false
	}
	m, ok := n.cloud.byName(name)
	if !ok || m.readyAt.IsZero() {
		return time.Time{}, false
	}
	at := m.readyAt.Add(n.joinDelay)
	// Whether it ever joined depends on when it stopped existing, not on which
	// path reaped it: a normal delete completes at deleteAt, and a 404 delete is
	// immediate. Comparing against the instant covers both; comparing against
	// deleteAt alone missed the 404 path entirely.
	if !m.goneAt.IsZero() && !m.goneAt.After(at) {
		return time.Time{}, false
	}
	return at, true
}

// nodeExists reports whether a record is present right now: it joined, its time
// has come, and it has not been forgotten. It says nothing about the machine,
// which may be long gone -- that is the point of the method.
func (n *Network) nodeExists(name string) bool {
	if n.forgotten[name] {
		return false
	}
	at, ok := n.joinInstant(name)
	if !ok {
		return false
	}
	return !n.cloud.now.Before(at)
}

// machineUsable reports whether the machine exists and is running. A node that
// outlives its machine is present but not joined, which is the distinction the
// two methods exist to draw.
func (n *Network) machineUsable(name string) bool {
	c := n.cloud
	m, ok := c.byName(name)
	if !ok || c.expired(m) {
		return false
	}
	return c.status(m) == firefly.ProviderRunning
}
