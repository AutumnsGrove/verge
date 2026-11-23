package firefly

import (
	"errors"
	"fmt"
	"time"
)

// MachineID is the provider's identifier for a machine, kept opaque. Firefly
// stores it and compares it; it never parses it, because the format is the
// provider's business and a parsed identifier is a coupling.
type MachineID string

// Machine is the durable record of one machine.
//
// Everything here is either a fact firefly observed or a deadline the caller
// supplied at creation. It deliberately holds no policy: no drain window, no
// grace period, no budget. Those are the caller's and are passed in, so that a
// stored record read back after a restart cannot silently disagree with the
// configuration the process is running under.
type Machine struct {
	ID MachineID

	// Name is the machine's name: the identifier the *network* knows it by, and
	// the one the provider was told at creation. It is not derived from ID and
	// not recoverable from the provider's id alone, which is why it is stored.
	//
	// Minted by Provision from a counter in the document, so it is unique, stable
	// across restarts, and readable in a test without a random source. A name has
	// to be unique per project anyway, the provider requires one, and it is the
	// only identifier a tailnet can be asked about — see docs/firefly.md, "What a
	// node is called".
	Name string

	// State is where the machine is. Origin is the state it was in before
	// entering Error, and is empty otherwise; it is what chooses between the
	// ERROR rows of the transition table.
	State  State
	Origin State

	// CreatedAt and Deadline bracket the machine's existence. Deadline is the
	// caller's MaxLifetime resolved to an instant at creation, which makes it
	// comparable across restarts; a duration would not be.
	CreatedAt time.Time
	Deadline  time.Time

	// Since is when the machine entered State, for reporting. It is not used to
	// decide anything — every decision is made from an injected now — because a
	// stored duration that ages in the background is a sleeper bug.
	Since time.Time

	// ReachedReady records that this machine was once Ready, which is what tells
	// Syncing whether there is anything to save. A machine that never booted has
	// nothing on it, and its sync is a successful no-op -- without that, a boot
	// that never answers could never be destroyed.
	ReachedReady bool

	// Failures counts consecutive failures, and is cleared only by a success. It
	// is separate from Attempt because the cap needs a number that survives a
	// retry: a per-error attempt counter resets every time the machine is allowed
	// to try again, which would make an exhausted-retries abandonment unreachable.
	Failures int

	// Attempt, LastErr, and RetryAt are the Error record. Attempt is zero
	// outside Error, and RetryAt is zero until a retry is scheduled.
	Attempt int
	LastErr string
	RetryAt time.Time
}

// Document is everything firefly must remember to keep being right after a
// restart: the machines it owns and the leases that keep them alive.
//
// It is one document for all machines rather than a shard each. A shard would
// survive a partial write better and would make the orphan sweep noticeably
// harder, and the sweep runs on every tick while a partial write needs a crash
// at a specific instant. Revisit when there is a test with two machines and
// something worth losing; until then the simpler shape is the honest one.
type Document struct {
	Version int

	// NextLeaseNumber is the counter lease ids are minted from. It lives in the
	// document rather than in memory so that a restart continues the sequence
	// instead of reissuing an id a caller still holds -- and so that the ids are
	// stable enough to appear in a test without a random source.
	NextLeaseNumber uint64

	// NextMachineNumber is the same idea for machine names, which have to be
	// unique per project and are the key the network is asked about. It shares the
	// document's fate rather than the process's, so a restart continues the
	// sequence instead of minting a name a live machine already has.
	//
	// A lost document restarts the sequence at zero, which is why Provision skips
	// a name that is already in the document: after recovering by adoption, the
	// next mint must not collide with a machine that is still running.
	NextMachineNumber uint64

	Machines []Machine
	Leases   []Lease
}

// DocumentVersion is the schema version. It exists so that a document written
// by a future binary is refused rather than half-understood.
//
// Version 2 added Machine.Name and Document.NextMachineNumber. There is no
// migration and no version 1 document in the world: the network was keyed on the
// provider id when 1 was written, so a 1 is not a document this binary can act
// on, and refusing it is more honest than adopting machines whose nodes nothing
// can name.
const DocumentVersion = 2

