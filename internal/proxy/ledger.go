package proxy

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// Price is what one model charges, in USD per million tokens.
//
// Cached is the price of a cache *read* and CacheWrite of a cache write, which every
// provider prices above input (a write costs the provider a computation). **A table that
// leaves CacheWrite at zero charges writes nothing**: there is no default, because "zero
// means free" and "zero means input" are both plausible and guessing one would be a silent
// under-charge either way. Set it.
type Price struct {
	Input      float64
	Output     float64
	Cached     float64
	CacheWrite float64
}

// Pricing is the price table, keyed by model id as the caller wrote it.
//
// It is configuration rather than code: prices change without a release, and a table in a
// binary is a table that is wrong for a month before anyone notices. An unknown model is
// the case that matters -- see Ledger.Record.
type Pricing map[string]Price

// Cost prices one usage, and reports whether the model was in the table.
func (p Pricing) Cost(model string, u Usage) (float64, bool) {
	price, ok := p[model]
	if !ok {
		return 0, false
	}
	// Input is the whole prompt, and the two cached subsets are billed at their own rates;
	// the remainder is billed at the input rate. Each subtraction is clamped on its own, so
	// an upstream that reports a subset larger than its total cannot produce a refund.
	cached, written := u.Cached, u.CacheWrite
	if cached < 0 {
		cached = 0
	}
	if written < 0 {
		written = 0
	}
	full := u.Input - cached - written
	if full < 0 {
		full = 0
	}
	usd := float64(full)/1e6*price.Input +
		float64(cached)/1e6*price.Cached +
		float64(written)/1e6*price.CacheWrite +
		float64(u.Output)/1e6*price.Output
	if usd < 0 {
		usd = 0
	}
	return usd, true
}

// Caps is the spend ceiling, in USD, over two windows. Zero means no cap in that window.
//
// A cap is a gate, not a hard ceiling: the proxy checks spend before forwarding, and a
// request that crosses the line finishes and is billed. Making it exact would mean knowing
// the cost of output that has not been generated yet, and a proxy that guessed would refuse
// work a user had already paid for in waiting.
type Caps struct {
	PerSession float64
	PerDay     float64
}

// ErrCapReached means the caller has spent its budget and the request is refused.
//
// It is the one refusal a caller is meant to see: the phone reads it as "come look", not as
// a broken proxy.
var ErrCapReached = errors.New("proxy: the spend cap is reached")

// Row is one request, as the caller records it.
type Row struct {
	Session string
	Model   string
	Usage   Usage

	// Cost is what the row is charged, in USD.
	Cost float64

	// Priced is false when nothing knew the model's price and the upstream did not report
	// one: the row is real, its cost is zero, and an operator's cap is not counting it.
	Priced bool

	// At is when the row was recorded, in the caller's clock.
	At time.Time
}

// Ledger counts spend per session and per day, and refuses what is over a cap.
//
// It is in memory, and that is a limitation rather than a design: a restart forgets the
// day's spend, so the day cap is a cap per process lifetime. Persisting it is the next
// piece (the plan's SQLite), and until then this is the honest shape -- a durable ledger
// bolted onto an in-memory one would be two sources of truth on the day it mattered.
type Ledger struct {
	mu     sync.Mutex
	caps   Caps
	prices Pricing

	sessions map[string]float64
	today    string
	spent    float64
}

// NewLedger returns a ledger with a price table and the deployment's caps.
func NewLedger(prices Pricing, caps Caps) (*Ledger, error) {
	if caps.PerSession < 0 || caps.PerDay < 0 {
		return nil, fmt.Errorf("%w: a cap cannot be negative", ErrConfig)
	}
	for model, price := range prices {
		for name, value := range map[string]float64{
			"input": price.Input, "output": price.Output,
			"cached": price.Cached, "cache write": price.CacheWrite,
		} {
			if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, fmt.Errorf("%w: the %s price of %q is not a price", ErrConfig, name, model)
			}
		}
	}
	if prices == nil {
		prices = Pricing{}
	}
	return &Ledger{
		caps:     caps,
		prices:   prices,
		sessions: map[string]float64{},
		today:    dayOf(time.Time{}),
	}, nil
}

// Check reports whether a session may make one more request, and why not.
//
// It is the caller's gate before the upstream request, because the cheap refusal is the one
// that costs nothing: a request that reached the provider and was then refused was billed
// anyway.
func (l *Ledger) Check(session string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.roll(now)
	if l.caps.PerSession > 0 && l.sessions[session] >= l.caps.PerSession {
		return fmt.Errorf("%w: session %q has spent %.4f of %.2f", ErrCapReached, session, l.sessions[session], l.caps.PerSession)
	}
	if l.caps.PerDay > 0 && l.spent >= l.caps.PerDay {
		return fmt.Errorf("%w: today has spent %.4f of %.2f", ErrCapReached, l.spent, l.caps.PerDay)
	}
	return nil
}

// Record charges one request and returns the row it wrote.
//
// The cost is the upstream's own when it reported one -- OpenRouter does, and its number is
// better than any table kept here -- and the price table's otherwise. A model in neither is
// recorded with a zero cost and Priced false: inventing a price would be worse than leaving
// a visible hole, and refusing the request would be a proxy that fails on a new model.
func (l *Ledger) Record(session, model string, u Usage, now time.Time) Row {
	// A NaN or infinite cost is something JSON cannot express, so only a caller can build it
	// -- and a NaN total makes every later "spent >= cap" false forever. It is not an answer,
	// so the table prices the tokens instead.
	if u.Reported && (math.IsNaN(u.Cost) || math.IsInf(u.Cost, 0)) {
		u.Cost, u.Reported = 0, false
	}

	row := Row{Session: session, Model: model, Usage: u, At: now}

	if u.Reported {
		row.Cost, row.Priced = u.Cost, true
	} else if cost, ok := l.prices.Cost(model, u); ok {
		row.Cost, row.Priced = cost, true
	}
	// The parser clamps, and this clamps again because Record is exported: a caller that
	// built a Usage itself must not be able to refund a session by passing a negative.
	if row.Cost < 0 || math.IsNaN(row.Cost) || math.IsInf(row.Cost, 0) {
		row.Cost = 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	l.sessions[session] += row.Cost
	l.spent += row.Cost
	return row
}

// Spent reports what a session has spent, and what the day has.
func (l *Ledger) Spent(session string, now time.Time) (sessionUSD, dayUSD float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll(now)
	return l.sessions[session], l.spent
}

// roll moves the day's counter when the date changed, forward only.
//
// The key is UTC because a day cap is not a local boundary: a deployment's machines and its
// control plane need to agree on when a day ends, and only UTC is a fact for both.
//
// Forward only, because two requests straddling midnight can finish out of order: the older
// timestamp arriving second would otherwise roll the counter *back* and zero today's spend --
// one free day's worth of requests per midnight, for anyone able to make two concurrent calls.
func (l *Ledger) roll(now time.Time) {
	today := dayOf(now)
	// ISO dates compare as strings, and every date a clock can produce has the same width.
	if today > l.today {
		l.today = today
		l.spent = 0
	}
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }
