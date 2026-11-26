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
    cloud.go                  the Cloud interface: provider facts, never pushed
    network.go                the Network interface: join, node presence, forget,
                              all keyed on the machine's name
    lease.go
    lease_test.go             table-driven: happy/, sad/, bad/
    document.go               the durable state: machines, leases, validation
    document_test.go
    store.go                  the Store interface and an in-memory implementation
    store_test.go             round-trip, rejected writes, copy isolation, race
    firefly.go                the mechanism: config, leases, snapshot
    name.go                   machine names: the prefix, the counter, uniqueness
    tick.go                   the reconcile loop, the orphan sweep, Result
    tick_test.go              the whole lifecycle against the shared fakes
    sync.go                   the Syncer boundary: saving the machine's own state
    provision.go              creating one machine, and the failed-create backoff
    firefly_test.go           the registry, id continuity, failed saves
    order_test.go             the sync-before-delete invariant
    testdata/golden/          recorded event logs, replayed by the fake
  cloud/hetzner/
    client.go                 the real client: four calls, no retries
    client_test.go            against httptest: decoding, 404s, status mapping
    contract_test.go          //go:build contract — a real listing and a real 404
  cloud/fake/
    fake.go                   the provider half: state machine, clock, six faults
    network.go                the network half: node records that outlive machines
    sync.go                   the syncer: records calls, fails on demand
    fake_test.go              one case per fault, plus the lifecycle
    network_test.go           joining, lingering nodes, forget refusals
    node_truth_test.go        NodeExists as a truth table, and order-independence
  config/
    config.go                 flags and environment into one validated Config
    config_test.go            defaults, rejections, and that a Secret never prints
  loop/
    loop.go                   the daemon's loop: immediate pass, interval, graceful stop
    loop_test.go              the injected wait, a failed pass, a pass in flight
  cloudinit/
    render.go                 the boot script: name, mesh address, enrolment key
    render_test.go            the document as a whole, and hostile values
    render_fuzz_test.go       FuzzCloudInit — the highest-value security target
  store/r2/
    store.go                  the state document in object storage, one object
    store_test.go             round trip, absent vs broken, against a real S3 client
    workspace.go              a machine's workspace as one object: the syncer's sink
    workspace_test.go         the request it sends, and the keys it refuses
    contract_test.go          //go:build contract — a document round trip, real bucket
  syncer/
    syncer.go                 fetch, spool, store: what "saved" means, and the cap
    syncer_test.go            the cap's boundary, an empty fetch, a gone machine,
                              a silent one, and ids that cannot be object keys
  syncer/ssh/
    ssh.go                    the transport: one command over ssh, exit status checked
    ssh_test.go               real processes: the stream, a failing exit, cancellation
  net/tailnet/
    client.go                 the real Network: observe, forget (joining is cloud-init)
    client_test.go            against httptest: the two facts, ambiguity, idempotent forget
    key.go                    the enrolment key: ephemeral, single-use, preauthorized
    key_test.go               the request it sends, and the rendering it feeds
    contract_test.go          //go:build contract — a real tailnet, read-only
  rpc/relay/
    relay.go                  the pi JSONL reader and writer
    relay_test.go
    relay_fuzz_test.go        FuzzRelay — the highest-value target
  proxy/
    stream.go
    stream_test.go
    stream_fuzz_test.go       FuzzStream
