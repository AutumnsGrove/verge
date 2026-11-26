package proxy_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/proxy"
)

var day1 = time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
var day2 = day1.Add(24 * time.Hour)

// prices is a table shaped like a real one: per million tokens, output dearer than input,
// cache reads far cheaper.
var prices = proxy.Pricing{
	"cheap/model": {Input: 3, Output: 15, Cached: 0.3},
}

func newLedger(t *testing.T, caps proxy.Caps) *proxy.Ledger {
	t.Helper()
	l, err := proxy.NewLedger(prices, caps)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	return l
}

func TestPricing(t *testing.T) {
	t.Run("happy/cache_reads_are_billed_at_the_cached_rate", func(t *testing.T) {
		// 500k full input at $3/M, 500k cached at $0.3/M, 100k output at $15/M.
		cost, ok := prices.Cost("cheap/model", proxy.Usage{Input: 1_000_000, Output: 100_000, Cached: 500_000})
		if !ok {
			t.Fatal("the model was not in the table")
		}
		want := 1.5 + 0.15 + 1.5
		if diff := cost - want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("cost = %v, want %v", cost, want)
		}
	})

	t.Run("bad/an_unknown_model_has_no_price", func(t *testing.T) {
		if cost, ok := prices.Cost("nobody/knows", proxy.Usage{Input: 1_000_000}); ok || cost != 0 {
			t.Fatalf("Cost = (%v, %v), want (0, false)", cost, ok)
		}
	})

	t.Run("bad/a_cached_count_over_the_input_count_cannot_go_negative", func(t *testing.T) {
		// Cached is a subset of Input. A supplier that says otherwise must not produce a
		// negative bill, which would refund the session.
		cost, ok := prices.Cost("cheap/model", proxy.Usage{Input: 0, Output: 0, Cached: 1_000_000})
		if !ok {
			t.Fatal("the model was not in the table")
		}
		if cost < 0 {
			t.Fatalf("cost = %v, want no refund", cost)
		}
	})
}

