# Firefly

The machinery that keeps exactly as many machines alive as the work needs, and no more.

Firefly is one package, `internal/firefly`, with no knowledge of sessions, repositories, models,
or money. It answers a single question on a loop: *given the leases currently held and the
machines that currently exist, what must change?* Everything else in this repository argues with
it through three narrow interfaces, and it is handed the current time rather than looking it up.

The transition table, the lease arithmetic, the durable document with its in-memory store, the lease
registry, both boundary interfaces including the syncer, and the reconcile loop (`Tick`) are
implemented in `internal/firefly`, with a fake for each outside world. The real Hetzner and tailnet
clients and the R2 store are not, and nothing provisions a machine yet: `Tick` reconciles the machines
the document already holds.
`docs/testing.md` names the files the remainder will become, and every new behaviour still starts as
a failing test.

One word on vocabulary: this document says **machine**, the product vocabulary of `README.md`.
Where `docs/testing.md` says *instance* it means the same thing; the property names there are left
as written rather than renamed, because renaming a property breaks the history of what it caught.

## Where the name comes from

The name is older than this package. Firefly was a pattern used before any of this existed: a server
that appears when it is needed, does its work, and vanishes, described in three phases — **ignite,
illuminate, fade** — written first for a game server and a coding runner. A specification for it
exists in another repository, in TypeScript, built around a provider interface and an idle detector
fed by activity signals.

This is not that specification, and not a port of it. It is the same idea, redesigned and written
fresh in Go. Every difference below is a decision rather than an unfinished step:

| Then | Here | Why |
|---|---|---|
| idle detected from activity signals | leases with TTLs, held by whatever needs the machine | A signal that nobody is *looking* cannot tell "the agent is thinking" from "abandoned". That is the failure the previous design died of. |
| a provider interface each consumer implements | `Cloud` and `Network`, queried rather than pushed | A provider that calls back into the loop owns part of the loop's timing. Asking is testable; being told is not. |
| callbacks, timers, and a background daemon | one reconcile loop over an injected clock | Nothing is delivered; everything is asked for at a time the caller controls, which is what keeps the suite fast. |
| sync on shutdown, by convention | sync before delete, enforced by the transition table | A convention is a thing a new code path forgets. |
| ignite / illuminate / fade | seven states | Three phases describe the happy path. The rest of the states name what goes wrong *between* them, which is where the cost is. |

The three phases are still legible inside the states — `PROVISIONING` and `READY` are ignite,
`READY` through `DRAINING` is illuminate, `SYNCING` to `ABSENT` is fade — but the phases are prose
and the states are the mechanism.

**Nothing here imports that specification, answers to it, or is obliged to stay compatible with
it.** That specification, and the plan around it, are design history in a private repository; this
package is design that stands on its own, which is the only kind this repository carries. The
lineage is worth knowing because the name is inherited, not because the contract is.

One consequence: `internal/firefly` stays inside this repository until it has a second consumer. Its
first shape is Verge's shape, deliberately. A module extracted before its shape is known is an API
invented for an imaginary caller.

## The boundary

The distinction that makes this package testable is **mechanism versus policy**, drawn once and
never crossed.

**Firefly is the mechanism.** It owns:

- the lifecycle state of every machine it created
- lease arithmetic: which leases are live, and when the last one lapses
- the reconcile loop, and the transition table it walks
- the ordering guarantee: state is saved before a machine is destroyed
- retry, backoff, and the record of what failed

**The caller is the policy.** It owns:

- who holds a lease, for how long, and under what name
- how long a machine may linger after its last lease (`DrainWindow`)
- how long any machine may live at all (`MaxLifetime`)
- how many sessions a machine may hold before another is provisioned
- which model, which repository, which provider key

Concretely: firefly will never be passed a session ID. It is passed a *holder name* — an opaque
string like `session:01J8…`, `turn:01J8…/7`, `sub:01J8…:repo/pr#12` — and it returns counts. When
firefly needs to know *why* a lease exists it reads the name, and the name is a label, not a
foreign key. Nothing in firefly joins to anything.

The test of this boundary: the fake cloud in `docs/testing.md` can boot a machine that runs no
agent at all, and every firefly test still passes. If a test needs a repository cloned to pass,
the boundary has leaked.

