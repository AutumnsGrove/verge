# Testing

How this repository proves its own correctness. Written before any code so that the first commit
is a test, and so the gates exist before there is anything for them to gate.

Three commitments, made deliberately:

1. **Test-first everywhere.** Every behaviour starts as a failing test, including the UI shell and
   the CLI. Where that sounds like ceremony, it is a signal the design has an untestable seam, and
   the fix is the design, not the rule.
2. **Fakes are models, not stubs.** The fake cloud has states, timing, and injectable faults,
   because a fake that is dumber than the real thing makes a green build lie.
3. **Strength is measured, not declared.** Coverage percentages are not a gate. Mutation results
   and transition completeness are.

## Layout

```
internal/
  firefly/                    the shared mechanism
    fsm.go                    the transition table, as data
    fsm_test.go               one case per transition — enforced (see "Gates")
    lease.go
    lease_test.go             table-driven: happy/, sad/, bad/
    firefly_test.go           integration: the real loop, fake cloud, fake clock
    order_test.go             the sync-before-delete invariant
    testdata/golden/          recorded event logs, replayed by the fake
  cloud/hetzner/
    client.go                 the real client
    client_test.go            against httptest
    contract_test.go          //go:build contract — real API, test project
  cloud/fake/
    fake.go                   the one shared fake: provider and Network
  net/tailnet/
    client.go                 the real Network: join, observe, forget
    client_test.go
  rpc/relay/
    relay.go                  the pi JSONL reader
    relay_test.go
    relay_fuzz_test.go        FuzzRelay — the highest-value target
  proxy/
    stream.go
    stream_test.go
    stream_fuzz_test.go       FuzzStream
e2e/
  contract_test.go            //go:build e2e
  full_test.go                //go:build e2e
docs/
  testing.md                  this file
  firefly.md                  the mechanism/policy boundary
```

`go test ./...` must finish in seconds, never touch the network, and never sleep. The slow tiers
are behind build tags and run in CI.

## Four tiers

| Tier | Command | Needs | Budget | Covers |
|---|---|---|---|---|
| static | `make vet` | nothing | seconds | vet, staticcheck, gofmt, govulncheck |
| unit + integration | `go test ./...` | nothing | < 5 s | everything reachable without a cloud |
| contract | `go test -tags contract ./internal/cloud/...` | a **separate** Hetzner project, R2, a tailnet key | ~6 min | a real machine boots, joins, leases, syncs, destroys |
| full | `go test -tags e2e ./e2e` | the above plus a model provider | ~12 min | one real session, end to end, and a resume |

The middle tier is the biggest slice of the suite, and it means **integration**, not unit: the real
reconcile loop, the real FSM, the real R2 client shape, against a fake cloud and a fake clock.
Unit tests cover only what is genuinely pure — lease arithmetic, uid derivation, template
rendering, framing.

### Why contract and full are separate

Provisioning takes minutes and costs money. Splitting them means the pre-merge gate can prove the
mechanism works against a real cloud without also paying for a model call and a full git clone.

## The fake cloud

One fake, in `internal/cloud/fake`, shared by every test. Not a stub per test file — divergent
fakes are how a suite starts lying.

It models the provider as a small state machine of its own:

```
absent → created → starting → running → stopping → deleted
```

**Timing.** Every duration comes from the injected clock. A four-minute boot is instant unless the
test advances it, which is what makes the integration tier fast.

**Faults.** Each is a named, composable behaviour, and each gets a sad-path test:

| Fault | Models |
|---|---|
| `FailCreate(err)` | quota exceeded, auth expired, malformed request |
| `StallBoot(d)` | the machine never finishes cloud-init |
| `DeleteRefuses(n)` | a delete that fails n times then succeeds |
| `DeleteReturns404()` | already gone — must be treated as success |
| `ListDrops(id)` | eventual consistency: the API doesn't know about a machine that exists |
| `CreateIsSlow(d)` | a request that returns after the caller's deadline |
| `MetadataMissing()` | a booted machine that never answers |
| `ForgetRefuses(n)` | the node record outlives its machine and refuses removal for n attempts |

**Conformance.** The fake must reproduce the recorded golden event logs from the contract tier.
When it cannot, the fake is wrong or the cloud changed, and either way a human looks.

**The same fake answers for `Network`.** `Joined` is half of `READY` and `NodeExists` is half of
deletion, so a fake that cannot hold a node record makes both untestable. It reads the same state:
a node whose machine is `deleted` stays present until `Forget` succeeds, which is what makes
`ForgetRefuses` and "a node that lingers on a private network" reproducible rather than
theoretical.

## The clock

Injected from day one, everywhere. No `time.Now()`, no `time.Sleep` outside `main`. This is the
single most common reason orchestrator suites become flaky, and it is cheap to avoid.

Lease expiry, idle windows, hard caps, token refresh, and poll intervals are all clock-driven.

## Happy, sad, bad

Case names carry the category, so `go test -run 'TestLease/bad'` works and so a missing category is
visible in review.

- **happy/**: the normal path. Provision → ready → work → sync → delete.
- **sad/**: an expected failure the design handles. Create rejected, boot stalls, sync errors,
  a sync that never succeeds and ends in a recorded abandonment, delete 404s, a node that outlives
  its machine and refuses to be forgotten, a lease renewal arrives late, a subscription expires.
- **bad/**: hostile input the design never invited. This is the category most suites skip and the
  one that finds real bugs in a proxy and a relay.

Committed `bad/` cases:

| Area | Case |
|---|---|
| tokens | a session token cannot read or spend another session's budget |
| tokens | a revoked token fails within one request |
| proxy | a model id cannot traverse a route prefix (`/go/../or/...`) |
| proxy | an upstream 200 carrying an HTML error page is an error, not a response |
| proxy | usage missing, or a string where a number belongs, does not crash the ledger |
| relay | `U+2028` and `U+2029` inside a JSON string are not record boundaries |
| relay | a frame split mid-UTF-8 rune |
| relay | a 10 MB line does not allocate unbounded |
| patch | a diff header escaping the working directory is refused |
| template | a repo URL containing a newline, quote, or `$` never escapes its YAML block |
| attention | an extension entry claiming a state outside the enum is rejected |
| subscriptions | an event for a PR the session does not hold is ignored |

## Fuzzing

Native `testing.F`, corpus committed under `testdata/fuzz/`, seeds replayed on every PR.

| Target | Why |
|---|---|
| `FuzzRelay` | pi's stream is LF-framed with explicit warnings about Unicode separators, backpressure, and partial UTF-8. This is the boundary most likely to eat a real session. |
| `FuzzTemplate` | cloud-init inputs render into YAML that runs as root at boot. A newline in a repo URL is remote code execution. This is a security test with a fuzzer attached. |
| `FuzzStream` | SSE chunk boundaries, `\r` handling, a chunk after close, malformed usage. |
| `FuzzPatch` | patches are generated from a hostile tree and applied on a fresh machine. Binary content, CRLF, path escape. |
| `FuzzHetznerDecode` | truncated and mutated JSON from the provider. Cheap; it is nearly free once the harness exists. |

**Budget.** Seeds on PR (~2 s). Ten minutes per target nightly. One 24-hour soak before the first
release tag. A failing input is minimised, committed as a seed, and fixed — never skipped.

Not fuzzed on purpose: the FSM and lease arithmetic. Their input space is meaningless random bytes;
they get the transition gate and property tests instead.

## Property tests

`pgregory.net/rapid`, for invariants that must hold over *any* generated history rather than one
golden one.

| Property | Statement |
|---|---|
| ordering | over any generated event log, a `DELETE` for an instance is preceded by a successful sync for it, or by a recorded abandonment (below) |
| idempotence | `Tick` twice with a frozen clock produces at most one transition |
| monotonicity | adding a lease never makes an instance fade sooner |
| monotonic clock | `Live(instance)` never grows as the clock advances with no renewal |
| lease expiry | a lease with TTL t is live for exactly `[start, start+t)` |
| release | `Release` is idempotent; a second release is not an error |

**The ordering property has one stated exception, and the generator says so.** A machine whose sync
cannot succeed would bill forever if the hard cap also refused to abandon the save, so at
`MaxLifetime` it takes `--Abandoned-->` and is deleted: the loss is logged and counted, and the
counter's steady state is zero (`docs/firefly.md`, "Where the ordering property yields").
`order_test.go` therefore excludes the permanent-sync-failure fault from the ordering generator,
except in the one `sad/` case that asserts the abandonment, and that case claims the counter is
exactly one. The property is strict everywhere else — a `DELETE` with no sync and no recorded
abandonment is a failure in every generated history.

## Strict test-first

Every behaviour starts as a failing test. Two consequences worth naming, because they are the
places people quietly exempt:

**The UI shell.** The SvelteKit app exists to render state. `vitest` plus
`@testing-library/svelte` are installed before the first component, and a component is written
after the test that renders it with props and asserts what a user sees. No test asserts on CSS
classes or component internals.

**The CLI.** Flags are parsed by a pure function — `Parse([]string) (Config, error)` — and tested
before the command exists. This is not ceremony: it forces configuration to be data that can be
inspected and validated, instead of scattered `flag.String` calls that can only be tested by
spawning a process.

Where test-first feels wrong, the design has an untestable seam. Fix the seam.

## Gates

These are enforced in CI, not by convention.

| Gate | Mechanism | Catches |
|---|---|---|
| every FSM transition has a test | the FSM is data; a test loops it and fails on a missing case | an unhandled transition, which is how `TERMINATING` slept forever once |
| no sleeps in tests | an AST walk over `*_test.go` fails on `time.Sleep` | the slowest form of suite rot |
| no network in the fast tier | `TestMain` installs a transport that panics | an accidental real API call that bills you |
| mutation efficacy does not regress | nightly `gremlins` on `internal/firefly` and `internal/rpc/relay`; the score is compared to the last accepted run | tests that assert nothing |
| `-race` everywhere | every tier runs with the race detector | the relay and the proxy both have concurrent readers |
| no flaky tests | a test that fails twice without a code change is fixed or deleted the same day | quarantine by another name |

Coverage is reported for information. It is not a threshold, because a threshold rewards asserting
nothing in many places.

## CI

| Trigger | Runs | Budget |
|---|---|---|
| pull request | vet, staticcheck, unit + integration, `-race`, fuzz seeds, all gates | < 3 min |
| pull request (same-repo branches) | + contract tier | ~8 min |
| nightly | 10 min per fuzz target, mutation on two packages, contract tier | ~50 min |
| nightly | full tier (one session, one resume) | ~15 min |
| weekly | a real `pi --mode rpc` transcript diffed against the committed golden | ~5 min |
| `workflow_dispatch` | full tier with any tier forced | — |

Fork pull requests skip the tiers that need secrets rather than failing them. The workflow says so
explicitly, so a red X from a fork reads as "skipped", not "broken".

## E2E ground rules

- **A separate cloud project with its own credentials.** `Sweep` is destructive by design; a test
  must never be able to see a production machine, and a bug in the test must never spend on the
  production account.
- **The ordering test is the point of the tier.** Inject a sync failure, assert the machine is
  still there and the state is `ERROR`; clear the fault, assert the retry deletes it.
- **A sweeper every 30 minutes** destroys anything labelled `verge-e2e` older than 30 minutes,
  whether or not a test is running. A cancelled job must not bill for a day.
- **A budget assertion per run.** The full tier fails if it spent more than its declared ceiling.
- **A hard timeout on every test.** `go test -timeout` at the workflow level, plus a per-test
  context deadline, so a hung cloud call cannot hold a runner for six hours.

## Adding a behaviour

1. Write the failing test, named with its category (`happy/`, `sad/`, `bad/`).
2. If it needs a new FSM transition, add it to the table first — the gate will then demand a case.
3. If it needs a new cloud behaviour, add the state or fault to the fake, and say in the fake's
   comment which real-world event it models.
4. Write the smallest implementation that passes.
5. If the test needed a seam that feels wrong, stop and change the design.

## What this repository deliberately does not do

- **No mocks at internal seams.** Real HTTP servers, real `os/exec`, real files, injected clock and
  provider. Only the cloud and the wall clock are replaced, because they are the two things that
  cannot run in a test.
- **No coverage threshold.**
- **No golden files accepted without reading the diff.** A golden file is a transcript of a
  behaviour; approving it without reading it is how a bug becomes the expected output.
- **No skipped tests in `main`.** `t.Skip` is allowed only for a missing credential, and it must
  print why.
