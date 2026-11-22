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

	nodes map[firefly.MachineID]*node

	joinedCalls int
	existsCalls int
	forgetCalls int
}

type node struct {
	id        firefly.MachineID
	joinedAt  time.Time // zero means no node ever appeared
	forgotten bool
}

// NewNetwork returns the network half, driven by the same clock as c.
func NewNetwork(c *Cloud) *Network {
	return &Network{
		cloud: c,
		nodes: make(map[firefly.MachineID]*node),
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
func (n *Network) Joined(ctx context.Context, id firefly.MachineID) (bool, error) {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	n.joinedCalls++

	if err := ctx.Err(); err != nil {
		return false, err
	}
	return n.joined(id) && n.machineUsable(id), nil
}

// NodeExists reports a node record, whether or not its machine is still there.
func (n *Network) NodeExists(ctx context.Context, id firefly.MachineID) (bool, error) {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	n.existsCalls++

	if err := ctx.Err(); err != nil {
		return false, err
	}
	return n.joined(id), nil
}

// Forget removes the node record. A node that is already gone is not an error.
func (n *Network) Forget(ctx context.Context, id firefly.MachineID) error {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	n.forgetCalls++

	if err := ctx.Err(); err != nil {
		return err
	}
	if n.forgetRefuses > 0 {
		n.forgetRefuses--
		return fmt.Errorf("fake: the network refused the forget (%d refusals remaining)", n.forgetRefuses)
	}
	// The record is created if it is not there yet. Without this, forgetting a
	// machine that has not joined does nothing, and the next NodeExists call
	// synthesises a fresh node -- a repair that silently fails to repair.
	rec, ok := n.nodes[id]
	if !ok {
		rec = &node{id: id}
		n.nodes[id] = rec
	}
	rec.forgotten = true
	return nil
}

// Calls reports how many times each network method has been called.
func (n *Network) Calls() (joined, exists, forgets int) {
	n.cloud.mu.Lock()
	defer n.cloud.mu.Unlock()
	return n.joinedCalls, n.existsCalls, n.forgetCalls
}

// --- internals -----------------------------------------------------------
//
// Everything below is called with the cloud's lock held.

// observe returns the node record for id, creating it and settling whether it
// joined. It is the only place joinedAt is written, so the rule is in one place.
func (n *Network) observe(id firefly.MachineID) *node {
	rec, ok := n.nodes[id]
	if !ok {
		rec = &node{id: id}
		n.nodes[id] = rec
	}
	if rec.forgotten || !rec.joinedAt.IsZero() || n.metadataMissing {
		return rec
	}

	c := n.cloud
	m, ok := c.mach[id]
	if !ok {
		return rec
	}
	// A machine that never finished cloud-init never runs a mesh agent.
	if m.readyAt.IsZero() || c.now.Before(m.readyAt) {
		return rec
	}

	// A node joins at readyAt+joinDelay, so it only ever exists if the machine was
	// still there at that instant. Deleting before boot -- or during the join
	// delay -- means no agent ever ran, so no record appears.
	//
	// Without this, a machine killed mid-boot grew a node once the clock passed
	// its boot time, and TERMINATING was then made to Forget a node that never
	// existed. It is the fake lying in the one direction the deletion rule cares
	// about.
	joinAt := m.readyAt.Add(n.joinDelay)
	if !m.deleteAt.IsZero() && !m.deleteAt.After(joinAt) {
		return rec
	}
	if c.expired(m) {
		return rec
	}
	rec.joinedAt = joinAt
	return rec
}

// joined reports whether a node record exists and its join has completed.
func (n *Network) joined(id firefly.MachineID) bool {
	rec := n.observe(id)
	if rec.forgotten || rec.joinedAt.IsZero() {
		return false
	}
	return !n.cloud.now.Before(rec.joinedAt)
}

// machineUsable reports whether the machine exists and is running. A node that
// outlives its machine is present but not joined, which is the distinction the
// two methods exist to draw.
func (n *Network) machineUsable(id firefly.MachineID) bool {
	c := n.cloud
	m, ok := c.mach[id]
	if !ok || c.expired(m) {
		return false
	}
	return c.status(m) == firefly.ProviderRunning
}
