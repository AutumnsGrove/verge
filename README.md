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

**Partly built, and only the pure parts.** `internal/firefly` has the transition table and the lease
arithmetic, with tests, and nothing else exists: no reconcile loop, no cloud or network client, no
state store. `docs/testing.md` names the files the rest will become, and `docs/firefly.md` is the
design they implement.

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

## The plan

The detailed plan — decisions, the two-level lifecycle, capacity, the proxy contract, the network
boundary, and the build order — lives in the homelab repository's `docs/verge-plan.md`, kept
private because it names the specific deployment. This repository keeps the design that stands on
its own.
