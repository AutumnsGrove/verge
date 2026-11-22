package firefly

import (
	"errors"
	"time"
)

// Kind is what a lease is held for. It exists so that a machine can outlive the
// turn that is currently running on it: a lapsed Turn does not end a Session,
// and a lapsed Session starts the drain window rather than a teardown.
type Kind string

const (
	// Turn covers one agent turn and is renewed by stream activity.
	Turn Kind = "turn"

	// Session covers the session's existence and its ability to be resumed.
	Session Kind = "session"

	// Subscription covers a PR or issue the session is watching.
	Subscription Kind = "subscription"
)

// LeaseID is minted by firefly and is opaque to callers. It is a string rather
// than a number so that it survives a JSON round-trip through the state
// document without growing a second representation.
type LeaseID string

// Lease is a record, not a goroutine. It has no expiry callback, no timer, and
// no cleanup, and that is the design rather than an omission.
//
// Liveness is arithmetic at read time — see Live — which is what makes
// Live(machine) monotonic as the clock advances with no renewal and what makes
// Tick idempotent under a frozen clock. Both are properties asserted in
// docs/testing.md, and this is the decision that earns them.
type Lease struct {
	ID      LeaseID
	Machine MachineID // which machine this lease keeps alive
	Holder  string    // the caller's name; never parsed by firefly
	Kind    Kind      //
	Start   time.Time // set by firefly from now, never by the caller
	TTL     time.Duration
}

// Valid reports whether k is one of the three kinds. It exists because the kind
// is stored, and a document read back with an unknown kind would otherwise be
// counted as if it were real.

var (
	// ErrLeaseExpired means the lease had already lapsed when it was renewed.
	//
	// Renewal never re-acquires: a silent re-acquire would let a supervisor
	// hold a machine open with a lease the caller believes it lost. The
	// session re-acquires explicitly instead, which is a `sad/` case.
	ErrLeaseExpired = errors.New("firefly: lease has lapsed")

	// ErrLeaseTTL means a TTL of zero or less. A lease that expires no later
	// than it begins is not a lease, and accepting one would make a machine
	// fade at a time nobody asked for.
	ErrLeaseTTL = errors.New("firefly: lease ttl must be positive")
)

// ExpiresAt is the first instant at which the lease is no longer live.
func (l Lease) ExpiresAt() time.Time {
	return l.Start.Add(l.TTL)
}

// Valid reports whether k is one of the three kinds. It exists because the kind
// is stored, and a document read back with an unknown kind would otherwise be
// counted as if it were real.
func (k Kind) Valid() bool {
	switch k {
	case Turn, Session, Subscription:
		return true
	default:
		return false
	}
}

// Live reports whether the lease is live at now.
//
// The window is exactly [Start, Start+TTL): live at Start, not live at
// ExpiresAt. The half-open interval is not a detail — it is the property
// docs/testing.md's expiry row asserts, and a closed interval would make a
// lease live at the instant it expired, so a machine would never fade.
func (l Lease) Live(now time.Time) bool {
	if l.TTL <= 0 {
		return false
	}
	return !now.Before(l.Start) && now.Before(l.ExpiresAt())
}

// Renewed returns the lease extended from now.
//
// It extends from *now*, never from the previous expiry, so a renewal arriving
// after a lapse cannot reach backwards and resurrect a lease that has already
// been counted as gone. That is also why renewal on a lapsed lease is an error
// rather than an extension: the caller needs to know its lease ended, because
// the machine may already be draining.
func (l Lease) Renewed(ttl time.Duration, now time.Time) (Lease, error) {
	if ttl <= 0 {
		return Lease{}, ErrLeaseTTL
	}
	if !l.Live(now) {
		return Lease{}, ErrLeaseExpired
	}
	l.Start = now
	l.TTL = ttl
	return l, nil
}

// LiveCount reports how many of leases are live at now.
//
// It is a count rather than a bool because a machine's fate depends on whether
// *any* lease is live, and the count is what the caller shows in the UI
// ("a machine with sessions but no turns" is idle but resumable).
func LiveCount(leases []Lease, now time.Time) int {
	n := 0
	for _, l := range leases {
		if l.Live(now) {
			n++
		}
	}
	return n
}

// LastExpiry returns the latest expiry among the leases live at now, and
// whether any lease is live at all.
//
// This is the instant the drain window starts. Returning ok=false rather than a
// zero time matters: a zero time would read as "expired in year 1" and every
// caller would have to special-case it, which is the kind of thing that turns
// into a machine that terminates immediately after provisioning.
func LastExpiry(leases []Lease, now time.Time) (time.Time, bool) {
	var last time.Time
	live := false
	for _, l := range leases {
		if !l.Live(now) {
			continue
		}
		if !live || l.ExpiresAt().After(last) {
			last = l.ExpiresAt()
			live = true
		}
	}
	return last, live
}