```go
type Firefly interface {
    // Tick reconciles once, at now. The only method that changes cloud state on
    // its own; Destroy below is an instruction, not a step it decides to take.
    Tick(ctx context.Context, now time.Time) (Result, error)

    // Leases. Acquire and Renew are the only ways to make a machine stay.
    //
    // Acquire names the machine, which the first version of this document forgot:
    // a lease that cannot say which machine it holds open cannot be counted
    // against one. Placement still belongs to the caller — it has already chosen
    // the machine by the time it asks.
    Acquire(machine MachineID, holder string, kind Kind, ttl time.Duration, now time.Time) (LeaseID, error)
    Renew(id LeaseID, ttl time.Duration, now time.Time) error
    Release(id LeaseID) error

    // Snapshot is read-only and safe to call from anywhere, but it reports live
    // counts, so it needs the clock like everything else -- and it can fail,
    // because it may be the first call that has to read the document. A snapshot
    // that silently returns nothing because storage is unreachable is worse than
    // one that says so.
    Snapshot(ctx context.Context, now time.Time) (Snapshot, error)

    // Destroy is the operator's override, not part of the normal path.
    Destroy(ctx context.Context, machine MachineID, reason string) error
}
```

`now` is a parameter rather than a call, for the same reason every other duration in this
repository is injected: a four-minute boot is instant in a test, and there is no path through the
integration tier that sleeps. There is deliberately no `Clock` interface — a clock you can ask for
the time is a clock that half the code will ask, and the tests would then have to install one to
control what they could have just passed. `Destroy` needs no clock because its deadline comes from
its `context`.

## Two lifetimes

The README promises two, and they are independent. Getting this wrong is how the previous design
died: `daemon.sh` polled a timestamp file, so a machine could not tell "the agent is thinking" from
"nobody has looked at this in an hour".

**Session lifetime** belongs to the caller. A session stops when it has nothing left to do — the
agent finished a turn, or is waiting on a human, or errored. Firefly has no opinion. It sees a
holder named `session:…` release its lease.

**Machine lifetime** belongs to firefly. A machine exists while at least one lease on it is live,
plus a drain window, and never past its hard cap.

```
time ─────────────────────────────────────────────────────►

leases    ████████████████████
drain                        ████████
machine   ████████████████████████████

hard cap  ├───────────────────────────────────────┤
          a ceiling, not a duration: past it the machine dies
          even with a live lease, and always through a sync
```

Three lease kinds, with different natural TTLs, so the machine outlives the turn that is currently
running it:

| Kind | Holder lifetime | Typical TTL |
|---|---|---|
| `Turn` | one agent turn, renewed by stream activity | minutes |
| `Session` | the session exists and may be resumed | hours |
| `Subscription` | a PR or issue the session is watching | days |

A turn lease lapsing does not end a session. A session lease lapsing starts the drain window, so a
session between turns does not pay for a teardown and a fresh clone. The drain window lapsing
destroys the machine.

`MaxLifetime` sits above all of it and is not negotiable by a lease. A machine that is renewed
forever still dies. This is the only thing standing between a bug and a month-long bill.

## Leases

A lease is a record, not a goroutine. It has no expiry callback, no timer, and no cleanup:

```go
type Lease struct {
    ID      LeaseID       // minted by firefly, opaque
    Machine MachineID     // which machine this keeps alive
    Holder  string        // policy's name; never parsed by firefly
    Kind    Kind          // Turn | Session | Subscription
    Start   time.Time     // set by firefly from now, never by the caller
    TTL     time.Duration
}
```

`Machine` was missing from the first version of this document, and writing the store found it:
the design talks throughout about leases *on an instance* — `Live(instance)`, "the last lease
lapses" — while the record had no way to say which instance. A lease that cannot name its machine
cannot be counted against one, pruned with one, or validated against a state document.

The struct is what is stored and read; `Acquire` takes the four facts separately, because a caller
that supplies its own `Start` could backdate a lease, and there is no reason to allow it. A lease
is the one thing in this package that outlives the process, so it is a plain record with no
receiver and no behaviour.

Live is arithmetic at read time: the lease is live for exactly `[Start, Start+TTL)`. Expiry is
therefore a property of the clock, not an event that must be delivered — which is what lets
`Live(machine)` be monotonic as the clock advances with no renewal, and what makes `Tick` idempotent
under a frozen clock. Both are properties in `docs/testing.md`; this is the design decision that
earns them.

`Renew` sets `Start = now` and replaces the TTL. It extends from *now*, never from the previous
expiry, so a renewal arriving after a lapse cannot reach backwards and resurrect a lease that has
already been counted as gone. A renewal for a lapsed lease is a `sad/` case, not an error worth
waking anyone for: the session re-acquires.

`Release` is idempotent. A second release of the same ID is not an error — a crashed turn and a
supervisor racing to clean up will both call it, and one of them is not a bug. `Release` on an
unknown, already-released, or pruned ID is `nil` for the same reason.

A lease record outlives its liveness and must not outlive its usefulness. Expired records are
pruned by `Tick` once they are past `Start+TTL` and no machine's teardown still depends on them, so
the state document does not grow without bound. Until pruned they are inert: `Live` ignores them,
so a stale record cannot hold a machine open.

`Renew` on a lapsed lease returns `ErrLeaseExpired` rather than quietly re-acquiring — and it still
does after `Tick` has pruned the record, because ids are minted from a counter: an id at or below
`NextLeaseNumber` existed and is gone, while one above it was never issued. The id is opaque to
callers and not opaque to the thing that minted it, which is what keeps the ordinary lapse path from
reporting the error callers are told to treat as a bug. The
distinction matters — a silent re-acquire would let a supervisor hold a machine open with a lease
the caller believes it lost — and the `sad/` case exists to assert the error rather than the
behaviour.

Leases are durable. They live in the same state document as the machine records, so a restart of
the orchestrator does not destroy every machine. On start, firefly reads leases back and reconciles
them against the cloud; a lease whose machine no longer exists is dropped, and one whose machine
exists is honoured.

## The state machine

Seven states, one per meaningful thing firefly can be wrong about.

| State | Meaning |
|---|---|
| `ABSENT` | no machine exists. The start, and the end. |
| `PROVISIONING` | a create was issued; no machine has answered yet. |
| `READY` | the machine booted, answered its metadata service, and joined the private network. |
| `DRAINING` | no live leases; the drain window is running. |
| `SYNCING` | saving the machine's state to object storage. |
| `TERMINATING` | a delete was issued; the machine may or may not be gone. |
| `ERROR` | a step failed. The machine still exists. A retry is scheduled. |

`ERROR` is not a terminal state and is never a resting place. Each `ERROR` carries `LastErr`,
`Attempt`, and `RetryAfter` — a clock time, not a duration, so the retry decision is the same
arithmetic as everything else.

The transitions are **data**, in a table, per `docs/testing.md`:

```
ABSENT      --Need-->              PROVISIONING
ABSENT      --CreateFailed-->      ABSENT           (backoff recorded; nothing to describe)
PROVISIONING --Created-->          PROVISIONING     (id recorded, not yet answering)
PROVISIONING --Answered-->         READY
PROVISIONING --Stalled-->          ERROR            (boot exceeded BootDeadline)
READY       --LeasesLapsed-->      DRAINING
DRAINING    --LeaseArrived-->      READY            (the drain window is not a point of no return)
DRAINING    --Drained-->           SYNCING
SYNCING     --Synced-->            TERMINATING
SYNCING     --SyncFailed-->        ERROR            (and the machine stays)
SYNCING     --Abandoned-->         TERMINATING      (cap reached, save recorded as lost)
TERMINATING --MachineGone-->       TERMINATING      (machine half done; Forget the node if it remains)
TERMINATING --Confirmed-->         ABSENT            (machine gone and node gone)
TERMINATING --Unconfirmed-->       ERROR            (deadline passed; re-check, never wait)
ERROR       --RetryDue-->          <the state it came from>
cappable    --Capped-->            SYNCING          (see below: not every state is cappable)
```

Three rows exist because of named failures and are not optional:

- **`TERMINATING --Unconfirmed--> ERROR`.** `TERMINATING` is the state that once slept forever: a
  delete was issued and the code then waited on a machine that had already gone. A machine in
  `TERMINATING` is on a deadline, and the next `Tick` after that deadline re-reads the cloud rather
  than waiting. `TERMINATING` never blocks.
- **`DRAINING --LeaseArrived--> READY`.** The drain window is a courtesy, not a commitment. Work
  arriving during it costs nothing to accept, and a design that makes it a one-way door turns a
  busy session into a cold clone.
- **`TERMINATING --MachineGone--> TERMINATING`.** A 404, or a `Describe` that no longer finds the
  machine, resolves the *machine* half of deletion and nothing else. It is not an exit: the node
  record can outlive the machine, and only `--Confirmed-->` leaves.

`ERROR --RetryDue--> <origin>` is the one row that is not a pair of literals. `ERROR` records the
state it came from, so the table holds one concrete row per origin and the transition gate still
loops over named cases with nothing wildcarded. A retry that cannot name its origin is a bug in the
record, not a case to handle.

`ABSENT --CreateFailed--> ABSENT` exists because `ERROR` cannot hold it. `ERROR` means a step failed
and *the machine still exists*; a create that fails creates nothing. So the attempt count, the
backoff, and the last error live on the machine *request* rather than on a machine record, `ABSENT`
stays `ABSENT`, and the caller logs `FailCreate` from the request. A state that pretends a machine
exists in order to remember an error about a machine that does not exist is how the next `Tick`
ends up sending `Describe` for an ID that was never assigned.

`cappable --Capped--> SYNCING` is deliberate, and `cappable` is a predicate over the machine rather
than a set of states, because one state does not mean one thing:

```
cappable(m) = m.state ∈ {PROVISIONING, READY, DRAINING}
            ∨ (m.state == ERROR ∧ m.origin ≠ TERMINATING)
```

`ABSENT` has nothing to cap, and `SYNCING` is already doing the thing the cap wants. `TERMINATING`,
and every `ERROR` whose origin is `TERMINATING`, is a *delete in flight* — capping it would un-issue
the delete, and an `ERROR(TERMINATING)` routed into `SYNCING` could then reach `--Abandoned-->` and
log a data loss and increment the abandonment counter for a machine whose save had already
succeeded. That single false positive is enough to turn the counter from an alarm into noise. A
delete in flight has its own deadline and its own exit, `--Unconfirmed-->` then `--RetryDue-->`, so
it needs nothing from the cap.

Two rules make that route terminate instead of loop, and both matter because a machine that cannot
be destroyed is a machine that bills until someone notices:

- **A machine that never reached `READY` has nothing to save.** Its sync succeeds trivially — no
  workspace, no session — so `PROVISIONING --Stalled--> ERROR` reaches `TERMINATING` through a
  `--Capped-->` and a no-op sync. Without this, a boot that never answers would be undestroyable by
  design, which is the opposite of what the hard cap is for.
- **A sync that keeps failing past the cap abandons the save.** `SYNCING --SyncFailed--> ERROR`
  retries to a bounded `SyncAttempts`, and once the cap has passed the machine takes
  `SYNCING --Abandoned--> TERMINATING`: it is deleted, and the abandonment is *recorded* — a
  data-loss warning naming what was not saved, and the abandonment counter, labelled
  `reason=sync_failed`.

That second rule is a real hole in the ordering property, so it is named as one rather than
implied. See "Where the ordering property yields".

## Two state machines, deliberately

The fake cloud in `docs/testing.md` is also a state machine, and it is a *different* one on purpose:

```
provider view:   absent → created → starting → running → stopping → deleted
firefly view:    ABSENT → PROVISIONING → READY → DRAINING → SYNCING → TERMINATING → ABSENT
```

The provider's states are facts it reports; firefly's are decisions it has made. They are not a
renaming of each other, and collapsing them would hide the two things that actually go wrong: a
provider that says `running` to a machine that has not answered its metadata service (firefly is
still `PROVISIONING`), and a provider that says `deleted` to a node record that still exists
(firefly is still `TERMINATING`). The contract tier records the provider's sequence as a golden log
and the fake replays it; firefly's sequence is asserted separately, from the event log it produces.

## Readiness is two facts

The metadata answer is the readiness signal, not CI exit status or a log line. A provisioned machine
is half-ready when `169.254.169.254/hetzner/v1/metadata/instance-id` answers from inside it — the
one part of the old boot sequence that survived wholesale.