func TestLedger(t *testing.T) {
	t.Run("happy/a_known_model_is_billed_by_the_table", func(t *testing.T) {
		l := newLedger(t, proxy.Caps{})
		row := l.Record("s-one", "cheap/model", proxy.Usage{Input: 1_000_000, Output: 100_000}, day1)
		if !row.Priced {
			t.Fatal("a model in the table was recorded as unpriced")
		}
		want := 3 + 1.5
		if diff := row.Cost - want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("cost = %v, want %v", row.Cost, want)
		}
		if session, day := l.Spent("s-one", day1); session != row.Cost || day != row.Cost {
			t.Fatalf("Spent = (%v, %v), want both %v", session, day, row.Cost)
		}
	})

	t.Run("happy/the_upstreams_own_cost_beats_the_table", func(t *testing.T) {
		l := newLedger(t, proxy.Caps{})
		u := proxy.Usage{Input: 1_000_000, Cost: 0.25, Reported: true}
		row := l.Record("s-one", "cheap/model", u, day1)
		if row.Cost != 0.25 {
			t.Fatalf("cost = %v, want the router's 0.25", row.Cost)
		}
	})

	t.Run("sad/an_unknown_model_is_recorded_unpriced", func(t *testing.T) {
		// The visible hole: a row that cost tokens and no money in this ledger. Refusing the
		// request would fail on every new model; inventing a price would be a lie.
		l := newLedger(t, proxy.Caps{})
		row := l.Record("s-one", "nobody/knows", proxy.Usage{Input: 500, Output: 5}, day1)
		if row.Priced {
			t.Fatal("an unknown model was priced")
		}
		if row.Cost != 0 {
			t.Fatalf("cost = %v, want 0", row.Cost)
		}
		if row.Usage.Input != 500 {
			t.Fatalf("the token counts were lost: %+v", row.Usage)
		}
	})

	t.Run("bad/a_negative_cost_cannot_refund_a_session", func(t *testing.T) {
		l := newLedger(t, proxy.Caps{PerSession: 1})
		l.Record("s-one", "cheap/model", proxy.Usage{Input: 1_000_000}, day1) // $3
		// A route object built by a caller that trusted a bad upstream.
		l.Record("s-one", "cheap/model", proxy.Usage{Cost: -100, Reported: true}, day1)
		session, _ := l.Spent("s-one", day1)
		if session < 0 {
			t.Fatalf("spent = %v, want no refund", session)
		}
		if err := l.Check("s-one", day1); !errors.Is(err, proxy.ErrCapReached) {
			t.Fatalf("Check = %v, want %v: the refund walked through the cap", err, proxy.ErrCapReached)
		}
	})

	t.Run("sad/the_per_session_cap", func(t *testing.T) {
		l := newLedger(t, proxy.Caps{PerSession: 1})
		if err := l.Check("s-one", day1); err != nil {
			t.Fatalf("Check before spending: %v", err)
		}
		l.Record("s-one", "cheap/model", proxy.Usage{Input: 400_000}, day1) // $1.20
		if err := l.Check("s-one", day1); !errors.Is(err, proxy.ErrCapReached) {
			t.Fatalf("Check = %v, want %v", err, proxy.ErrCapReached)
		}
		// One session's spending is not another's.
		if err := l.Check("s-two", day1); err != nil {
			t.Fatalf("Check for a fresh session: %v", err)
		}
	})

	t.Run("sad/the_per_day_cap_spans_sessions", func(t *testing.T) {
		l := newLedger(t, proxy.Caps{PerDay: 1})
		l.Record("s-one", "cheap/model", proxy.Usage{Input: 400_000}, day1)
		if err := l.Check("s-two", day1); !errors.Is(err, proxy.ErrCapReached) {
			t.Fatalf("Check = %v, want %v across sessions", err, proxy.ErrCapReached)
		}
		if err := l.Check("s-two", day2); err != nil {
			t.Fatalf("Check on the next day = %v, want the day counter reset", err)
		}
	})

	t.Run("happy/a_new_day_resets_the_day_but_not_the_session", func(t *testing.T) {
		// Two windows, two lifetimes: a session is billed for its whole life, and the day
		// cap only counts today. A cross-day session must carry both rows.
		l := newLedger(t, proxy.Caps{PerSession: 10, PerDay: 1})
		l.Record("s-one", "cheap/model", proxy.Usage{Input: 400_000}, day1) // $1.20
		l.Record("s-one", "cheap/model", proxy.Usage{Input: 400_000}, day2) // $1.20

		session, day := l.Spent("s-one", day2)
		if diff := session - 2.4; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("session spent %v, want 2.4", session)
		}
		if diff := day - 1.2; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("day spent %v, want the second day's 1.2", day)
		}
	})

	t.Run("bad/negative_caps_are_refused", func(t *testing.T) {
		for _, caps := range []proxy.Caps{{PerSession: -1}, {PerDay: -1}} {
			if _, err := proxy.NewLedger(nil, caps); !errors.Is(err, proxy.ErrConfig) {
				t.Fatalf("NewLedger(%+v) = %v, want %v", caps, err, proxy.ErrConfig)
			}
		}
	})

	t.Run("happy/the_ledger_is_safe_for_concurrent_requests", func(t *testing.T) {
		// The proxy serves requests concurrently, so this runs under -race in the gate.
		l := newLedger(t, proxy.Caps{})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					l.Record("s-one", "cheap/model", proxy.Usage{Input: 1000, Output: 10}, day1)
					_ = l.Check("s-one", day1)
				}
			}()
		}
		wg.Wait()

		_, day := l.Spent("s-one", day1)
		want := float64(8*50) * (1000.0/1e6*3 + 10.0/1e6*15)
		if diff := day - want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("day spent %v, want %v", day, want)
		}
	})
}
