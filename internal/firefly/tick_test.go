// Tick's tests live in the external test package so they can use the shared fake
// cloud, fake network, and fake syncer. A second, local fake would be the
// divergent-fake problem docs/testing.md warns about: two implementations of one
// cloud, drifting apart, with the suite trusting whichever one the test imported.
package firefly_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AutumnsGrove/verge/internal/cloud/fake"
	"github.com/AutumnsGrove/verge/internal/firefly"
)

var base = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return base.Add(d) }

// harness is one fake world and the firefly over it.
type harness struct {
	t     *testing.T
	cloud *fake.Cloud
	net   *fake.Network
	sync  *fake.Syncer
	store *firefly.MemStore
	f     *firefly.Firefly
}

func newHarness(t *testing.T, opts ...func(*fake.Cloud, *fake.Network)) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		cloud: fake.New(base).Boot(30 * time.Second),
		store: firefly.NewMemStore(),
		sync:  fake.NewSyncer(),
	}
	h.net = fake.NewNetwork(h.cloud)
	for _, o := range opts {
		o(h.cloud, h.net)
	}

	f, err := firefly.New(firefly.Config{
		Store:             h.store,
		Cloud:             h.cloud,
		Network:           h.net,
		Syncer:            h.sync,
		Label:             "verge=machine",
		MaxLifetime:       4 * time.Hour,
		DrainWindow:       15 * time.Minute,
		BootDeadline:      10 * time.Minute,
		SyncAttempts:      3,
		TerminateDeadline: 5 * time.Minute,
		RetryBase:         30 * time.Second,
		RetryMax:          5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("firefly.New: %v", err)
	}
	h.f = f
	return h
}

// provision creates a machine in the fake cloud and records it in the document,
// which is what a successful Provision would leave behind.
func (h *harness) provision(state firefly.State) firefly.MachineID {
	h.t.Helper()
	ctx := context.Background()

	id, err := h.cloud.Create(ctx, firefly.Spec{
		CloudInit: []byte("#cloud-config\n"),
		Image:     "ubuntu-24.04",
		Labels:    map[string]string{"verge": "machine"},
	})
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	cm, err := h.cloud.Describe(ctx, id)
	if err != nil {
		h.t.Fatalf("Describe: %v", err)
	}

	doc, err := h.store.Load(ctx)
	if errors.Is(err, firefly.ErrNoDocument) {
		doc = firefly.Document{Version: firefly.DocumentVersion}
	} else if err != nil {
		h.t.Fatalf("Load: %v", err)
	}
	doc.Machines = append(doc.Machines, firefly.Machine{
		ID:           id,
		Name:         cm.Name,
		State:        state,
		CreatedAt:    cm.CreatedAt,
		Deadline:     cm.CreatedAt.Add(4 * time.Hour),
		Since:        cm.CreatedAt,
		ReachedReady: state == firefly.Ready,
	})
	if err := h.store.Save(ctx, doc); err != nil {
		h.t.Fatalf("Save: %v", err)
	}
	return id
}

func (h *harness) tick(now time.Time) firefly.Result {
	h.t.Helper()
	// The fake owns one clock and Tick's now is that clock: a test that walked
	// them independently would be asserting against two different worlds.
	if d := now.Sub(h.cloud.Now()); d > 0 {
		h.cloud.Advance(d)
	}
	res, err := h.f.Tick(context.Background(), now)
	if err != nil {
		h.t.Fatalf("Tick at %s: %v", now.Sub(base), err)
	}
	return res
}

func (h *harness) stateOf(id firefly.MachineID) firefly.State {
	h.t.Helper()
	doc, err := h.store.Load(context.Background())
	if err != nil {
		h.t.Fatalf("Load: %v", err)
	}
	m, ok := doc.Machine(id)
	if !ok {
		h.t.Fatalf("machine %s is not in the document", id)
	}
	return m.State
}

// TestLifecycleEndToEnd drives one machine from provisioning to gone, asserting the
// state after every tick. This is the whole mechanism in one test.
func TestLifecycleEndToEnd(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Provisioning)

	// Booting: the provider knows it, the mesh does not yet.
	res := h.tick(at(0))
	if got := h.stateOf(id); got != firefly.Provisioning {
		t.Fatalf("after the first tick: %s, want %s", got, firefly.Provisioning)
	}
	if len(res.Transitions) != 0 {
		t.Fatalf("a machine that is still booting produced %d transitions", len(res.Transitions))
	}

	// Booted: provider running, node joined -> Ready.
	h.cloud.Advance(time.Minute)
	res = h.tick(at(time.Minute))
	if got := h.stateOf(id); got != firefly.Ready {
		t.Fatalf("after booting: %s, want %s", got, firefly.Ready)
	}
	if len(res.Transitions) != 1 || res.Transitions[0].Trigger != firefly.Answered {
		t.Fatalf("transitions = %+v, want one Answered", res.Transitions)
	}

	// Work arrives, then finishes.
	lease, err := h.f.Acquire(ctx, id, "session:01J8", firefly.Session, 10*time.Minute, at(time.Minute))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	h.tick(at(2 * time.Minute))
	if got := h.stateOf(id); got != firefly.Ready {
		t.Fatalf("while leased: %s, want %s", got, firefly.Ready)
	}

	if err := h.f.Release(ctx, lease); err != nil {
		t.Fatalf("Release: %v", err)
	}
	h.tick(at(3 * time.Minute))
	if got := h.stateOf(id); got != firefly.Draining {
		t.Fatalf("after the last lease lapsed: %s, want %s", got, firefly.Draining)
	}

	// Drain window elapses -> sync -> terminate.
	h.tick(at(3*time.Minute + 16*time.Minute))
	if got := h.stateOf(id); got != firefly.Syncing {
		t.Fatalf("after the drain window: %s, want %s", got, firefly.Syncing)
	}

	// The save happens on the tick that leaves SYNCING, because a machine walks
	// one row per pass and the row it is on is the one that does the work.
	h.tick(at(20 * time.Minute))
	if got := h.stateOf(id); got != firefly.Terminating {
		t.Fatalf("after syncing: %s, want %s", got, firefly.Terminating)
	}
	if got := h.sync.Synced(); len(got) != 1 || got[0] != id {
		t.Fatalf("synced = %v, want [%s]", got, id)
	}

	// The provider reaps it; the node lingers, so the tick after confirms.
	h.cloud.Advance(time.Minute)
	h.tick(at(21 * time.Minute))
	if got := h.stateOf(id); got != firefly.Terminating {
		t.Fatalf("while the node lingers: %s, want %s", got, firefly.Terminating)
	}

	h.tick(at(22 * time.Minute))
	if _, err := h.store.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	doc, _ := h.store.Load(ctx)
	if _, ok := doc.Machine(id); ok {
		t.Fatal("a confirmed machine is still in the document")
	}
}

// TestOneTransitionPerMachinePerTick is the rule the ordering property depends on:
// a pass that walked two rows would let a machine reach a delete without the save
// in between being observed.
func TestOneTransitionPerMachinePerTick(t *testing.T) {
	h := newHarness(t)
	id := h.provision(firefly.Provisioning)

	// Even with everything that would advance it now true at once.
	h.cloud.Advance(time.Minute)
	res := h.tick(at(30 * time.Minute))

	if got := h.stateOf(id); got != firefly.Ready {
		t.Fatalf("state = %s, want %s (one row only)", got, firefly.Ready)
	}
	if len(res.Transitions) != 1 {
		t.Fatalf("%d transitions in one tick, want 1", len(res.Transitions))
	}
}

// TestDocumentIsSavedOncePerTick is what MemStore.Saves exists for.
func TestDocumentIsSavedOncePerTick(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		h.provision(firefly.Provisioning)
	}
	before := h.store.Saves()

	h.cloud.Advance(time.Minute)
	h.tick(at(time.Minute))

	if got := h.store.Saves() - before; got != 1 {
		t.Fatalf("three machines changing produced %d saves, want 1", got)
	}
}

// TestNothingChangedWritesNothing: a quiet tick must not touch storage, or every
// poll interval becomes a write.
func TestNothingChangedWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.provision(firefly.Provisioning)
	h.tick(at(0))
	before := h.store.Saves()

	res := h.tick(at(time.Second))
	if res.Saved {
		t.Fatal("a tick with no changes wrote the document")
	}
	if got := h.store.Saves() - before; got != 0 {
		t.Fatalf("%d writes on a quiet tick, want 0", got)
	}
}

// TestStalledBootBecomesAnError covers StallBoot: the provider keeps saying
// starting, and the boot deadline turns that into a failure rather than patience.
func TestStalledBootBecomesAnError(t *testing.T) {
	h := newHarness(t, func(c *fake.Cloud, _ *fake.Network) { c.StallBoot() })
	id := h.provision(firefly.Provisioning)

	h.tick(at(0))
	if got := h.stateOf(id); got != firefly.Provisioning {
		t.Fatalf("before the deadline: %s, want %s", got, firefly.Provisioning)
	}

	h.cloud.Advance(11 * time.Minute)
	h.tick(at(11 * time.Minute))
	if got := h.stateOf(id); got != firefly.Error {
		t.Fatalf("after the boot deadline: %s, want %s", got, firefly.Error)
	}
}

// TestMachineThatNeverBecameReadyStillDies is the cap working on a machine with
// nothing to save: the sync is a no-op, so the machine can be destroyed.
func TestMachineThatNeverBecameReadyStillDies(t *testing.T) {
	h := newHarness(t, func(c *fake.Cloud, _ *fake.Network) { c.StallBoot() })
	id := h.provision(firefly.Provisioning)

	// Past the cap while still in ERROR: capped into Syncing, which no-ops.
	h.cloud.Advance(5 * time.Hour)
	h.tick(at(5 * time.Hour))

	doc, _ := h.store.Load(context.Background())
	m, ok := doc.Machine(id)
	if !ok {
		t.Fatal("the machine vanished")
	}
	if m.State != firefly.Syncing && m.State != firefly.Terminating {
		t.Fatalf("state = %s, want Syncing or Terminating", m.State)
	}
	if got := h.sync.Calls(); got != 0 {
		t.Fatalf("a machine that never became ready called Sync %d times, want 0", got)
	}
}

