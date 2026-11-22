package firefly

import (
	"errors"
	"testing"
	"time"
)

// leaseBase is a fixed instant so that every case reads as an offset from one
// clock rather than as a literal. Tests here never call time.Now.
var leaseBase = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return leaseBase.Add(d) }

func TestLeaseLive(t *testing.T) {
	// The window is [Start, Start+TTL). Every boundary is asserted, because an
	// off-by-one at ExpiresAt means either a machine that never fades or one
	// that fades while leased.
	l := Lease{ID: "l1", Holder: "session:01J8", Kind: Session, Start: leaseBase, TTL: 10 * time.Minute}

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"happy/at_start_is_live", at(0), true},
		{"happy/inside_is_live", at(9 * time.Minute), true},
		{"sad/at_expiry_is_not_live", at(10 * time.Minute), false},
		{"sad/after_expiry_is_not_live", at(11 * time.Minute), false},
		{"bad/before_start_is_not_live", at(-time.Nanosecond), false},
		{"bad/zero_ttl_is_never_live", at(0), false},
		{"bad/negative_ttl_is_never_live", at(0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subject := l
			if c.name == "bad/zero_ttl_is_never_live" {
				subject.TTL = 0
			}
			if c.name == "bad/negative_ttl_is_never_live" {
				subject.TTL = -time.Minute
			}
			if got := subject.Live(c.now); got != c.want {
				t.Fatalf("Live(%s) = %v, want %v", c.now.Format(time.RFC3339Nano), got, c.want)
			}
		})
	}
}

func TestLeaseRenewed(t *testing.T) {
	base := Lease{ID: "l1", Holder: "turn:01J8/7", Kind: Turn, Start: leaseBase, TTL: 5 * time.Minute}

	t.Run("happy/extends_from_now", func(t *testing.T) {
		got, err := base.Renewed(10*time.Minute, at(3*time.Minute))
		if err != nil {
			t.Fatalf("Renewed: %v", err)
		}
		if !got.Start.Equal(at(3 * time.Minute)) {
			t.Fatalf("Start = %s, want %s (extend from now, not from the old expiry)",
				got.Start, at(3*time.Minute))
		}
		if want := at(13 * time.Minute); !got.ExpiresAt().Equal(want) {
			t.Fatalf("ExpiresAt = %s, want %s", got.ExpiresAt(), want)
		}
	})

	t.Run("happy/renewal_at_start_is_allowed", func(t *testing.T) {
		if _, err := base.Renewed(time.Minute, at(0)); err != nil {
			t.Fatalf("Renewed at Start: %v", err)
		}
	})

	// A renewal arriving after the lapse must fail rather than resurrect the
	// lease: the machine may already be draining by then, and a supervisor
	// that silently re-acquired would hold it open on a lease the caller
	// believes it lost.
	t.Run("sad/lapsed_lease_cannot_be_renewed", func(t *testing.T) {
		_, err := base.Renewed(time.Minute, at(5*time.Minute))
		if !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("Renewed on a lapsed lease = %v, want ErrLeaseExpired", err)
		}
	})

	t.Run("sad/one_nanosecond_late_is_lapsed", func(t *testing.T) {
		_, err := base.Renewed(time.Minute, at(5*time.Minute+time.Nanosecond))
		if !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("Renewed just after expiry = %v, want ErrLeaseExpired", err)
		}
	})

	// A renewal cannot reach backwards. Being explicit about it stops a future
	// optimisation from "fixing" the Start assignment.
	t.Run("bad/renewal_never_moves_start_backwards", func(t *testing.T) {
		got, err := base.Renewed(10*time.Minute, at(-time.Minute))
		if !errors.Is(err, ErrLeaseExpired) {
			t.Fatalf("Renewed before Start = %v, want ErrLeaseExpired", err)
		}
		if !got.Start.IsZero() {
			t.Fatalf("rejected renewal returned a lease with Start %s, want the zero Lease", got.Start)
		}
	})

	t.Run("bad/zero_ttl_refused", func(t *testing.T) {
		_, err := base.Renewed(0, at(time.Minute))
		if !errors.Is(err, ErrLeaseTTL) {
			t.Fatalf("Renewed with zero ttl = %v, want ErrLeaseTTL", err)
		}
	})

	t.Run("bad/negative_ttl_refused", func(t *testing.T) {
		_, err := base.Renewed(-time.Minute, at(time.Minute))
		if !errors.Is(err, ErrLeaseTTL) {
			t.Fatalf("Renewed with negative ttl = %v, want ErrLeaseTTL", err)
		}
	})
}