The other half is **joining the private network**. `README.md` lists the failures worth catching,
and one of them is *a node that lingers on a private network*; the contract tier in
`docs/testing.md` says *boots, joins, leases, syncs, destroys* for the same reason. So readiness is
conjunctive, and each half has its own way of lying:

| Symptom | Fact | Read by | Fault |
|---|---|---|---|
| never leaves `starting` | cloud-init never finished | `Cloud.Describe` | `StallBoot` |
| running but never joins | the metadata service never came up, so the mesh agent never started | `Network.Joined` | `MetadataMissing` |

Both columns matter. A guarantee that names a fact no interface can read is not a guarantee, it is
a wish — which is why the `Network` interface in "What firefly depends on" exists at all.

The second row is a correction worth keeping visible: an earlier version said `Cloud.Describe` read
the metadata fact, and it cannot. The provider API describes the machine, not what is running inside
it. The observable consequence of a metadata service that never answered is a machine that is
`running` and never joins — which is exactly what `Network.Joined` can see. So the split is one fact
per interface, and diagnosing *why* a machine never joined is a human reading its console log,
which is a fine thing for a human to do and a poor thing to gate a state machine on.

A machine that boots and never answers is *not* `ABSENT`: it exists, it is billing, and it must be
swept.

**Deletion is two facts as well.** A machine can be gone from the provider and still be a member of
the network — its node record outlives it, and a tailnet accumulates those quietly. `TERMINATING`
reaches `ABSENT` only when `Cloud.Describe` confirms the machine is gone *and* `Network.NodeExists`
returns false. Until then it is `TERMINATING`, and past its deadline, `ERROR` with a `LastErr` naming
which half is outstanding. A `DELETE` that returns 404 resolves the machine half and still has work
to do.

**Observing a lingering node is not repairing it.** This is the gap that makes the previous
paragraph either a guarantee or a trap, and it has to be closed in both directions:

- The enrolment key is **ephemeral**, so the common case needs no repair at all: the node record
disappears when the machine disconnects, and `NodeExists` becomes false on its own.
- When it does not — the disconnect never arrived, the daemon died mid-leave — the same `Tick` that
  observes `--MachineGone-->` with a node still present calls `Network.Forget(id)`. The machine
  stays in `TERMINATING` while that is in flight, `Forget` is retried to a bounded count, and a
  `Forget` that keeps failing is `ERROR(TERMINATING)`, which raises the alarm rather than spinning.
  `ForgetRefuses(n)` in `docs/testing.md` reproduces exactly that, and a `sad/` case asserts the
  bounded retry count and the state after exhaustion — a repair path with no fault to exercise it
  is the untestable seam the rest of this document is written to avoid.

Without the second half, `README.md`'s *node that lingers on a private network* is a failure the
design can only watch, and `ABSENT` is unreachable for exactly the machines that need it most: a
404'd delete would cycle `TERMINATING → ERROR` until a human noticed. Detection without repair is
not a fix, it is a log line.

## Every named fault has a home

`docs/testing.md` lists eight faults. Each one has a state, a transition, or a rule here, and this
table is the claim the pull request makes — if a row is ever wrong, one of the two documents is:

| Fault | Home |
|---|---|
| `FailCreate(err)` | `ABSENT --CreateFailed--> ABSENT`; backoff on the request record, nothing to describe |
| `StallBoot(d)` | `PROVISIONING --Stalled--> ERROR` at `BootDeadline`; the no-op sync destroys it at the cap |
| `DeleteRefuses(n)` | bounded delete attempts, then `TERMINATING --Unconfirmed--> ERROR` |
| `DeleteReturns404()` | `TERMINATING --MachineGone--> TERMINATING`; success for the machine half only |
| `ForgetRefuses(n)` | `Forget` retried to a bounded count, then `ERROR(TERMINATING)`; a `sad/` case asserts both |
| `ListDrops(id)` | presence in `List` is evidence, absence is not; only `Describe` resolves it |
| `CreateIsSlow(d)` | a late create is adopted by the next `Tick` if the id came back, else swept as an orphan |
| `MetadataMissing()` | `PROVISIONING --Stalled--> ERROR`; never `ABSENT`, because it bills. Produced by the network fake, not the cloud one: it is a machine that runs and never joins |