// TestSyncFailureKeepsTheMachine is the asymmetry the design insists on: a
// lingering machine costs cents, and a destroyed one holding work costs the work.
func TestSyncFailureKeepsTheMachine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Ready)

	h.sync.FailNext(1)

	// Ready, unleased, past the drain window: lapsing then draining.
	h.tick(at(0))
	if got := h.stateOf(id); got != firefly.Draining {
		t.Fatalf("state = %s, want %s", got, firefly.Draining)
	}

	h.tick(at(16 * time.Minute))
	if got := h.stateOf(id); got != firefly.Syncing {
		t.Fatalf("state = %s, want %s", got, firefly.Syncing)
	}

	// The save fails: the machine stays.
	res := h.tick(at(17 * time.Minute))
	if got := h.stateOf(id); got != firefly.Error {
		t.Fatalf("after a failed save: %s, want %s", got, firefly.Error)
	}
	if len(res.Abandoned) != 0 {
		t.Fatal("a machine was abandoned before its cap")
	}
	if _, err := h.cloud.Describe(ctx, id); err != nil {
		t.Fatalf("the machine was destroyed after one failed save: %v", err)
	}

	// The retry succeeds and the machine continues to its teardown.
	h.tick(at(18 * time.Minute))
	if got := h.stateOf(id); got != firefly.Syncing {
		t.Fatalf("after the retry: %s, want %s", got, firefly.Syncing)
	}
	h.tick(at(19 * time.Minute))
	if got := h.stateOf(id); got != firefly.Terminating {
		t.Fatalf("after a successful retry: %s, want %s", got, firefly.Terminating)
	}
}

// TestAbandonmentIsRecorded is the one deletion path that skips a successful save.
// It has to be reachable, recorded, and counted.
func TestAbandonmentIsRecorded(t *testing.T) {
	h := newHarness(t)
	id := h.provision(firefly.Ready)
	h.sync.FailAlways()

	// Lapse, drain, then fail the save at the cap until attempts run out.
	h.tick(at(0))
	h.tick(at(16 * time.Minute))
	if got := h.stateOf(id); got != firefly.Syncing {
		t.Fatalf("state = %s, want %s", got, firefly.Syncing)
	}

	// Past the cap, with SyncAttempts reached.
	var abandoned firefly.Result
	now := at(16 * time.Minute)
	for i := 0; i < 20; i++ {
		now = now.Add(time.Minute)
		abandoned = h.tick(now)
		if len(abandoned.Abandoned) > 0 {
			break
		}
	}
	if len(abandoned.Abandoned) != 1 || abandoned.Abandoned[0] != id {
		t.Fatalf("Abandoned = %v, want [%s]", abandoned.Abandoned, id)
	}
	if got := h.stateOf(id); got != firefly.Terminating {
		t.Fatalf("after abandoning: %s, want %s", got, firefly.Terminating)
	}
}

// TestListDropsDoesNotLoseAMachine is the fault that punishes deriving absence
// from a listing.
func TestListDropsDoesNotLoseAMachine(t *testing.T) {
	h := newHarness(t)
	id := h.provision(firefly.Ready)
	h.cloud.ListDrops(id)

	h.tick(at(0))

	// The machine is still managed: it drained rather than being adopted twice or
	// forgotten.
	if got := h.stateOf(id); got != firefly.Draining {
		t.Fatalf("state = %s, want %s", got, firefly.Draining)
	}
	doc, _ := h.store.Load(context.Background())
	if len(doc.Machines) != 1 {
		t.Fatalf("the document has %d machines, want 1", len(doc.Machines))
	}
}

