# Firefly

The machinery that keeps exactly as many machines alive as the work needs, and no more.

Firefly is one package, `internal/firefly`, with no knowledge of sessions, repositories, models,
or money. It answers a single question on a loop: *given the leases currently held and the
machines that currently exist, what must change?* Everything else in this repository argues with
it through three narrow interfaces, and it is handed the current time rather than looking it up.

Design only. No Go yet — `docs/testing.md` names the files this will become, and the first commit
that implements any of it is a failing test.

One word on vocabulary: this document says **machine**, the product vocabulary of `README.md`.
Where `docs/testing.md` says *instance* it means the same thing; the property names there are left
as written rather than renamed, because renaming a property breaks the history of what it caught.

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
    Acquire(holder string, kind Kind, ttl time.Duration, now time.Time) (LeaseID, error)
    Renew(id LeaseID, ttl time.Duration, now time.Time) error
    Release(id LeaseID) error

    // Snapshot is read-only and safe to call from anywhere, but it reports
    // live counts, so it needs the clock like everything else.
    Snapshot(now time.Time) Snapshot

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
    ID     LeaseID       // minted by firefly, opaque
    Holder string        // policy's name; never parsed by firefly
    Kind   Kind          // Turn | Session | Subscription
    Start  time.Time     // set by firefly from now, never by the caller
    TTL    time.Duration
}
```

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

`Renew` on a lapsed lease returns `ErrLeaseExpired` rather than quietly re-acquiring. The
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
  data-loss warning naming what was not saved, and the same counter the operator override uses.

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
| boots, never answers | metadata never came up | `Cloud.Describe` | `MetadataMissing` |
| answers, never joins | the network key was rejected or the join hung | `Network.Joined` | `StallBoot` |

Both columns matter. A guarantee that names a fact no interface can read is not a guarantee, it is
a wish — which is why the `Network` interface in "What firefly depends on" exists at all, and why
the second row is the one that is easiest to write and hardest to compute.

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

Without the second half, `README.md`'s *node that lingers on a private network* is a failure the
design can only watch, and `ABSENT` is unreachable for exactly the machines that need it most: a
404'd delete would cycle `TERMINATING → ERROR` until a human noticed. Detection without repair is
not a fix, it is a log line.

## Every named fault has a home

`docs/testing.md` lists seven faults. Each one has a state, a transition, or a rule here, and this
table is the claim the pull request makes — if a row is ever wrong, one of the two documents is:

| Fault | Home |
|---|---|
| `FailCreate(err)` | `ABSENT --CreateFailed--> ABSENT`; backoff on the request record, nothing to describe |
| `StallBoot(d)` | `PROVISIONING --Stalled--> ERROR` at `BootDeadline`; the no-op sync destroys it at the cap |
| `DeleteRefuses(n)` | bounded delete attempts, then `TERMINATING --Unconfirmed--> ERROR` |
| `DeleteReturns404()` | `TERMINATING --MachineGone--> TERMINATING`; success for the machine half only |
| `ListDrops(id)` | presence in `List` is evidence, absence is not; only `Describe` resolves it |
| `CreateIsSlow(d)` | a late create is adopted by the next `Tick` if the id came back, else swept as an orphan |
| `MetadataMissing()` | `PROVISIONING --Stalled--> ERROR`; never `ABSENT`, because it bills |

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

**Nothing is deleted before its state is saved.** No machine reaches `TERMINATING` except through a
`SYNCING` transition — `--Synced-->` normally, `--Abandoned-->` only under the cap rule above. This
is the ordering property in `docs/testing.md` — over any generated history, a `DELETE` for a
machine is preceded by a successful sync for it — and it is the reason a machine keeps existing
across a failed save.

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
`SYNCING`, so the cap can delay a teardown but never skip one. Two cases still
abandon a save — one automatic, one manual — and both are counted. See below.

### Where the ordering property yields

The property needs a caveat to be true, so the caveat is stated here rather than discovered later.
A save is abandoned in exactly two situations:

1. **The sync has failed for the whole lifetime of the machine.** At `MaxLifetime`, with
   `SyncAttempts` exhausted, the machine takes `SYNCING --Abandoned--> TERMINATING`. It is deleted
   because the alternative is billing forever, and the loss is recorded: a data-loss warning naming
   what was not saved, and a counter increment.
2. **An operator says so.** `Destroy(ctx, machine, reason)` is the interface method, and it reaches
   `TERMINATING` without a sync at all. It exists for exactly two callers: a human at a shell, and
   the E2E sweeper in `docs/testing.md` that reaps anything older than thirty minutes.

`Destroy` is not a transition and is not reachable from `Tick`. Every path in (1) and (2) logs what
was lost and increments the same counter, whose correct steady-state value is zero.

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

If the counter is nonzero on a normal day, something upstream is failing to sync and the number is
the alarm, not the fix.

## What firefly depends on

Three interfaces, all narrow enough to fake in the integration tier.

```go
type Cloud interface {
    Create(ctx context.Context, spec Spec) (MachineID, error)
    Describe(ctx context.Context, id MachineID) (Machine, error)
    List(ctx context.Context, label string) ([]Machine, error)
    Delete(ctx context.Context, id MachineID) error
}

// Network makes both two-fact rules computable. Joined and NodeExists
// observe; Forget repairs, because observation alone can detect a lingering
// node forever without ever resolving it.
type Network interface {
    Joined(ctx context.Context, id MachineID) (bool, error)
    NodeExists(ctx context.Context, id MachineID) (bool, error)
    Forget(ctx context.Context, id MachineID) error
}

type Store interface {
    Load(ctx context.Context) (State, error)
    Save(ctx context.Context, s State) error
}
```

`Spec` carries the rendered cloud-init, the image, the region, the size, and the labels. It does not
carry a provider credential — no path through this package can put a key on a box, which is the line
`legacy/grove-bloom/README.md` asks you to notice. It does carry what a machine needs to *join* the
network: the mesh address and a single-use, short-lived **ephemeral** enrolment key that grants
membership and nothing else, and whose node record disappears on disconnect so that `NodeExists`
usually resolves itself. That distinction is the same one the whole design rests on — a credential
that can spend money never reaches the runner; a credential that can only join a private network
may.

## What firefly does not own

Named so that review can point at them and say "not here":

- **Placement.** Firefly reports capacity (`Snapshot(now).Machines[i].Live`, `.Capacity`) and the
  caller picks a machine. The policy of packing sessions onto one box is not a lifecycle question.
- **The proxy and its ledger.** Per-session tokens, spend caps, and provider keys. Firefly never
  learns that a model exists.
- **Repository cloning and the agent process.** Everything that happens inside `READY`.
- **Auth.** `Acquire` takes a holder name from an already-authenticated caller and does not verify
  it. Authorisation is not a lifecycle concern.

## Open questions

Not decided here, recorded so the next document does not invent an answer by accident:

1. **Lease durability granularity.** One state document for all machines is simplest and is what
   this doc assumes. A shard per machine would survive a partial write better and complicate the
   orphan sweep. Decide when the first two-machine test exists.
2. **The drain window's default.** Long enough to survive a turn boundary, short enough not to be
   noticed on a bill. Guess: fifteen minutes. Measure before fixing.
3. **Whether `Turn` leases are worth their own kind.** They may collapse into `Session` renewed by
   stream activity, and the kind list is one shorter. The reason to keep them is that
   `Snapshot(now)` can then report "a machine with sessions but no turns" — idle, but resumable —
   which is exactly what the UI wants to show.
4. **Adoption after a split brain.** Two orchestrators disagreeing about one machine resolves by
   the cloud's label and the earlier `MaxLifetime` deadline, but the tie-break deserves a `bad/`
   case and a paragraph.