func TestLiveCount(t *testing.T) {
	leases := []Lease{
		{ID: "turn", Kind: Turn, Start: leaseBase, TTL: 2 * time.Minute},
		{ID: "session", Kind: Session, Start: leaseBase, TTL: 10 * time.Minute},
		{ID: "sub", Kind: Subscription, Start: leaseBase, TTL: 24 * time.Hour},
	}

	cases := []struct {
		name string
		now  time.Time
		want int
	}{
		{"happy/all_live_at_start", at(0), 3},
		{"happy/turn_lapsed", at(3 * time.Minute), 2},
		{"sad/only_subscription_left", at(11 * time.Minute), 1},
		{"sad/none_live", at(25 * time.Hour), 0},
		{"bad/empty_is_zero", at(0), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subject := leases
			if c.name == "bad/empty_is_zero" {
				subject = nil
			}
			if got := LiveCount(subject, c.now); got != c.want {
				t.Fatalf("LiveCount = %d, want %d", got, c.want)
			}
		})
	}
}

// TestLiveCountIsMonotonicInTheClock is the "monotonic clock" property from
// docs/testing.md: Live(instance) never grows as the clock advances with no
// renewal. It is asserted by stepping the clock rather than by a golden value,
// because the property is about every instant, not one.
func TestLiveCountIsMonotonicInTheClock(t *testing.T) {
	leases := []Lease{
		{ID: "turn", Kind: Turn, Start: leaseBase, TTL: 2 * time.Minute},
		{ID: "session", Kind: Session, Start: leaseBase, TTL: 10 * time.Minute},
	}
	prev := LiveCount(leases, at(0))
	for step := time.Duration(1); step <= 12*time.Minute; step += 7 * time.Second {
		got := LiveCount(leases, at(step))
		if got > prev {
			t.Fatalf("LiveCount grew from %d to %d at +%s with no renewal", prev, got, step)
		}
		prev = got
	}
}

// TestLastExpiryNeverFadesEarlier is the "monotonicity" property: adding a
// lease never makes a machine fade sooner. It is the reason a busy session
// cannot be evicted by an unrelated subscription arriving.
func TestLastExpiryNeverFadesEarlier(t *testing.T) {
	session := Lease{ID: "session", Kind: Session, Start: leaseBase, TTL: 10 * time.Minute}
	sub := Lease{ID: "sub", Kind: Subscription, Start: leaseBase, TTL: time.Hour}

	before, ok := LastExpiry([]Lease{session}, at(time.Minute))
	if !ok {
		t.Fatal("LastExpiry with a live lease = ok false")
	}
	after, ok := LastExpiry([]Lease{session, sub}, at(time.Minute))
	if !ok {
		t.Fatal("LastExpiry after adding a lease = ok false")
	}
	if after.Before(before) {
		t.Fatalf("adding a lease moved the fade from %s to %s, which is sooner", before, after)
	}
}

func TestLastExpiry(t *testing.T) {
	short := Lease{ID: "short", Start: leaseBase, TTL: time.Minute}
	long := Lease{ID: "long", Start: leaseBase, TTL: time.Hour}

	t.Run("happy/latest_wins", func(t *testing.T) {
		got, ok := LastExpiry([]Lease{short, long}, at(0))
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if want := at(time.Hour); !got.Equal(want) {
			t.Fatalf("LastExpiry = %s, want %s", got, want)
		}
	})

	t.Run("happy/order_does_not_matter", func(t *testing.T) {
		got, ok := LastExpiry([]Lease{long, short}, at(0))
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if want := at(time.Hour); !got.Equal(want) {
			t.Fatalf("LastExpiry = %s, want %s", got, want)
		}
	})

	t.Run("sad/all_lapsed_reports_not_ok", func(t *testing.T) {
		got, ok := LastExpiry([]Lease{short}, at(2*time.Minute))
		if ok {
			t.Fatal("ok = true with no live lease")
		}
		if !got.IsZero() {
			t.Fatalf("LastExpiry with no live lease = %s, want the zero time", got)
		}
	})

	t.Run("bad/empty_reports_not_ok", func(t *testing.T) {
		got, ok := LastExpiry(nil, at(0))
		if ok {
			t.Fatal("ok = true for an empty lease set")
		}
		if !got.IsZero() {
			t.Fatalf("LastExpiry of nothing = %s, want the zero time", got)
		}
	})
}
