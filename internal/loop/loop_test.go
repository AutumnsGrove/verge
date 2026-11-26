package loop_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/firefly"
	"github.com/AutumnsGrove/verge/internal/loop"
)

var base = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// recorder is a loop driven by the test rather than by the clock.
//
// wait is the seam: the test decides when an interval has elapsed, so nothing here
// sleeps and the whole file finishes in microseconds no matter what Interval says.
type recorder struct {
	mu sync.Mutex

	ticks   []time.Time
	waits   []time.Duration
	done    chan struct{} // closed when the loop returns
	release chan struct{} // a tick that blocks waits on this
	fail    error         // returned by the first tick when set
	block   bool          // the first tick blocks until release is closed
	// cancelledPass is set when a pass in flight finds its context cancelled, which
	// is the failure the graceful-shutdown promise forbids.
	cancelledPass error
	waitFor       chan time.Time // the test signals "the interval elapsed"
	ticked        chan struct{}  // each pass announces itself, so no test polls

	log *bytes.Buffer
}

func newRecorder() *recorder {
	return &recorder{
		done:    make(chan struct{}),
		release: make(chan struct{}),
		waitFor: make(chan time.Time, 8),
		ticked:  make(chan struct{}, 32),
		log:     &bytes.Buffer{},
	}
}