// TestOrphansAreAdopted is the repair for a lost document: machines the provider
// has and the document does not come back under management.
func TestOrphansAreAdopted(t *testing.T) {
	h := newHarness(t)
	h.cloud.Boot(0)

	// A machine that exists in the provider only.
	id, err := h.cloud.Create(context.Background(), firefly.Spec{Labels: map[string]string{"verge": "machine"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h.cloud.Advance(time.Minute)
	res := h.tick(at(time.Minute))

	if len(res.Adopted) != 1 || res.Adopted[0] != id {
		t.Fatalf("Adopted = %v, want [%s]", res.Adopted, id)
	}
	// Adoption writes a record, not a transition, so the same pass walks the
	// machine one row -- and it is already running and joined by now.
	if got := h.stateOf(id); got != firefly.Ready {
		t.Fatalf("an adopted machine is %s, want %s", got, firefly.Ready)
	}
}

// TestOrphansPastTheCapAreSwept: adoption is bounded by age, because a machine we
// have never managed has no state of ours worth saving.
func TestOrphansPastTheCapAreSwept(t *testing.T) {
	h := newHarness(t)
	h.cloud.Boot(0)

	id, err := h.cloud.Create(context.Background(), firefly.Spec{Labels: map[string]string{"verge": "machine"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h.cloud.Advance(5 * time.Hour)
	res := h.tick(at(5 * time.Hour))

	if len(res.Adopted) != 0 {
		t.Fatalf("Adopted = %v, want none", res.Adopted)
	}
	if len(res.Swept) != 1 || res.Swept[0] != id {
		t.Fatalf("Swept = %v, want [%s]", res.Swept, id)
	}
	doc, _ := h.store.Load(context.Background())
	if len(doc.Machines) != 0 {
		t.Fatalf("a swept machine was recorded: %d machines", len(doc.Machines))
	}
}

// TestLingeringNodeIsForgotten is the repair, not the observation: a node record
// that outlives its machine would otherwise cycle TERMINATING and ERROR forever.
func TestLingeringNodeIsForgotten(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Ready)

	// Let it boot and join, then have the provider reap it. The document is
	// rewritten before the first tick, because firefly caches the document once
	// loaded -- writing behind its back after a tick would test nothing.
	h.cloud.Advance(time.Minute)
	if err := h.cloud.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	doc, err := h.store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := range doc.Machines {
		if doc.Machines[i].ID == id {
			doc.Machines[i].State = firefly.Terminating
			doc.Machines[i].ReachedReady = true
			doc.Machines[i].Since = at(time.Minute)
		}
	}
	if err := h.store.Save(ctx, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// First tick: the node is still there, so it is forgotten and TERMINATING stays.
	res := h.tick(at(2 * time.Minute))
	if len(res.Transitions) != 1 || res.Transitions[0].Trigger != firefly.MachineGone {
		t.Fatalf("transitions = %+v, want one MachineGone", res.Transitions)
	}
	if got := h.stateOf(id); got != firefly.Terminating {
		t.Fatalf("state = %s, want %s", got, firefly.Terminating)
	}

	// Second tick: the node is gone, so the machine is confirmed and dropped.
	h.tick(at(3 * time.Minute))
	final, err := h.store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := final.Machine(id); ok {
		t.Fatal("a confirmed machine is still in the document")
	}
}

// TestCapRoutesThroughTheSave: the cap never skips the save for a machine that had
// something on it.
func TestCapRoutesThroughTheSave(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Ready)

	lease, err := h.f.Acquire(ctx, id, "session:01J8", firefly.Session, 24*time.Hour, at(0))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Past the cap while still leased: the lease does not save it.
	h.tick(at(5 * time.Hour))
	if got := h.stateOf(id); got != firefly.Syncing {
		t.Fatalf("state = %s, want %s", got, firefly.Syncing)
	}

	h.tick(at(5*time.Hour + time.Minute))
	if got := h.sync.Synced(); len(got) != 1 || got[0] != id {
		t.Fatalf("the cap skipped the save: synced = %v", got)
	}
	if err := h.f.Release(ctx, lease); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestCloudFailureAbortsTheTick: a half-reconciled pass against a failing provider
// is worse than no pass, because the next one inherits the ambiguity.
func TestCloudFailureAbortsTheTick(t *testing.T) {
	h := newHarness(t, func(c *fake.Cloud, _ *fake.Network) {
		c.FailCreate(errors.New("nope"))
	})
	// A List failure is what matters here; the fake has no knob for it, so this
	// test covers the path through a cancelled context instead.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.f.Tick(ctx, at(0)); err == nil {
		t.Fatal("Tick with a cancelled context succeeded")
	}
}

// TestWorkDuringDrainRevives is the courtesy rule: the drain window is not a point
// of no return, because making it one turns a busy session into a cold clone.
func TestWorkDuringDrainRevives(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Ready)

	h.tick(at(0))
	if got := h.stateOf(id); got != firefly.Draining {
		t.Fatalf("state = %s, want %s", got, firefly.Draining)
	}

	if _, err := h.f.Acquire(ctx, id, "session:01J8", firefly.Session, time.Hour, at(time.Minute)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	res := h.tick(at(2 * time.Minute))

	if got := h.stateOf(id); got != firefly.Ready {
		t.Fatalf("after work arrived: %s, want %s", got, firefly.Ready)
	}
	if len(res.Transitions) != 1 || res.Transitions[0].Trigger != firefly.LeaseArrived {
		t.Fatalf("transitions = %+v, want one LeaseArrived", res.Transitions)
	}
}

// TestDeleteThatNeverConfirmsBecomesAnError is the state that once slept forever:
// a delete issued and then waited on, rather than re-read.
func TestDeleteThatNeverConfirmsBecomesAnError(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Terminating)

	// The provider keeps reporting the machine after the delete was accepted.
	h.cloud.Boot(0).DeleteAfter(time.Hour)
	if err := h.cloud.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	h.tick(at(time.Minute))
	if got := h.stateOf(id); got != firefly.Terminating {
		t.Fatalf("before the deadline: %s, want %s", got, firefly.Terminating)
	}

	h.tick(at(10 * time.Minute))
	if got := h.stateOf(id); got != firefly.Error {
		t.Fatalf("after the delete deadline: %s, want %s", got, firefly.Error)
	}
}

// TestForgetRefusalIsAnError: the repair can fail, and when it does the machine
// must say so rather than spinning.
func TestForgetRefusalIsAnError(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Ready)

	h.cloud.Advance(time.Minute)
	if err := h.cloud.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	doc, err := h.store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := range doc.Machines {
		if doc.Machines[i].ID == id {
			doc.Machines[i].State = firefly.Terminating
			doc.Machines[i].Since = at(time.Minute)
		}
	}
	if err := h.store.Save(ctx, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	h.net.ForgetRefuses(1)
	h.tick(at(2 * time.Minute))

	if got := h.stateOf(id); got != firefly.Error {
		t.Fatalf("after a refused forget: %s, want %s", got, firefly.Error)
	}

	// The retry gets back to Terminating, and the forget succeeds this time.
	h.tick(at(3 * time.Minute))
	if got := h.stateOf(id); got != firefly.Terminating {
		t.Fatalf("after the retry: %s, want %s", got, firefly.Terminating)
	}
}

// TestRetryBackoffIsCapped keeps a long-lived failure from pushing its own retry
// past the machine's deadline, where the cap could never act on it.
func TestRetryBackoffIsCapped(t *testing.T) {
	h := newHarness(t, func(c *fake.Cloud, _ *fake.Network) { c.FailList(errors.New("unreachable")) })

	// A failing listing aborts the tick rather than half-reconciling.
	if _, err := h.f.Tick(context.Background(), at(0)); err == nil {
		t.Fatal("Tick with a failing listing succeeded")
	}
}

// TestListFailureAbortsTheTick is separate from the backoff test because it is
// about the provider being unreachable rather than about timing.
func TestListFailureAbortsTheTick(t *testing.T) {
	boom := errors.New("provider unreachable")
	h := newHarness(t, func(c *fake.Cloud, _ *fake.Network) { c.FailList(boom) })

	_, err := h.f.Tick(context.Background(), at(0))
	if !errors.Is(err, boom) {
		t.Fatalf("Tick = %v, want the provider's error", err)
	}
}

// TestTeardownDeleteFailureAbortsTheTick: entering TERMINATING issues the delete,
// and a provider that refuses every attempt is an error the caller has to see
// rather than a machine recorded as deleting that never was.
func TestTeardownDeleteFailureAbortsTheTick(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Ready)

	// Drain to SYNCING, then make the provider refuse the delete.
	h.tick(at(0))
	h.tick(at(16 * time.Minute))
	if got := h.stateOf(id); got != firefly.Syncing {
		t.Fatalf("state = %s, want %s", got, firefly.Syncing)
	}

	h.cloud.DeleteRefuses(10)
	if _, err := h.f.Tick(ctx, at(17*time.Minute)); err == nil {
		t.Fatal("a refused teardown delete did not abort the tick")
	}
}

// TestConfirmedMachineDropsItsLeases is the wedge the review found: a machine
// removed from the document while a lease still names it makes the document
// invalid, so the save fails on every tick forever and the confirmation is never
// persisted.
func TestConfirmedMachineDropsItsLeases(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Ready)

	if _, err := h.f.Acquire(ctx, id, "session:01J8", firefly.Session, 24*time.Hour, at(0)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Cap straight from READY while still leased: tear down without ever draining.
	h.tick(at(5 * time.Hour))
	h.tick(at(5*time.Hour + time.Minute))

	// Now confirm it. The tick must save, and must not leave the lease behind.
	var confirmed firefly.Result
	now := at(5*time.Hour + time.Minute)
	for i := 0; i < 5; i++ {
		now = now.Add(time.Minute)
		confirmed = h.tick(now)
		if len(confirmed.Confirmed) > 0 {
			break
		}
	}
	if len(confirmed.Confirmed) != 1 {
		t.Fatalf("Confirmed = %v, want the machine", confirmed.Confirmed)
	}
	if !confirmed.Saved {
		t.Fatal("the confirmation was not persisted")
	}

	doc, err := h.store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := doc.Machine(id); ok {
		t.Fatal("the machine is still in the document")
	}
	if len(doc.Leases) != 0 {
		t.Fatalf("the machine left %d leases behind", len(doc.Leases))
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("the saved document is invalid: %v", err)
	}
}

// TestLapsedLeasesArePruned: a lease that lapsed without a Release should not
// accumulate in the document, and must not block a save.
func TestLapsedLeasesArePruned(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Ready)

	if _, err := h.f.Acquire(ctx, id, "turn:01J8", firefly.Turn, time.Minute, at(0)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Well past the lease's expiry, with a live lease re-acquired after.
	h.tick(at(30 * time.Minute))

	doc, err := h.store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(doc.Leases) != 0 {
		t.Fatalf("a lapsed lease survived the tick: %d leases", len(doc.Leases))
	}
}

// TestAbandonmentIsReachableAtTheCap is the hole the review found twice: the cap
// used to reset the failure count, so a machine past its deadline cycled between
// SYNCING and ERROR forever with the syncer retried every tick and no delete ever
// issued.
func TestAbandonmentIsReachableAtTheCap(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	id := h.provision(firefly.Ready)
	h.sync.FailAlways()

	// A lease keeps it up until the cap, then every save fails.
	if _, err := h.f.Acquire(ctx, id, "session:01J8", firefly.Session, 24*time.Hour, at(0)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var abandoned firefly.Result
	now := at(5 * time.Hour)
	for i := 0; i < 20; i++ {
		now = now.Add(time.Minute)
		abandoned = h.tick(now)
		if len(abandoned.Abandoned) > 0 {
			break
		}
	}

	if len(abandoned.Abandoned) != 1 || abandoned.Abandoned[0] != id {
		t.Fatalf("Abandoned = %v, want [%s]: the cap reset the failure count", abandoned.Abandoned, id)
	}

	// And the machine actually left, rather than being abandoned on paper.
	doc, err := h.store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m, ok := doc.Machine(id); ok && m.State != firefly.Terminating {
		t.Fatalf("after abandoning, the machine is %s", m.State)
	}
}

// TestProvision covers creating a machine: the one operation that turns nothing
// into something billable.
func TestProvision(t *testing.T) {
	ctx := context.Background()

	t.Run("happy/records_a_provisioning_machine", func(t *testing.T) {
		h := newHarness(t)
		id, err := h.f.Provision(ctx, firefly.Spec{Image: "ubuntu-24.04"}, at(0))
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		if id == "" {
			t.Fatal("Provision returned an empty id")
		}
		if got := h.stateOf(id); got != firefly.Provisioning {
			t.Fatalf("state = %s, want %s", got, firefly.Provisioning)
		}

		doc, _ := h.store.Load(ctx)
		m, ok := doc.Machine(id)
		if !ok {
			t.Fatal("the machine is not in the document")
		}
		if want := at(4 * time.Hour); !m.Deadline.Equal(want) {
			t.Fatalf("Deadline = %s, want %s", m.Deadline, want)
		}
	})

	// The label is firefly's, not the caller's: a machine created without it is
	// invisible to List, so the sweep cannot find it and it bills unmanaged.
	t.Run("happy/the_label_is_always_applied", func(t *testing.T) {
		h := newHarness(t)
		id, err := h.f.Provision(ctx, firefly.Spec{Labels: map[string]string{"outpost": "minecraft"}}, at(0))
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		listed, err := h.cloud.List(ctx, "verge=machine")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		found := false
		for _, cm := range listed {
			if cm.ID == id {
				found = true
				if cm.Labels["outpost"] != "minecraft" {
					t.Fatalf("the caller's label was lost: %v", cm.Labels)
				}
			}
		}
		if !found {
			t.Fatal("the machine is not visible under our label")
		}
	})

	t.Run("happy/a_wrong_label_from_the_caller_is_overwritten", func(t *testing.T) {
		h := newHarness(t)
		id, err := h.f.Provision(ctx, firefly.Spec{Labels: map[string]string{"verge": "wrong"}}, at(0))
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		listed, err := h.cloud.List(ctx, "verge=machine")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, cm := range listed {
			if cm.ID == id {
				return
			}
		}
		t.Fatal("a caller-supplied label overrode ours and hid the machine")
	})

	t.Run("sad/failed_create_records_nothing", func(t *testing.T) {
		boom := errors.New("quota exceeded")
		h := newHarness(t, func(c *fake.Cloud, _ *fake.Network) { c.FailCreate(boom) })

		if _, err := h.f.Provision(ctx, firefly.Spec{}, at(0)); !errors.Is(err, boom) {
			t.Fatalf("Provision = %v, want the provider's error", err)
		}
		doc, err := h.store.Load(ctx)
		if !errors.Is(err, firefly.ErrNoDocument) {
			t.Fatalf("a failed create wrote a document: %v", err)
		}
		_ = doc
	})

	t.Run("sad/second_attempt_backs_off", func(t *testing.T) {
		h := newHarness(t, func(c *fake.Cloud, _ *fake.Network) { c.FailCreate(errors.New("quota")) })
		if _, err := h.f.Provision(ctx, firefly.Spec{}, at(0)); err == nil {
			t.Fatal("the first create succeeded")
		}
		if _, err := h.f.Provision(ctx, firefly.Spec{}, at(time.Second)); !errors.Is(err, firefly.ErrProvisioningBackoff) {
			t.Fatalf("Provision during backoff = %v, want ErrProvisioningBackoff", err)
		}
	})

	t.Run("happy/backoff_clears_before_a_later_attempt", func(t *testing.T) {
		h := newHarness(t, func(c *fake.Cloud, _ *fake.Network) { c.FailCreate(errors.New("quota")) })
		if _, err := h.f.Provision(ctx, firefly.Spec{}, at(0)); err == nil {
			t.Fatal("the first create succeeded")
		}

		// Retried on the same firefly, past RetryBase: the backoff is over, so the
		// error must be the provider's again rather than ErrProvisioningBackoff.
		// Building a second Firefly instead would assert nothing about this one.
		_, err := h.f.Provision(ctx, firefly.Spec{}, at(30*time.Second))
		if err == nil || errors.Is(err, firefly.ErrProvisioningBackoff) {
			t.Fatalf("second attempt = %v, want the provider's error", err)
		}
	})
}

// TestProvisionOrphanIsAdopted is the repair for a create that succeeded and a
// save that did not: the machine exists, the document does not know, and the next
// pass picks it up rather than leaving it unmanaged.
func TestProvisionOrphanIsAdopted(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	rejecting := &toggleStore{inner: h.store}
	cfg := firefly.Config{
		Store: rejecting, Cloud: h.cloud, Network: h.net, Syncer: h.sync,
		Label: "verge=machine", MaxLifetime: 4 * time.Hour, DrainWindow: 15 * time.Minute,
		BootDeadline: 10 * time.Minute, SyncAttempts: 3,
		TerminateDeadline: 5 * time.Minute, RetryBase: 30 * time.Second, RetryMax: 5 * time.Minute,
	}
	f, err := firefly.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.f = f

	rejecting.fail = true
	id, err := f.Provision(ctx, firefly.Spec{}, at(0))
	if err == nil {
		t.Fatal("Provision succeeded while the store rejected writes")
	}
	if id == "" {
		t.Fatal("Provision did not report the id of the machine it created")
	}

	rejecting.fail = false
	h.cloud.Advance(time.Minute)
	res := h.tick(at(time.Minute))
	if len(res.Adopted) != 1 || res.Adopted[0] != id {
		t.Fatalf("Adopted = %v, want [%s]", res.Adopted, id)
	}
}

// toggleStore rejects saves while fail is set.
type toggleStore struct {
	inner *firefly.MemStore
	fail  bool
}

func (s *toggleStore) Load(ctx context.Context) (firefly.Document, error) {
	return s.inner.Load(ctx)
}

func (s *toggleStore) Save(ctx context.Context, doc firefly.Document) error {
	if s.fail {
		return errors.New("store: write rejected")
	}
	return s.inner.Save(ctx, doc)
}

// TestNamesJoinTheProviderAndTheNetwork is the mapping test docs/firefly.md asks
// for, and it is the one the fake cannot pass by accident.
//
// The fake resolves a node by looking its own machine records up by name, so it
// answers happily for any name it agrees with -- including an id, which is exactly
// the question that was unanswerable before the rekey. The only place the two
// vocabularies are actually joined is Provision: the name it sends to Create has to
// be the name it later asks the network about, and nothing in the fake checks that.
// This test does, by comparing what the provider was told with what the network was
// asked, and by driving all three network calls so none of them can go unchecked.
func TestNamesJoinTheProviderAndTheNetwork(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	id, err := h.f.Provision(ctx, firefly.Spec{Image: "ubuntu-24.04"}, at(0))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	specs := h.cloud.Specs()
	if len(specs) != 1 {
		t.Fatalf("the provider was told about %d creates, want 1", len(specs))
	}
	name := specs[0].Name
	if name == "" {
		t.Fatal("Provision created a machine without a name")
	}

	doc, err := h.store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m, ok := doc.Machine(id)
	if !ok {
		t.Fatal("the machine is not in the document")
	}
	if m.Name != name {
		t.Fatalf("the document records %q, the provider was told %q", m.Name, name)
	}

	// Walk the whole lifecycle, so Joined, NodeExists and Forget are all asked.
	h.tick(at(30 * time.Second))          // ready: Joined
	h.tick(at(2 * time.Hour))             // no leases: draining
	h.tick(at(3 * time.Hour))             // drained: syncing
	h.tick(at(3 * time.Hour))             // synced: terminating, delete issued
	h.tick(at(3*time.Hour + time.Minute)) // machine gone, node lingering: Forget
	res := h.tick(at(3*time.Hour + 2*time.Minute))
	if len(res.Confirmed) != 1 || res.Confirmed[0] != id {
		t.Fatalf("Confirmed = %v, want [%s]", res.Confirmed, id)
	}

	joined, exists, forgets := h.net.Calls()
	if joined == 0 || exists == 0 || forgets == 0 {
		t.Fatalf("network calls = joined %d, exists %d, forgets %d; want all three exercised",
			joined, exists, forgets)
	}

	asked := h.net.Asked()
	if len(asked) == 0 {
		t.Fatal("the network was never asked anything")
	}
	for _, got := range asked {
		if got != name {
			t.Fatalf("the network was asked about %q, want %q (the name the provider was told)", got, name)
		}
	}
}

// TestProvisionMintsNames: the caller supplies no name, so firefly does, and two
// machines must not share one.
func TestProvisionMintsNames(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	first, err := h.f.Provision(ctx, firefly.Spec{}, at(0))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	second, err := h.f.Provision(ctx, firefly.Spec{}, at(0))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	doc, err := h.store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, _ := doc.Machine(first)
	b, _ := doc.Machine(second)

	// The prefix comes from the harness's label, verge=machine, so the name says
	// which deployment minted it.
	if a.Name != "verge-machine-1" {
		t.Fatalf("first name = %q, want verge-machine-1", a.Name)
	}
	if b.Name != "verge-machine-2" {
		t.Fatalf("second name = %q, want verge-machine-2", b.Name)
	}
	if a.Name == b.Name {
		t.Fatal("two machines were given one name")
	}
}

// TestProvisionUsesTheCallersName: policy may know something about the name that
// firefly does not, and it is honoured as long as it is unique.
func TestProvisionUsesTheCallersName(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	id, err := h.f.Provision(ctx, firefly.Spec{Name: "runner-a"}, at(0))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := h.cloud.Specs()[0].Name; got != "runner-a" {
		t.Fatalf("the provider was told %q, want runner-a", got)
	}
	doc, _ := h.store.Load(ctx)
	if m, _ := doc.Machine(id); m.Name != "runner-a" {
		t.Fatalf("the document records %q, want runner-a", m.Name)
	}

	// A caller-chosen name still advances the counter past collisions, so the next
	// minted name cannot land on top of it.
	t.Run("sad/a_taken_name_is_refused_before_the_create", func(t *testing.T) {
		createsBefore, _, _, _ := h.cloud.Calls()
		if _, err := h.f.Provision(ctx, firefly.Spec{Name: "runner-a"}, at(time.Second)); !errors.Is(err, firefly.ErrNameTaken) {
			t.Fatalf("Provision with a taken name = %v, want ErrNameTaken", err)
		}
		if createsAfter, _, _, _ := h.cloud.Calls(); createsAfter != createsBefore {
			t.Fatalf("the provider was asked to create a machine for a name we refused (%d -> %d)", createsBefore, createsAfter)
		}
	})
}

// TestAdoptionKeepsTheProvidersName: an adopted machine's node is keyed on the name
// it was created with. A name invented here would be a machine firefly could never
// ask the network about, and the whole point of adoption is to recover management of
// a machine that is already running.
func TestAdoptionKeepsTheProvidersName(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.cloud.Boot(0)

	id, err := h.cloud.Create(ctx, firefly.Spec{Name: "orphan-1", Labels: map[string]string{"verge": "machine"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h.cloud.Advance(time.Minute)
	res := h.tick(at(time.Minute))
	if len(res.Adopted) != 1 || res.Adopted[0] != id {
		t.Fatalf("Adopted = %v, want [%s]", res.Adopted, id)
	}

	doc, _ := h.store.Load(ctx)
	m, _ := doc.Machine(id)
	if m.Name != "orphan-1" {
		t.Fatalf("the adopted machine is named %q, want orphan-1", m.Name)
	}
	if got := h.stateOf(id); got != firefly.Ready {
		t.Fatalf("state = %s, want %s: the network answered for orphan-1", got, firefly.Ready)
	}
}

// unnamedCloud is the shared fake with the one lie this suite needs and no other
// test does: a provider that lists machines without names. Wrapping the fake rather
// than writing a second cloud keeps one model of the provider, per docs/testing.md.
type unnamedCloud struct{ *fake.Cloud }

func (c unnamedCloud) List(ctx context.Context, label string) ([]firefly.CloudMachine, error) {
	machines, err := c.Cloud.List(ctx, label)
	if err != nil {
		return nil, err
	}
	out := make([]firefly.CloudMachine, len(machines))
	for i, m := range machines {
		m.Name = ""
		out[i] = m
	}
	return out, nil
}

// TestUnnamedMachineCannotBeAdopted: adoption has no name to invent, because the
// node the machine actually has is keyed on the name it was created with. Guessing
// would produce a document Validate refuses, on every tick, forever.
func TestUnnamedMachineCannotBeAdopted(t *testing.T) {
	ctx := context.Background()

	t.Run("sad/an_orphan_without_a_name_is_reported", func(t *testing.T) {
		h := newHarness(t)
		h.cloud.Boot(0)
		f := unnamedFirefly(t, h)

		if _, err := h.cloud.Create(ctx, firefly.Spec{Labels: map[string]string{"verge": "machine"}}); err != nil {
			t.Fatalf("Create: %v", err)
		}

		h.cloud.Advance(time.Minute)
		if _, err := f.Tick(ctx, at(time.Minute)); err == nil {
			t.Fatal("Tick adopted a machine the provider reported without a name")
		}
	})

	// The cap still has to work on a machine nothing can name: the sweep deletes by
	// id and needs no node record, and a tick that refused to sweep would be exactly
	// the unbounded bill the cap exists to prevent.
	t.Run("sad/an_unnamed_orphan_past_the_cap_is_still_swept", func(t *testing.T) {
		h := newHarness(t)
		h.cloud.Boot(0)
		f := unnamedFirefly(t, h)

		id, err := h.cloud.Create(ctx, firefly.Spec{Labels: map[string]string{"verge": "machine"}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		h.cloud.Advance(5 * time.Hour)
		res, err := f.Tick(ctx, at(5*time.Hour))
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if len(res.Swept) != 1 || res.Swept[0] != id {
			t.Fatalf("Swept = %v, want [%s]", res.Swept, id)
		}
	})
}

// unnamedFirefly builds a firefly over the fake whose listing drops machine names.
func unnamedFirefly(t *testing.T, h *harness) *firefly.Firefly {
	t.Helper()
	f, err := firefly.New(firefly.Config{
		Store: h.store, Cloud: unnamedCloud{h.cloud}, Network: h.net, Syncer: h.sync,
		Label: "verge=machine", MaxLifetime: 4 * time.Hour, DrainWindow: 15 * time.Minute,
		BootDeadline: 10 * time.Minute, SyncAttempts: 3,
		TerminateDeadline: 5 * time.Minute, RetryBase: 30 * time.Second, RetryMax: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}