## The reconcile loop

`Tick` is the whole mechanism:

1. Read the cloud's view (`List`, filtered by our label) and the state document. Union them.
   A machine in the union but not in the state document is an **orphan**: it is adopted if it
   carries our lease label and is younger than `MaxLifetime`, and swept otherwise.

   **Presence in `List` is evidence; absence is not.** `ListDrops(id)` in `docs/testing.md` models
   eventual consistency — the API does not know about a machine that exists — so "not in `List`"
   never means gone and never permits a second provision or a confirmed delete. Only a direct
   `Describe` returning not-found resolves the machine half, and only `Network.NodeExists` returning
   false resolves the node half. Taking absence from a listing as absence from the world is how a
   design provisions a second machine beside a live one and then pays for both.
2. Update live-lease counts per machine, at `now`.
3. Walk each machine forward one transition at most.
4. Write the state document once, if anything changed.
5. Return what changed, for the caller to log.

One transition per `Tick`. That is what makes `Tick` twice under a frozen clock produce at most one
transition, and it keeps a partial write from being indistinguishable from a half-applied
transition sequence.

`Tick` is called from `main` on a poll interval — the one place in the repository allowed to read
the wall clock or sleep. Firefly itself contains no `time.Now()` and no `time.Sleep`, and the AST
gate in `docs/testing.md` enforces it rather than review.

## Deletion

Deletion is the only operation that loses data, and the real content of this document is what it
promises. It happens on two paths, and every promise below holds on both:

- **Inside `Tick`**, as the teardown every machine eventually takes:
  `DRAINING → SYNCING → TERMINATING → ABSENT`.
- **Through `Destroy`**, the explicit operator method, which is not a step of that sequence. It is
  covered separately at the end of this section, because conflating the two is how a "safety"
  method quietly becomes the normal path.

**Nothing is deleted through `Tick` before its state is saved.** No machine reaches `TERMINATING`
from the loop except through a `SYNCING` row — `--Synced-->` normally, `--Abandoned-->` only under
the cap rule above. This is the ordering property in `docs/testing.md`, in the qualified form that
document now carries: over any generated history, a `DELETE` for a machine is preceded by a
successful sync for it **or by a recorded abandonment**. The exception is stated once, in the
property row and in "Where the ordering property yields"; `Destroy` is the third door, it is not
part of the loop, and its abandonment is counted against the same counter under a different reason
label. The property is the reason a machine keeps existing across a failed save.

If the sync fails, the machine stays, the state becomes `ERROR`, and the retry syncs again. The
asymmetry is intentional: a lingering machine costs cents and is fixable, while a destroyed machine
holding an hour of uncommitted work is not fixable at any price. Firefly always pays the few cents
first, and abandons only when the cap leaves no alternative.

**Deletion is idempotent, and a 404 is success for the machine half.** `DeleteReturns404` in the
fake models the common real case — a delete submitted twice, a machine reaped by the provider, an
operator using the console. It is indistinguishable from a clean delete, and it resolves exactly
one of the two facts: the node record may still be there, so the machine stays in `TERMINATING`
until `Network.NodeExists` says otherwise.

**Deletion is deadline-bounded and never waits.** It issues the delete, records `TERMINATING`, and
returns. Confirmation arrives on a later `Tick`. A delete that has not confirmed by its deadline
moves to `ERROR` and is re-checked by reading, not by blocking.

**Deletion retries a refusing provider.** `DeleteRefuses(n)` — a delete that fails n times then
succeeds — is a `sad/` case with a bounded number of attempts, then `ERROR`, then the operator.

**A delete cannot race a save.** This is the third of the failures `README.md` says are worth
catching, and the design closes it structurally rather than with care: the transition into
`TERMINATING` is only reachable from a `SYNCING` row, so there is no interleaving in which a save is
in flight when the delete is issued. The hard cap does not break this — it routes *through*
`SYNCING`, so the cap can delay a teardown but never skip one. Two cases still abandon a save — one
automatic, one manual — and both are counted, which is why the counter is labelled rather than
merely incremented. See below.

### Where the ordering property yields

The property needs a caveat to be true, so the caveat is stated here rather than discovered later.
A save is abandoned in exactly two situations:

1. **The sync has failed for the whole lifetime of the machine.** At `MaxLifetime`, with
   `SyncAttempts` exhausted, the machine takes `SYNCING --Abandoned--> TERMINATING`. It is deleted
   because the alternative is billing forever, and the loss is recorded: a data-loss warning naming
   what was not saved, and an `abandonments_total{reason="sync_failed"}` increment.
2. **An operator says so.** `Destroy(ctx, machine, reason)` is the interface method, and it reaches
   `TERMINATING` without a sync at all. It exists for exactly two callers: a human at a shell, and
   the E2E sweeper in `docs/testing.md` that reaps anything older than thirty minutes.

`Destroy` is not a transition and is not reachable from `Tick`. Every path in (1) and (2) logs what
was lost and increments **one** counter, because a single number is what makes it impossible for a
new teardown path to forget to count itself. Sharing the counter only works if the two reasons are
distinguishable, so it is labelled:

| Series | Incremented by | Correct steady state |
|---|---|---|
| `abandonments_total{reason="sync_failed"}` | case (1), the cap | zero; nonzero only in a run that injects the permanent sync failure, where the `sad/` case asserts exactly one |
| `abandonments_total{reason="operator"}` | case (2), `Destroy` | zero in production; nonzero wherever the E2E sweeper runs, by design |

The alarm is therefore on the series, not the total, and it is a claim about **steady state rather
than about every run** — a distinction worth keeping explicit, because the same document describes a
test that deliberately drives `reason="sync_failed"` to one. A metric that is never allowed to
reach one in any circumstances could not have a test asserting it does.

An earlier draft of this paragraph said a nonzero counter meant something upstream was failing to
sync, which was wrong the moment the E2E sweeper — a legitimate caller of (2) that runs every thirty
minutes — was taken seriously: `reason="operator"` is expected to be nonzero there, and only
`reason="sync_failed"` is a never-event. Same counter, two contracts.

Case (1) is reachable from `Tick`, so the property needs an exception and **`docs/testing.md` says
so**: its ordering row now reads "preceded by a successful sync for it, or by a recorded
abandonment", and `order_test.go` excludes the permanent-sync-failure fault except in the one case
that asserts the abandonment. Two documents that disagree about a property is worse than a property
with a stated exception, and the alternative — moving abandonment out of `Tick` — would leave the
cap reachable only by an operator, which is the failure mode the cap exists to prevent.

The property is kept honest by saying precisely where each form holds:

- **strictly**, over any `Tick`-generated history in which the sync eventually succeeds, and that
  generator is the default: `pgregory.net/rapid` produces what its generators allow, so the
  exclusion is written in the generator rather than assumed from the library;
- the cap does not reach the hole first: it routes through `SYNCING`, so a runaway *healthy*
  machine always syncs before it dies. Only a sync that cannot succeed reaches `--Abandoned-->`;
- the abandonment is a tested behaviour, not a paragraph. A `sad/` case injects a permanent sync
  failure and asserts three things: the machine is gone, the counter is exactly one, and the
  warning names what was lost. A hole with a test is a decision; a hole with only prose is a bug
  waiting for its first busy night.

If `abandonments_total{reason="sync_failed"}` is nonzero on a normal day, something upstream is
failing to sync and the number is the alarm, not the fix. A nonzero `reason="operator"` outside the
E2E project means someone is stopping machines by hand, which is worth knowing and is not a bug in
this package.

## What firefly depends on

Three interfaces, all narrow enough to fake in the integration tier.

```go
type Cloud interface {
    Create(ctx context.Context, spec Spec) (MachineID, error)
    Describe(ctx context.Context, id MachineID) (CloudMachine, error)
    List(ctx context.Context, label string) ([]CloudMachine, error)
    Delete(ctx context.Context, id MachineID) error
}

// Network makes both two-fact rules computable. Joined and NodeExists
// observe; Forget repairs, because observation alone can detect a lingering
// node forever without ever resolving it.
// Syncer saves a machine's own state -- the session, the workspace, the
// uncommitted patch. Not the document: that is Store, and losing it costs a
// reconcile rather than an afternoon.
type Syncer interface {
    Sync(ctx context.Context, id MachineID) error
}

type Network interface {
    Joined(ctx context.Context, id MachineID) (bool, error)
    NodeExists(ctx context.Context, id MachineID) (bool, error)
    Forget(ctx context.Context, id MachineID) error
}

type Store interface {
    Load(ctx context.Context) (Document, error)
    Save(ctx context.Context, doc Document) error
}
```