// Lease errors that a document can be wrong about. They are distinct from the
// lease arithmetic errors in lease.go: those are about a caller's request, these
// are about stored state that has already gone wrong.
var (
	// ErrNoDocument means nothing has been saved yet. It is the ordinary state
	// of a fresh deployment and must not be treated as an error by callers.
	ErrNoDocument = errors.New("firefly: no state document yet")

	// ErrDocumentVersion means the stored schema is not one this binary knows.
	ErrDocumentVersion = errors.New("firefly: unsupported state document version")
)

// Validate reports the first thing wrong with a document, or nil.
//
// It runs on Save and on Load, because the two failures it prevents are
// different and both expensive: saving a broken document loses the good one,
// and loading one silently makes every later decision wrong. A document that
// has been through Validate can be trusted to describe a world that could
// exist.
func (d Document) Validate() error {
	if d.Version != DocumentVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrDocumentVersion, d.Version, DocumentVersion)
	}

	origins := errorOrigins()

	byID := make(map[MachineID]struct{}, len(d.Machines))
	byName := make(map[string]MachineID, len(d.Machines))
	for _, m := range d.Machines {
		if m.ID == "" {
			return errors.New("firefly: machine with empty id")
		}
		if _, dup := byID[m.ID]; dup {
			return fmt.Errorf("firefly: duplicate machine %q", m.ID)
		}
		byID[m.ID] = struct{}{}

		// The name is what the network is asked about, so a machine without one
		// could never become Ready and could never be confirmed gone. Two machines
		// sharing one name is worse: they would share a node record, and deleting
		// either would look like deleting both.
		if m.Name == "" {
			return fmt.Errorf("firefly: machine %q has no name", m.ID)
		}
		if other, dup := byName[m.Name]; dup {
			return fmt.Errorf("firefly: machines %q and %q share the name %q", other, m.ID, m.Name)
		}
		byName[m.Name] = m.ID

		// The state is persisted, so it is checked before it is trusted to select
		// a branch below. Without this, an unknown state falls through to the
		// default arm and is accepted, and the machine then matches no row.
		if !m.State.Valid() {
			return fmt.Errorf("firefly: machine %q is in unknown state %q", m.ID, m.State)
		}

		if m.CreatedAt.IsZero() {
			return fmt.Errorf("firefly: machine %q has no CreatedAt", m.ID)
		}
		if m.Deadline.IsZero() || !m.Deadline.After(m.CreatedAt) {
			return fmt.Errorf("firefly: machine %q has deadline %s, which is not after its creation",
				m.ID, m.Deadline)
		}
		if m.Since.IsZero() || m.Since.Before(m.CreatedAt) {
			return fmt.Errorf("firefly: machine %q entered %s at %s, before it was created",
				m.ID, m.State, m.Since)
		}

		switch m.State {
		case Error:
			// An Error must say where it came from, and only the origins the
			// transition table has a RetryDue row for are legal. Reading the
			// table rather than listing them here is what keeps the two from
			// drifting.
			if _, ok := origins[m.Origin]; !ok {
				return fmt.Errorf("firefly: machine %q is in ERROR with origin %q, which has no retry row",
					m.ID, m.Origin)
			}
			if m.Attempt < 1 {
				return fmt.Errorf("firefly: machine %q is in ERROR with attempt %d", m.ID, m.Attempt)
			}
			if m.LastErr == "" {
				return fmt.Errorf("firefly: machine %q is in ERROR with no LastErr", m.ID)
			}
		default:
			// Outside Error there is nothing to remember: a stale origin or
			// retry time here is how a machine ends up retrying a step it is
			// no longer doing.
			if m.Origin != "" {
				return fmt.Errorf("firefly: machine %q is in %s but carries origin %q", m.ID, m.State, m.Origin)
			}
			if m.Attempt != 0 {
				return fmt.Errorf("firefly: machine %q is in %s but carries attempt %d", m.ID, m.State, m.Attempt)
			}
			if !m.RetryAt.IsZero() {
				return fmt.Errorf("firefly: machine %q is in %s but has a retry scheduled", m.ID, m.State)
			}
		}
	}

	leaseIDs := make(map[LeaseID]struct{}, len(d.Leases))
	for _, l := range d.Leases {
		if l.ID == "" {
			return errors.New("firefly: lease with empty id")
		}
		if _, dup := leaseIDs[l.ID]; dup {
			return fmt.Errorf("firefly: duplicate lease %q", l.ID)
		}
		leaseIDs[l.ID] = struct{}{}

		// A lease on a machine that is not in the document is the shape of the
		// bug that keeps a machine alive after it has been forgotten.
		if _, ok := byID[l.Machine]; !ok {
			return fmt.Errorf("firefly: lease %q refers to machine %q, which is not in the document",
				l.ID, l.Machine)
		}
		if !l.Kind.Valid() {
			return fmt.Errorf("firefly: lease %q has kind %q", l.ID, l.Kind)
		}
		if l.TTL <= 0 {
			return fmt.Errorf("firefly: lease %q has ttl %s", l.ID, l.TTL)
		}
		if l.Start.IsZero() {
			return fmt.Errorf("firefly: lease %q has no Start", l.ID)
		}
		if l.Holder == "" {
			return fmt.Errorf("firefly: lease %q has no holder", l.ID)
		}
	}
	return nil
}

