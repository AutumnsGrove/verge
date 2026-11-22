# Firefly

The machinery that keeps exactly as many machines alive as the work needs, and no more.

Firefly is one package, `internal/firefly`, with no knowledge of sessions, repositories, models,
or money. It answers a single question on a loop: *given the leases currently held and the
machines that currently exist, what must change?* Everything else in this repository argues with
it through two interfaces and a clock.

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
    // Tick reconciles once, at now. The only method that changes cloud state.
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
supervisor racing to clean up will both call it, and one of them is not a bug.

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
PROVISIONING --Created-->          PROVISIONING     (id recorded, not yet answering)
PROVISIONING --Answered-->         READY
PROVISIONING --Stalled-->          ERROR            (boot exceeded BootDeadline)
READY       --LeasesLapsed-->      DRAINING
DRAINING    --LeaseArrived-->      READY            (the drain window is not a point of no return)
DRAINING    --Drained-->           SYNCING
SYNCING     --Synced-->            TERMINATING
SYNCING     --SyncFailed-->        ERROR            (and the machine stays)
TERMINATING --Confirmed-->         ABSENT            (machine gone and node gone)
TERMINATING --Gone-->              ABSENT            (a 404 is success)
TERMINATING --Unconfirmed-->       ERROR            (deadline passed; re-check, never wait)
ERROR       --RetryDue-->          <the state it came from>
any         --Capped-->            SYNCING          (hard cap; sync first, always)
```

Two rows exist because of named failures in the previous design and are not optional:

- **`TERMINATING --Unconfirmed--> ERROR`.** `TERMINATING` is the state that once slept forever: a
  delete was issued and the code then waited on a machine that had already gone. A machine in
  `TERMINATING` is on a deadline, and the next `Tick` after that deadline re-reads the cloud rather
  than waiting. `TERMINATING` never blocks.
- **`DRAINING --LeaseArrived--> READY`.** The drain window is a courtesy, not a commitment. Work
  arriving during it costs nothing to accept, and a design that makes it a one-way door turns a
  busy session into a cold clone.

`ERROR --RetryDue--> <origin>` is the one row that is not a pair of literals. `ERROR` records the
state it came from, so the table holds one concrete row per origin and the transition gate still
loops over named cases with nothing wildcarded. A retry that cannot name its origin is a bug in the
record, not a case to handle.

`any --Capped--> SYNCING` is deliberate: the hard cap routes *through* the sync, so a capped
machine saves its work. See "Destroy" for the one case where even that yields.

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

| Symptom | Fact | Fault |
|---|---|---|
| boots, never answers | metadata never came up | `MetadataMissing` |
| answers, never joins | the network key was rejected or the join hung | `StallBoot` |

A machine that boots and never answers is *not* `ABSENT`: it exists, it is billing, and it must be
swept.

**Deletion is two facts as well.** A machine can be gone from the provider and still be a member of
the network — its node record outlives it, and a tailnet accumulates those quietly. `TERMINATING`
reaches `ABSENT` only when the provider has confirmed the machine *and* the node record is no longer
present. Until then it is `TERMINATING`, and past its deadline, `ERROR` with a `LastErr` that
names which half is outstanding. A `DELETE` that returns 404 therefore resolves the machine half and
still has work to do.

## The reconcile loop

`Tick` is the whole mechanism:

1. Read the cloud's view (`List`, filtered by our label) and the state document. Union them.
   A machine in the union but not in the state document is an **orphan**: it is adopted if it
   carries our lease label and is younger than `MaxLifetime`, and swept otherwise. Firefly trusts
   the cloud over its own memory, because the cloud is the thing that bills.
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

## Destroy

`Destroy` is the only operation that loses data, and the doc's real content is what it promises.

**A machine is destroyed only after its state has been saved.** Nothing in the state machine
reaches `TERMINATING` except from `SYNCING --Synced-->`. This is the ordering property in
`docs/testing.md` — over any generated history, a `DELETE` for a machine is preceded by a
successful sync for it — and it is the reason the machine keeps existing across a failed save.

If the sync fails, the machine stays, the state becomes `ERROR`, and the retry syncs again. The
asymmetry is intentional: a lingering machine costs cents and is fixable, while a destroyed machine
holding an hour of uncommitted work is not fixable at any price. Firefly always pays the few cents.

**`Destroy` is idempotent, and a 404 is success.** `DeleteReturns404` in the fake models the common
real case — a delete submitted twice, a machine reaped by the provider, an operator using the
console. It is indistinguishable from a clean delete and is treated as one.

**`Destroy` is deadline-bounded and never waits.** It issues the delete, records `TERMINATING`, and
returns. Confirmation arrives on a later `Tick`. A delete that has not confirmed by its deadline
moves to `ERROR` and is re-checked by reading, not by blocking.

**`Destroy` retries a refusing provider.** `DeleteRefuses(n)` — a delete that fails n times then
succeeds — is a `sad/` case with a bounded number of attempts, then `ERROR`, then the operator.

**A delete cannot race a save.** This is the third of the failures `README.md` says are worth
catching, and the design closes it structurally rather than with care: the transition into
`TERMINATING` is only reachable from `SYNCING --Synced-->`, so there is no interleaving in which a
save is in flight when the delete is issued. The hard cap does not break this — `any --Capped-->
SYNCING` routes *through* the save, so the cap can delay a teardown but never skip one. The one
case where a save is genuinely abandoned is the operator override below, which is not a transition
and is counted.

### The one hole, named

`Destroy(ctx, machine, reason)` is an operator override that reaches `TERMINATING` without a sync.
It exists for exactly two callers: an operator at a shell, and the E2E sweeper in
`docs/testing.md` that reaps anything older than thirty minutes.

It is not a transition. It is not reachable from `Tick`. It always logs a data-loss warning naming
what was not saved, and it increments a counter whose correct steady-state value is zero.

This is the seam where the ordering property and the hard cap genuinely collide, so it is worth
being explicit about how it is kept honest:

- the property is stated over histories *generated by `Tick`*, which is what `pgregory.net/rapid`
  generates, and holds absolutely there;
- the hard cap does not use the hole — it routes through `SYNCING`, so even a runaway machine syncs
  before it dies;
- the hole exists for the case where the *orchestrator itself* is gone and a human has to stop the
  bleeding. There is no state machine to walk, which is why it cannot be one.

If the override counter is ever nonzero on a normal day, something upstream is failing to sync and
the number is the alarm, not the fix.

## What firefly depends on

Two interfaces, both narrow enough to fake in the integration tier.

```go
type Cloud interface {
    Create(ctx context.Context, spec Spec) (MachineID, error)
    Describe(ctx context.Context, id MachineID) (Machine, error)
    List(ctx context.Context, label string) ([]Machine, error)
    Delete(ctx context.Context, id MachineID) error
}

type Store interface {
    Load(ctx context.Context) (State, error)
    Save(ctx context.Context, s State) error
}
```

`Spec` carries the rendered cloud-init, the image, the region, the size, and the labels. It does not
carry a provider credential — no path through this package can put a key on a box, which is the line
`legacy/grove-bloom/README.md` asks you to notice.

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
