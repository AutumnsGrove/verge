// Package loop is the only place in verge that reads the wall clock or sleeps.
//
// Everything else takes time as a parameter: firefly is handed `now`, the fakes
// are handed a clock, and the stores and clients never ask what time it is. That
// discipline is what keeps the test tier offline and instant, and a daemon is
// where it would quietly break -- a `time.Sleep` in a loop is invisible until the
// suite takes eleven minutes.
//
// So the clock and the wait are injected, and this package is the one caller that
// supplies the real ones.
package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
)

// Tick reconciles once. *firefly.Firefly's method value is the ordinary one.
type Tick func(ctx context.Context, now time.Time) (firefly.Result, error)

// Config is what the loop needs to exist.
type Config struct {
	// Tick is one reconcile pass. Required.
	Tick Tick

	// Interval is how long to wait between passes. Required and positive.
	Interval time.Duration

	// Now is the clock handed to Tick. Defaults to time.Now.
	Now func() time.Time

	// Wait returns a channel that is ready after d. Defaults to time.After.
	//
	// It exists so a test can decide when the interval elapsed instead of waiting
	// for it, which is the difference between a loop test that takes seconds and
	// one that takes as long as the production interval.
	Wait func(d time.Duration) <-chan time.Time

	// Log records what a pass did and what it failed to do. Defaults to
	// slog.Default().
	Log *slog.Logger
}

// Loop reconciles on an interval until its context ends.
type Loop struct {
	tick     Tick
	interval time.Duration
	now      func() time.Time
	wait     func(time.Duration) <-chan time.Time
	log      *slog.Logger
}

// ErrConfig means the loop cannot be built from its configuration.
var ErrConfig = errors.New("loop: invalid configuration")

// New validates the configuration and returns the loop.
func New(cfg Config) (*Loop, error) {
	if cfg.Tick == nil {
		return nil, fmt.Errorf("%w: no Tick", ErrConfig)
	}
	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("%w: Interval must be positive", ErrConfig)
	}

	l := &Loop{
		tick:     cfg.Tick,
		interval: cfg.Interval,
		now:      cfg.Now,
		wait:     cfg.Wait,
		log:      cfg.Log,
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.wait == nil {
		l.wait = time.After
	}
	if l.log == nil {
		l.log = slog.Default()
	}
	return l, nil
}

// Run reconciles immediately, then on every interval, until ctx is done.
//
// The first pass is immediate on purpose: a process that has just restarted owns
// machines it last saw some time ago, and waiting one interval to look at them is
// an interval of drift on a machine that may already be overdue.
//
// A failed pass is logged and the loop continues. firefly aborts a pass rather
// than half-applying one, so the next pass is the retry -- and a provider having a
// bad minute is not a reason for a daemon to exit.
//
// Run returns when ctx is done, and it returns *after* the pass in flight, not in
// the middle of one: a tick that is cancelled half way through is the state the
// whole design refuses to leave behind.
func (l *Loop) Run(ctx context.Context) error {
	// The pass gets a context that cannot be cancelled, and that is the whole of
	// the graceful-shutdown promise. ctx.Done is the signal to stop *starting*
	// work; handing it to a pass would instead fire every ctx.Err guard inside the
	// stores and clients, aborting a tick between a machine's save and its delete
	// -- the half-applied state firefly's ordering guarantee exists to prevent.
	//
	// A pass that hangs is therefore not bounded from here. It is bounded from
	// outside: every client has its own timeout, and the process has its own
	// deadline.
	passCtx := context.WithoutCancel(ctx)

	for {
		// A process cancelled before the loop starts -- during startup, or by a
		// signal racing the wiring -- does not reconcile first and stop afterwards.
		if ctx.Err() != nil {
			return nil
		}

		l.pass(passCtx)

		select {
		case <-ctx.Done():
			return nil
		case <-l.wait(l.interval):
		}
	}
}

// pass runs one reconcile and reports it.
func (l *Loop) pass(ctx context.Context) {
	start := l.now()
	res, err := l.tick(ctx, start)
	if err != nil {
		// Not fatal, and not silent: the error is the only record of why nothing
		// happened this pass.
		l.log.Error("reconcile failed", "err", err, "at", start)
		return
	}
	l.log.Info("reconciled",
		"at", start,
		"adopted", len(res.Adopted),
		"swept", len(res.Swept),
		"confirmed", len(res.Confirmed),
		"abandoned", len(res.Abandoned),
		"transitions", len(res.Transitions),
		"saved", res.Saved,
	)
	for _, ev := range res.Transitions {
		l.log.Info("machine moved", "machine", ev.Machine, "from", ev.From, "to", ev.To, "trigger", ev.Trigger)
	}
	// Abandonment is data loss, and it is logged at its own level so that an alert
	// can find it without parsing every tick.
	for _, id := range res.Abandoned {
		l.log.Warn("workspace abandoned: the save could not succeed before the cap", "machine", id)
	}
}