// Clone returns a document whose slices do not alias the receiver's.
//
// Stores hand it out and take it in, and callers hold a document for the length
// of a tick. Without this, one goroutine's append is another's corrupt state,
// and the race detector finds it only if a test happens to interleave.
func (d Document) Clone() Document {
	out := Document{Version: d.Version, NextLeaseNumber: d.NextLeaseNumber, NextMachineNumber: d.NextMachineNumber}
	if d.Machines != nil {
		out.Machines = make([]Machine, len(d.Machines))
		copy(out.Machines, d.Machines)
	}
	if d.Leases != nil {
		out.Leases = make([]Lease, len(d.Leases))
		copy(out.Leases, d.Leases)
	}
	return out
}

// Machine returns the named machine and whether it is in the document.
func (d Document) Machine(id MachineID) (Machine, bool) {
	for _, m := range d.Machines {
		if m.ID == id {
			return m, true
		}
	}
	return Machine{}, false
}

// nameTaken reports whether any machine in the document already has name.
//
// It is what keeps the minted sequence from colliding with a machine that
// survived a lost document by being adopted: the counter restarts at zero, the
// adopted machines keep the names they were created with, and the next mint has to
// walk past them.
func (d Document) nameTaken(name string) bool {
	for _, m := range d.Machines {
		if m.Name == name {
			return true
		}
	}
	return false
}

// LeasesFor returns every lease held on a machine, live or lapsed.
//
// Lapsed leases are included because the caller usually wants to prune them, and
// a function that silently filtered would make pruning impossible.
func (d Document) LeasesFor(id MachineID) []Lease {
	var out []Lease
	for _, l := range d.Leases {
		if l.Machine == id {
			out = append(out, l)
		}
	}
	return out
}

// Pruned returns a copy with every lease that has lapsed at now removed, and
// how many were removed.
//
// Pruning is what stops the document growing without bound: a session that lasts
// a day renews one lease and leaves hundreds of expired records behind. The
// leases that remain are inert either way — Live decides liveness, not presence
// — so this is housekeeping, not correctness, and can lag a tick without harm.
func (d Document) Pruned(now time.Time) (Document, int) {
	out := d.Clone()
	kept := out.Leases[:0]
	removed := 0
	for _, l := range out.Leases {
		if l.Live(now) {
			kept = append(kept, l)
			continue
		}
		removed++
	}
	out.Leases = kept
	return out, removed
}