func (r *recorder) loop(t *testing.T, interval time.Duration) *loop.Loop {
	t.Helper()
	l, err := loop.New(loop.Config{
		Interval: interval,
		Tick: func(ctx context.Context, now time.Time) (firefly.Result, error) {
			r.mu.Lock()
			r.ticks = append(r.ticks, now)
			block, failure := r.block, r.fail
			r.block = false
			r.fail = nil
			r.mu.Unlock()
			select {
			case r.ticked <- struct{}{}:
			default:
			}

			if block {
				<-r.release
				// The pass in flight must be finishable: a cancelled context here is
				// the bug this case exists to catch, and it is a bug about the context
				// the loop hands the pass rather than about blocking.
				if err := ctx.Err(); err != nil {
					r.mu.Lock()
					r.cancelledPass = err
					r.mu.Unlock()
					return firefly.Result{}, err
				}
			}
			if failure != nil {
				return firefly.Result{}, failure
			}
			return firefly.Result{Transitions: []firefly.TransitionEvent{{
				Machine: "fake-001", From: firefly.Ready, To: firefly.Draining, Trigger: firefly.LeasesLapsed,
			}}}, nil
		},
		Now: func() time.Time {
			r.mu.Lock()
			defer r.mu.Unlock()
			// A clock that moves only because a tick asked what time it was: it
			// makes the recorded times distinguishable without ever sleeping.
			return base.Add(time.Duration(len(r.ticks)) * time.Minute)
		},
		Wait: func(d time.Duration) <-chan time.Time {
			r.mu.Lock()
			r.waits = append(r.waits, d)
			r.mu.Unlock()
			return r.waitFor
		},
		Log: slog.New(slog.NewTextHandler(r.log, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		t.Fatalf("loop.New: %v", err)
	}
	return l
}

// run starts the loop and returns a function that stops it and waits.
func (r *recorder) run(t *testing.T, l *loop.Loop) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var err error
	go func() {
		err = l.Run(ctx)
		close(r.done)
	}()
	return func() error {
		cancel()
		select {
		case <-r.done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("the loop did not return after its context ended")
			return nil
		}
	}
}

// awaitTicks waits until n passes have announced themselves.
func (r *recorder) awaitTicks(t *testing.T, n int) {
	t.Helper()
	for got := r.count(); got < n; got = r.count() {
		select {
		case <-r.ticked:
		case <-time.After(5 * time.Second):
			t.Fatalf("saw %d ticks, want %d", r.count(), n)
		}
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ticks)
}

func TestRun(t *testing.T) {
	t.Run("happy/first_pass_is_immediate", func(t *testing.T) {
		// A restarted process owns machines it last saw before it restarted, so the
		// first look happens before any interval elapses.
		r := newRecorder()
		stop := r.run(t, r.loop(t, time.Minute))
		r.awaitTicks(t, 1)
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
		if got := r.count(); got != 1 {
			t.Fatalf("%d ticks, want exactly 1: the loop kept going after cancellation", got)
		}
	})

	t.Run("happy/waits_the_configured_interval", func(t *testing.T) {
		const interval = 90 * time.Second
		r := newRecorder()
		stop := r.run(t, r.loop(t, interval))
		r.awaitTicks(t, 1)

		for i := 0; i < 3; i++ {
			r.waitFor <- base
			r.awaitTicks(t, i+2)
		}
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}

		r.mu.Lock()
		defer r.mu.Unlock()
		for i, d := range r.waits {
			if d != interval {
				t.Fatalf("wait %d asked for %s, want %s", i, d, interval)
			}
		}
		if len(r.waits) < 3 {
			t.Fatalf("waited %d times, want at least 3", len(r.waits))
		}
	})

	t.Run("happy/the_clock_is_read_once_per_pass", func(t *testing.T) {
		// Tick is handed the same instant the loop logged, so a report and the pass
		// it describes cannot disagree about when they happened.
		r := newRecorder()
		stop := r.run(t, r.loop(t, time.Minute))
		r.awaitTicks(t, 1)
		r.waitFor <- base
		r.awaitTicks(t, 2)
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}

		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.ticks) < 2 || !r.ticks[1].After(r.ticks[0]) {
			t.Fatalf("tick instants = %v, want each pass to see a later clock", r.ticks)
		}
	})

	t.Run("sad/a_failed_pass_does_not_stop_the_loop", func(t *testing.T) {
		// firefly aborts a pass rather than half-applying one, so the retry is the
		// next pass. A provider having a bad minute must not end the daemon.
		r := newRecorder()
		r.fail = errors.New("the provider said no")
		stop := r.run(t, r.loop(t, time.Minute))
		r.awaitTicks(t, 1)

		r.waitFor <- base
		r.awaitTicks(t, 2)
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}

		logs := r.log.String()
		if !strings.Contains(logs, "reconcile failed") || !strings.Contains(logs, "the provider said no") {
			t.Fatalf("the failed pass was not logged:\n%s", logs)
		}
		if !strings.Contains(logs, "reconciled") {
			t.Fatalf("the pass after the failure was not logged:\n%s", logs)
		}
	})

	t.Run("happy/a_pass_in_flight_finishes", func(t *testing.T) {
		// Cancellation stops the loop starting another pass; it does not abandon the
		// one already walking a machine's row. The pass itself checks its own context
		// once it unblocks, so this case fails if Run hands Tick the cancellable one.
		r := newRecorder()
		r.block = true
		stop := r.run(t, r.loop(t, time.Minute))
		r.awaitTicks(t, 1)

		stopped := make(chan error, 1)
		go func() { stopped <- stop() }()

		select {
		case <-stopped:
			t.Fatal("Run returned while a pass was still in flight")
		case <-time.After(50 * time.Millisecond):
		}

		close(r.release)
		if err := <-stopped; err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
		if got := r.count(); got != 1 {
			t.Fatalf("%d ticks, want 1: a pass started after cancellation", got)
		}
		if r.cancelledPass != nil {
			t.Fatalf("the pass in flight was cancelled under it: %v", r.cancelledPass)
		}
	})

	t.Run("happy/already_cancelled_starts_nothing", func(t *testing.T) {
		// A signal that lands between NotifyContext and Run must not buy a full
		// reconcile first: the process was asked to stop before it started.
		r := newRecorder()
		l := r.loop(t, time.Minute)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := l.Run(ctx); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
		if got := r.count(); got != 0 {
			t.Fatalf("%d ticks before the loop noticed cancellation, want 0", got)
		}
	})

	t.Run("happy/reports_what_the_pass_did", func(t *testing.T) {
		r := newRecorder()
		stop := r.run(t, r.loop(t, time.Minute))
		r.awaitTicks(t, 1)
		if err := stop(); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}

		logs := r.log.String()
		for _, want := range []string{"reconciled", "transitions=1", "machine moved", "fake-001", "trigger=LeasesLapsed"} {
			if !strings.Contains(logs, want) {
				t.Fatalf("log does not contain %q:\n%s", want, logs)
			}
		}
	})
}

func TestNew(t *testing.T) {
	t.Run("bad/no_tick", func(t *testing.T) {
		if _, err := loop.New(loop.Config{Interval: time.Minute}); !errors.Is(err, loop.ErrConfig) {
			t.Fatalf("New = %v, want %v", err, loop.ErrConfig)
		}
	})

	t.Run("bad/interval_is_not_positive", func(t *testing.T) {
		for _, d := range []time.Duration{0, -time.Second} {
			_, err := loop.New(loop.Config{Tick: func(context.Context, time.Time) (firefly.Result, error) {
				return firefly.Result{}, nil
			}, Interval: d})
			if !errors.Is(err, loop.ErrConfig) {
				t.Fatalf("New(Interval=%s) = %v, want %v", d, err, loop.ErrConfig)
			}
		}
	})

	t.Run("happy/defaults_are_the_wall_clock", func(t *testing.T) {
		// The defaults are the real ones on purpose: this package is where the wall
		// clock is allowed, and a loop built without an explicit clock is the daemon
		// rather than a test.
		l, err := loop.New(loop.Config{
			Interval: time.Minute,
			Tick: func(context.Context, time.Time) (firefly.Result, error) {
				return firefly.Result{}, nil
			},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := l.Run(ctx); err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})
}