The stored type is `Document`, not `State`, because `State` already means a machine's position in
the transition table and one name for both would make every sentence in this file ambiguous. A
`Document` is the machines plus the leases, versioned, whole-document on both read and write: a
partial update could record a machine as terminating while its lease still holds it open, which is
the state the ordering property exists to make impossible.

`CloudMachine`, not `Machine`: the provider reports facts in its own vocabulary — `created`,
`starting`, `running`, `stopping` — while `Machine` is the durable record of a *decision*. Returning
`Machine` here would let the provider's word for "running" be mistaken for firefly's `READY`, which is
the confusion the whole readiness section exists to avoid.

`Spec` carries the rendered cloud-init, the image, the region, the size, and the labels. It does not
carry a provider credential — no path through this package can put a key on a box, which is the line
`legacy/grove-bloom/README.md` asks you to notice. It does carry what a machine needs to *join* the
network: the mesh address and a single-use, short-lived **ephemeral** enrolment key that grants
membership and nothing else, and whose node record disappears on disconnect so that `NodeExists`
usually resolves itself. That distinction is the same one the whole design rests on — a credential
that can spend money never reaches the runner; a credential that can only join a private network
may.

### The dependency that was missing

`Cloud`, `Network`, and `Store` were all specified before any code existed, and the ordering property
— nothing is deleted before its state is saved — was specified with them. Writing the loop found that
the save itself had no method: `Store` is firefly's own bookkeeping, and nothing could reach the
machine to save the work. A guarantee whose work has no method is a guarantee about nothing, which is
why `Syncer` is now a required dependency and a missing one fails `New` rather than being discovered
on the first teardown.

The same shape as the missing `Machine` field on `Lease` and the missing machine argument on
`Acquire`: three gaps, all invisible in prose and immediate in code, all found by building the next
layer rather than by re-reading the last one.

## What firefly does not own

Named so that review can point at them and say "not here":

- **Provisioning.** There is no `Ensure` yet. `Tick` reconciles the machines the document holds and
  adopts orphans the provider has; deciding that a machine *should* exist is the caller's question
  and lands with its own method, because it needs the placement policy to answer it.
- **Placement.** Firefly reports live counts and each machine's deadline —
  `Snapshot(ctx, now).Machines[i]` gives `Live`, `State`, `Since`, and `Deadline` — and the caller
  picks a machine. Capacity is absent on purpose: how many sessions a machine *should* hold is
  policy, and the table above says so, so the caller compares `Live` against its own limit. The
  policy of packing sessions onto one box is not a lifecycle question.
- **The proxy and its ledger.** Per-session tokens, spend caps, and provider keys. Firefly never
  learns that a model exists.
- **Repository cloning and the agent process.** Everything that happens inside `READY`.
- **Auth.** `Acquire` takes a holder name from an already-authenticated caller and does not verify
  it. Authorisation is not a lifecycle concern.

## Open questions

Not decided here, recorded so the next document does not invent an answer by accident:

1. **Lease durability granularity.** Decided for now: **one document for all machines**, and the
   store validates it on both read and write. A shard per machine would survive a partial write
   better, but a partial write needs a crash at a specific instant while the orphan sweep runs every
   tick, and the sweep is materially harder against shards. Revisit when there is a two-machine
   test and something in it worth losing.
2. **The drain window's default.** Long enough to survive a turn boundary, short enough not to be
   noticed on a bill. Guess: fifteen minutes. Measure before fixing.
3. **Whether `Turn` leases are worth their own kind.** They may collapse into `Session` renewed by
   stream activity, and the kind list is one shorter. The reason to keep them is that
   `Snapshot(ctx, now)` can then report "a machine with sessions but no turns" — idle, but resumable —
   which is exactly what the UI wants to show.
4. **Adoption after a split brain.** Two orchestrators disagreeing about one machine resolves by
   the cloud's label and the earlier `MaxLifetime` deadline, but the tie-break deserves a `bad/`
   case and a paragraph.
