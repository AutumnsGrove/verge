# Verge

A remote coding runner for [pi](https://github.com/earendil-works/pi).

Prompt from your phone. A VM you rent by the hour picks it up, an agent works in a real
checkout of a real repository, and you watch it think. When nothing is holding the VM open, it
saves its state and disappears. You pay for the hours you used, not for the month.

```
 phone ──► control plane ──► Hetzner API ──► runner VM
              │                               ├─ agent session, repo A
              │                               └─ agent session, repo B
              └── durable state  ◄────────────┘  (session, uncommitted work)
```

## Status

**Partly built, and only the offline parts.** `internal/firefly` has the transition table, the lease
arithmetic, the durable state document with an in-memory store, the lease registry, the reconcile
loop, `Provision`, machine naming, and all three boundary interfaces — provider, private network,
and state sync — each with a fake. The Hetzner client is written but has never been pointed at the
real API: the unit tier runs it against `httptest`, and the contract tier that would use a real token
does not exist yet. The tailnet client and the R2 state store exist and are tested against `httptest`,
and neither has been pointed at real infrastructure. The cloud-init renderer exists and is fuzzed, and the enrolment-key minter it
feeds exists, but nothing composes the two yet. `internal/syncer` saves a machine's workspace as one
tarball in R2, and `internal/syncer/ssh` fetches it by running one command on the machine over
Tailscale SSH. What is still missing is the machine side of that contract: the image has to provide
`/usr/local/bin/verge-save`, the sibling of the `verge-join` the renderer already assumes. Until it
does, a save fails with the command's own error rather than quietly destroying a machine it could not
save — which is the behaviour the ordering guarantee asks for.
`cmd/verge` reconciles on a timer: it builds the clients, calls `Tick` on every interval, and finishes
the pass in flight when it is interrupted. It does not provision anything yet — placement, the caller
that decides when work needs a machine, is still to come. `docs/testing.md` names the files the rest
will become, and `docs/firefly.md` is the design they implement.

This repository is a fresh start in Go, on a different architecture from the version whose history
precedes this commit. The earlier TypeScript/Cloudflare Code implementation is kept in
[`legacy/grove-bloom/`](legacy/grove-bloom/) for reference only; it is not built, not run, and not
supported.

## What it is

- **One shared machine by default.** Several sessions live on one VM, each with its own Unix
  user, working directory, and memory limit. A dedicated machine is the same code with a
  capacity of one.
- **Nothing survives the VM.** No volumes, no persistent disk. State lives in object storage and
  is restored on the next boot; repositories are cloned fresh at a recorded commit.
- **Two lifetimes.** A session stops when it has nothing left to do. The machine fades when no
  session, turn, or subscription is holding it open, with a hard cap above everything.
- **No model credentials on the VM.** The runner holds a revocable per-session token and talks to
  a proxy that owns the provider keys, counts spend, and enforces the caps.
- **Pi over RPC.** The agent is driven as a long-lived process speaking JSONL, so the UI shows
  thinking and tool calls as they happen and a nudge from a phone arrives mid-run instead of
  interrupting it.

## Testing

Two of the three tiers need real infrastructure, because the failures worth catching are cloud
failures: a machine that never becomes ready, a delete that races a save, a node that lingers on a
private network.

| Tier | Needs | What it proves |
|---|---|---|
| unit | nothing | the state machine, leases, placement, ordering logic, against fakes |
| contract | a Hetzner project, object storage, a private-network key | a real machine boots, joins, takes a lease, syncs, and is destroyed — including that a failed save leaves it running |
| full | the above, plus a model provider | one real session, end to end, and a resume on a fresh machine |

Provisioning takes minutes, so the tiers are gated rather than run on every save. The contract
tier always runs against a **separate cloud project** with its own credentials: the sweep
primitive is destructive by design, and a test must never be able to see a production machine.

## Releases and the image

`make dist` cross-compiles three static linux binaries — `verge` (the control plane),
`verge-join` and `verge-save` (the machine's half) — into `dist/` with checksums. Pushing a `v*`
tag runs the same target in `.github/workflows/release.yml` and publishes the tarballs as a GitHub
Release, so what an image installs is exactly what a developer can rebuild from the tag.

The image owes two things, both named by the code that depends on them:

- `/usr/local/bin/verge-join` — run once at boot by cloud-init, which renders the facts it reads
  into `/etc/verge/join.json` (see `internal/cloudinit`). It joins the private network with
  Tailscale SSH enabled, because that is how the control plane saves a workspace.
- `/usr/local/bin/verge-save` — run by the control plane over ssh, writes the workspace to stdout
  as a tar stream. A machine without it fails its save loudly and is kept rather than destroyed.

## The plan

The detailed plan — decisions, the two-level lifecycle, capacity, the proxy contract, the network
boundary, and the build order — lives in the homelab repository's `docs/verge-plan.md`, kept
private because it names the specific deployment. This repository keeps the design that stands on
its own.
