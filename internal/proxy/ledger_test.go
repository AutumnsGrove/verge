package proxy_test

import (
	"errors"
	"math"
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
	"cheap/model": {Input: 3, Output: 15, Cached: 0.3, CacheWrite: 3.75},
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

	t.Run("happy/cache_writes_are_billed_at_the_write_rate", func(t *testing.T) {
		// A write costs the provider a computation and is priced above input; dropping the
		// field, as the first version did for Anthropic, billed the dearest tokens at nothing.
		cost, ok := prices.Cost("cheap/model", proxy.Usage{Input: 1_000_000, CacheWrite: 1_000_000})
		if !ok {
			t.Fatal("the model was not in the table")
		}
		if diff := cost - 3.75; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("cost = %v, want 3.75", cost)
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

	t.Run("bad/a_null_cost_falls_back_to_the_table", func(t *testing.T) {
		// End to end through the parser: the upstream said nothing about money, so the table
		// prices the tokens instead of the request being recorded as priced at nothing.
		body := `{"usage":{"prompt_tokens":1000000,"completion_tokens":0,"cost":null}}`
		u, err := proxy.UsageFromJSON(proxy.OpenRouter, []byte(body))
		if err != nil {
			t.Fatalf("UsageFromJSON: %v", err)
		}
		l := newLedger(t, proxy.Caps{})
		row := l.Record("s-one", "cheap/model", u, day1)
		if !row.Priced {
			t.Fatal("the row was not priced by the table")
		}
		if diff := row.Cost - 3; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("cost = %v, want the table's 3.00", row.Cost)
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

	t.Run("sad/spending_exactly_the_cap_is_over_it", func(t *testing.T) {
		// ">=", not ">": a cap of $1 is reached at $1. The two caps are checked on separate
		// ledgers, because a day cap firing would mask a session cap that never did.
		for name, caps := range map[string]proxy.Caps{
			"session": {PerSession: 1},
			"day":     {PerDay: 1},
		} {
			t.Run(name, func(t *testing.T) {
				l, err := proxy.NewLedger(proxy.Pricing{"flat/model": {Input: 1}}, caps)
				if err != nil {
					t.Fatalf("NewLedger: %v", err)
				}
				l.Record("s-one", "flat/model", proxy.Usage{Input: 1_000_000}, day1) // exactly $1
				if err := l.Check("s-one", day1); !errors.Is(err, proxy.ErrCapReached) {
					t.Fatalf("Check = %v, want %v at exactly the cap", err, proxy.ErrCapReached)
				}
			})
		}
	})

	t.Run("sad/the_day_is_utc_not_local", func(t *testing.T) {
		// A day cap is a UTC boundary, and every test time was already UTC, so dropping .UTC()
		// survived. These instants are chosen so local date and UTC date disagree.
		l := newLedger(t, proxy.Caps{PerDay: 1})
		first := time.Date(2026, 11, 26, 23, 0, 0, 0, time.UTC)
		l.Record("s-one", "cheap/model", proxy.Usage{Input: 400_000}, first) // $1.20, UTC 26th

		// UTC 27th, but still the 26th at UTC-10. The day must have rolled.
		next := time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC).In(time.FixedZone("UTC-10", -10*3600))
		if err := l.Check("s-two", next); err != nil {
			t.Fatalf("Check on the next UTC day = %v, want the day counter reset", err)
		}

		// And an instant in the same UTC day, expressed in a zone where the date is already
		// the 28th, must not roll again.
		l.Record("s-two", "cheap/model", proxy.Usage{Input: 400_000}, next)
		later := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC).In(time.FixedZone("UTC+14", 14*3600))
		if err := l.Check("s-three", later); !errors.Is(err, proxy.ErrCapReached) {
			t.Fatalf("Check = %v, want %v within the same UTC day", err, proxy.ErrCapReached)
		}
	})

	t.Run("sad/a_late_arrival_from_an_earlier_day_does_not_reset_the_day", func(t *testing.T) {
		// Two requests straddling midnight can finish out of order. A roll that accepted the
		// older timestamp would zero today's spend -- a day cap bypass at every midnight, for
		// anyone who can make two concurrent requests.
		l := newLedger(t, proxy.Caps{PerDay: 1})
		l.Record("s-one", "cheap/model", proxy.Usage{Input: 400_000}, day2) // $1.20, the day is the 27th
		if err := l.Check("s-two", day1); !errors.Is(err, proxy.ErrCapReached) {
			t.Fatalf("Check with an earlier timestamp = %v, want %v", err, proxy.ErrCapReached)
		}
	})

	t.Run("bad/a_nan_cost_is_not_an_answer", func(t *testing.T) {
		// NaN makes every "spent >= cap" false forever. JSON cannot carry it, so only an
		// exported Record can be handed one -- which is exactly why it is guarded.
		l := newLedger(t, proxy.Caps{PerSession: 1})
		// A nonsense cost falls back to the table rather than becoming free money: a million
		// tokens are not $0 because the upstream's number was NaN.
		row := l.Record("s-one", "cheap/model", proxy.Usage{Input: 1_000_000, Cost: math.NaN(), Reported: true}, day1)
		if diff := row.Cost - 3; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("cost = %v, want the table's 3.00", row.Cost)
		}
		l.Record("s-one", "nobody/knows", proxy.Usage{Input: 10, Cost: math.Inf(1), Reported: true}, day1)
		if session, day := l.Spent("s-one", day1); math.IsNaN(session) || math.IsInf(day, 0) {
			t.Fatalf("Spent = (%v, %v), want finite numbers", session, day)
		}

		l.Record("s-one", "cheap/model", proxy.Usage{Input: 400_000}, day1) // $1.20
		if err := l.Check("s-one", day1); !errors.Is(err, proxy.ErrCapReached) {
			t.Fatalf("Check = %v, want %v after a NaN cost", err, proxy.ErrCapReached)
		}
	})

	t.Run("bad/negative_caps_and_prices_are_refused", func(t *testing.T) {
		for _, caps := range []proxy.Caps{{PerSession: -1}, {PerDay: -1}} {
			if _, err := proxy.NewLedger(nil, caps); !errors.Is(err, proxy.ErrConfig) {
				t.Fatalf("NewLedger(%+v) = %v, want %v", caps, err, proxy.ErrConfig)
			}
		}
		for name, price := range map[string]proxy.Price{
			"negative input":  {Input: -1},
			"NaN output":      {Output: math.NaN()},
			"infinite cached": {Cached: math.Inf(1)},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := proxy.NewLedger(proxy.Pricing{"m": price}, proxy.Caps{}); !errors.Is(err, proxy.ErrConfig) {
					t.Fatalf("NewLedger(%+v) = %v, want %v", price, err, proxy.ErrConfig)
				}
			})
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