cmd/verge/
  main.go                     wiring only: four clients, firefly, the loop, signals
  main_test.go                the refusals, the workspace prefix, no transport
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
| static | `make vet` | nothing | seconds | vet, staticcheck, gofmt |
| unit + integration | `go test ./...` | nothing | < 5 s | everything reachable without a cloud |
| contract | `make contract` | a **separate** Hetzner project, a **separate** R2 bucket, a tailnet key (`VERGE_TAILNET_NAME` if the token's default is not the one to ask) | seconds while read-only | the three clients, against the real APIs |
| full | `go test -tags e2e ./e2e` | the above plus a model provider | ~12 min | one real session, end to end, and a resume |

The middle tier is the biggest slice of the suite, and it means **integration**, not unit: the real
reconcile loop, the real FSM, the real R2 client shape, against a fake cloud and a fake clock.
Unit tests cover only what is genuinely pure — lease arithmetic, uid derivation, template
rendering, framing.

`govulncheck` is deliberately not part of `make vet`. It consults the vulnerability database, so it
is the one static check that needs a warm cache or a network, and burying it in the target that
claims "needs: nothing" made that claim false. It is `make vuln`, and it is excluded from anything
promised to work with the network unplugged.

`make vuln` runs on every pull request: `.github/workflows/go.yml` calls it. It is the only job whose
*check* consults the vulnerability database, and it caught a real one on its first run: `go.mod`
pinned Go 1.27.3, which carries GO-2026-6859 in `net/http`, and the first client that called
`http.Client.Do` made the path reachable. The toolchain version in `go.mod` is a dependency like any
other, and the scan is what treats it as one — the `static` job also reaches the network, but only to
install its linters, so "offline" describes what `make vet` does once the tools are present, not
what the job does.

Nothing in this workflow makes a check *required*. It reports a status; making that status a merge
gate is a branch-protection or ruleset setting on the repository, and the setting is independent of
the workflow — a red check is advisory unless it exists. Whether it exists is a fact about the
repository today rather than about this design, so read `main`'s settings instead of this paragraph;
a setting can change without a commit, and a document that pins one goes stale on a day nobody touched
the code.

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

Each fault belongs to exactly one fake, because each models a fact exactly one interface can observe.

| Fault | Fake | Models |
|---|---|---|
| `FailCreate(err)` | cloud | quota exceeded, auth expired, malformed request |
| `StallBoot(d)` | cloud | the machine never finishes cloud-init |
| `DeleteRefuses(n)` | cloud | a delete that fails n times then succeeds |
| `DeleteReturns404()` | cloud | already gone — must be treated as success |
| `ListDrops(id)` | cloud | eventual consistency: the API doesn't know about a machine that exists |
| `CreateIsSlow(d)` | cloud | a request that returns after the caller's deadline |
| `MetadataMissing()` | network | a machine that runs and never joins, because its metadata service never answered |
| `ForgetRefuses(n)` | network | the node record outlives its machine and refuses removal for n attempts |

`MetadataMissing` moved to the network column while the cloud fake was written. The provider API
describes the machine, not what is running inside it, so `Cloud.Describe` cannot report whether the
metadata service answered; the observable consequence is a machine that is `running` and never joins,
which is `Network.Joined`. The fake for it therefore lives with the network, and now exists there.

`CreateIsSlow` has a limit worth stating before someone relies on it: it advances the fake's clock and
then succeeds, so it never observes a real `context.WithTimeout` expiring. The caller-gave-up half is
reachable by passing an already-cancelled context, which is the same path through firefly without a
sleep. When the loop test wires a real deadline against it, the fake will take the success path and
the test will pass for the wrong reason — so the loop's own timeout handling needs either a
context-aware fault or a test that asserts on `ctx.Err()` directly.

**Conformance.** The fake must reproduce the recorded golden event logs from the contract tier.
When it cannot, the fake is wrong or the cloud changed, and either way a human looks.

**One fake package answers for both `Cloud` and `Network`, on one clock.** `Joined` is half of
`READY` and `NodeExists` is half of deletion, so a fake that cannot hold a node record makes both
untestable — and two fakes with two clocks could not express "the machine is deleted but its node
record is still there", which is the exact interleaving the deletion rule is about. So the network
half shares the cloud fake's clock and machine records rather than keeping its own. A node whose
machine is gone stays present until `Forget` succeeds, which is what makes `ForgetRefuses` and "a
node that lingers on a private network" reproducible rather than theoretical.

Both halves now exist, in one package on one clock, which is what makes the lingering-node case
expressible: the provider can forget a machine while the network still holds its record.

The network half is keyed on the machine's **name**, and this is the one place the fake cannot
police the join: it resolves a name against its own machine records, so it answers happily for any
name it happens to agree with — including a provider id, which is the question that was
unanswerable before the rekey. So the fake records both halves (`Cloud.Specs()` is every create
request, `Network.Asked()` is every name the three methods were handed) and one test compares them.
That is `TestNamesJoinTheProviderAndTheNetwork`, and it is the shape to reach for whenever two
interfaces are joined by a value neither of them mints.

### The cloud-init renderer

The most privileged bytes this system writes: a script that runs as root at boot. Four decisions, each of
which is cheaper to read than to rediscover:

- **No text template.** The document is a struct marshalled by `yaml.v3`, because a template is a parser
  on both sides of a trust boundary. The fuzz target therefore attacks the *encoder's* escaping, which is
  a much smaller thing to be wrong about than a quoting scheme someone wrote by hand.
- **Every string is emitted double-quoted**, and that is not decoration. Left alone, the encoder writes a
  multi-line string as a block scalar, which is lossy for values that begin or end with whitespace: a
  hostname of exactly `"\n"` renders as an empty scalar, and `"\na"` loses its leading newline. The
  fuzzer found that on its second run. A hostname that leaves this package different from the one the
  network will be asked about is a machine nothing can find, so the style is pinned rather than trusted.
- **Invalid UTF-8 is refused, not coerced.** Both `encoding/json` and the YAML encoder rewrite it as
  U+FFFD. A silently mangled enrolment key is a machine that boots, fails to join and says nothing; a
  mangled name is invisible. `Render` returns `ErrSpec` instead. The fuzzer found this one too — the
  first seed it tried.
- **The renderer is not a hostname validator.** It guarantees escaping, and the provider guarantees
  validity by refusing the create. Two validators would be two things to keep in step, and a name that
  is refused here has no create to be refused by.
- **There is no `preserve_hostname` key**, and the first version of this package had one. cloud-init
  returns early from `cc_set_hostname` and `cc_update_hostname` when that flag is true — before it reads
  the `hostname` key or the datasource metadata — so setting it does not preserve the name, it stops the
  document naming the machine at all. The machine keeps the image's default hostname, joins the tailnet
  under it, and the network is never asked about a name that exists: the exact failure this package
  exists to prevent, caused by a key that looked like hardening. The flag is gone from the struct so it
  cannot come back, and `TestRender` asserts the key set exactly — a document that grows a key without a
  review is a document a machine executes. A distribution that later renames itself over DHCP is an image
  problem (netplan's `dhcp4-overrides`), because cloud-init only acts at boot.

`FuzzCloudInit` asserts the strong property: every accepted input parses back byte for byte, the document
contains exactly one `write_files` entry and one `runcmd`, and no input can become structure. Seeds on
every PR, and both minimised inputs the fuzzer found — a newline-only hostname and an invalid-UTF-8 one —
are committed under `testdata/fuzz/FuzzCloudInit/`.

### The R2 store

The third real client, and the first with a dependency that is not the standard library: `aws-sdk-go-v2`
does the S3 protocol and SigV4 signing, and the store is two calls over it. The tests keep the seam
honest by signing real requests — a real `s3.Client` pointed at `httptest` with path-style URLs — so
there is no mocked API object anywhere.

Three distinctions the httptest tier has to make, because each one is a different data-loss bug if it is
collapsed into the others:

- **absent** is a `NoSuchKey`, and is `ErrNoDocument`: a fresh deployment, not a failure;
- **absent bucket** is an error, not an absent document — a missing bucket is a torn-down or never-created
  deployment, and calling it "no document yet" hides a misconfiguration behind the first-run path;
- **a body that will not decode** is an error, not an absent document: an empty or truncated object is a
  write that did not arrive, and reading it as "nothing saved yet" is how a restart adopts nothing and
  then deletes everything.

`TestStoreIsUsableByFirefly` drives the store through `firefly.Store` itself: provision on one `Firefly`,
snapshot from a second one built over the same store, and assert the machine — and its **name** — came
back. That is the package's whole reason for existing, and it is the one assertion that would catch a
store which round-trips everything except the field the rekey added.

### The tailnet client

The second real client, and the one that tests the *interface* rather than a schema: `Joined` is
`authorized && connectedToControl`, `NodeExists` is "the device is there", and every method lists once
because the API has no lookup by name. Three of its cases exist because they are the ones an httptest
server will happily let you get wrong:

- a device that is registered and not connected, or registered and not authorized, is `NodeExists`
  true and `Joined` false — the pair the two methods exist to separate;
- two devices sharing a hostname is `ErrAmbiguousNode` from all three methods, because every tie-break
  is wrong in a way that either marks a machine ready or deletes someone else's node;
- a failing tailnet is an error and never an empty one, because `false` from a listing that did not
  happen is how a machine that never joined gets confirmed gone.

`Forget` is idempotent twice over: a name with no device issues no request at all, and a delete that
races an ephemeral node away is a 404, which is success for the same reason it is on the provider side.

The same package mints the **enrolment key**, and three of its four capabilities are asserted as literals
rather than left to a caller: ephemeral, single-use, preauthorized. Two request-level facts are pinned by
tests because both are silent failures rather than errors: the expiry is whole seconds and **zero means
no expiry at all**, so a sub-second TTL is refused instead of truncated; and the description has a
50-byte cap, so a firefly-minted name of 63 characters is shortened from the front, keeping the counter
that tells two machines of one deployment apart. Each one is load-bearing elsewhere —
ephemeral is what makes `NodeExists` self-clearing, single-use is what stops a key escaping the machine
it was written for, preauthorized is what stops a device waiting for a human and looking like a failed
boot. `TestMintKeyFitsTheRenderer` renders a minted key through `cloudinit.Render`, which is the one
join neither package can check on its own.

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
| relay | a line over the record limit is refused rather than accumulated |
| patch | a diff header escaping the working directory is refused |
| template | a repo URL containing a newline, quote, or `$` never escapes its YAML block |
| attention | an extension entry claiming a state outside the enum is rejected |
| subscriptions | an event for a PR the session does not hold is ignored |

## Fuzzing

Native `testing.F`, corpus committed under `testdata/fuzz/`, seeds replayed on every PR.

| Target | Why |
|---|---|
| `FuzzRelay` | pi's stream is LF-framed with explicit warnings about Unicode separators, backpressure, and partial UTF-8. This is the boundary most likely to eat a real session. |
| `FuzzCloudInit` | cloud-init facts render into YAML that runs as root at boot. A newline in a name is a machine nothing can find. This is a security test with a fuzzer attached. |
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

Only the first row is real. `.github/workflows/go.yml` runs the static tier, the race tier, and the
vulnerability scan on every pull request and on `main`. Everything below it is a plan: there is no
contract-tier job, no fuzz target to seed, no mutation gate, and no nightly schedule yet. The table
says which is which rather than describing all of it as though it runs.

| Trigger | Runs | Budget | Status |
|---|---|---|---|
| pull request | vet, staticcheck, gofmt, unit + integration under `-race`, govulncheck | < 3 min | **real** |
| pull request (same-repo branches) | + contract tier | ~8 min | plan |
| nightly | 10 min per fuzz target, mutation on two packages, contract tier | ~50 min | plan |
| nightly | full tier (one session, one resume) | ~15 min | plan |
| weekly | a real `pi --mode rpc` transcript diffed against the committed golden | ~5 min | plan |
| `workflow_dispatch` | full tier with any tier forced | — | plan |

**No job needs a secret yet**, so all three run on fork pull requests with `contents: read`.

The contract tests exist now, and every one of them skips with a stated reason when its credential is
missing — `contract: VERGE_HETZNER_TOKEN is not set, so there is no project to talk to` — so a fork
still reads as "skipped" rather than "broken". They are deliberately read-only and free: a listing
against a label nothing uses, a `Describe` of an id no server can have, a node lookup for a name that
is not in the tailnet, and a document round-trip under a per-run `contract-state-<nanos>.json`.
The round trip writes and never deletes, so the contract bucket wants a lifecycle rule rather than a
cleanup routine — and `VERGE_R2_BUCKET` must not be the production bucket, both for that rule and
because the round trip writes documents nobody else should read.

One thing the contract tier cannot cover, and it is worth naming so it is not mistaken for a gap in the
tests: a real join. `cmd/verge-join` is exercised against a fake CLI, which proves what it asks the CLI
to do and what it does with the answer, and proves nothing about whether the installed Tailscale honours
`TS_AUTHKEY`. That needs a machine and a tailnet, which is the e2e tier. Provisioning a real
machine, which is what the tier table promises it covers, belongs to the e2e tier: there it is the
thing under test rather than a side effect of checking a decoder, and it is the only part that costs
money. Nothing schedules them yet — the workflow row above is still "plan".

The Go workflow has no path filter: the suite is under a second, so deciding whether to run it would
cost more than running it. Both the tools it installs are pinned, because an unpinned linter is a
build that goes red on a day nobody changed anything.

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
