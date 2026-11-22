# legacy/grove-bloom

The previous design, kept read-only. It was a Cloudflare Worker orchestrator (D1, SvelteKit
dashboard, `ttyd` terminal iframe) driving Kilo Code on a Hetzner VPS, with prebuilt repositories
in R2. It got as far as a working dashboard and a Worker; it never ran a real session.

It is here for one reason: these four scripts are the only prose-and-shell record of what it
actually takes to boot a machine that can do work, and the current design reads them before
writing its own template.

## What is here

| File | What it did |
|---|---|
| `cloud-init.yaml` | the full boot: Node 22, pnpm, Kilo Code CLI, rclone, ttyd on 7681, a heartbeat daemon, and an R2 restore of prebuilt `node_modules` |
| `daemon.sh` | polled a timestamp file every 30 s, posted heartbeats, and posted a shutdown trigger on idle |
| `run-task.sh` | ran Kilo in autonomous mode with a timeout, then posted a completion webhook |
| `sync-to-r2.sh` | tarred `node_modules` per project and a workspace snapshot, uploaded with rclone |

## What survived into the current design

- **The Hetzner metadata service**: `169.254.169.254/hetzner/v1/metadata/{instance-id,public-ipv4}`
  answers from inside the machine, no API call needed.
- **The systemd unit shape**: `EnvironmentFile=` plus `Restart=always` for the on-machine agent.
- **The idle-and-shut-down idea**, and nothing about its implementation. `daemon.sh` polls a file
  written by a `ttyd` wrapper; the current design uses leases with expiries, because a file that
  nobody updates cannot distinguish "thinking" from "abandoned".

## What was deliberately dropped

Everything else, and most of it for the same reason: it put the design's own logic on the wrong
side of a single program.

- the Worker, D1, and the bearer-secret webhooks from machine to orchestrator — the orchestrator
  now runs next to the state it owns, and reaches the machine directly
- `ttyd` — the UI renders the agent's structured stream instead of an iframe around a terminal
- prebuilt repositories and `node_modules.tar.gz` in R2 — machines clone at a recorded commit, so
  there is nothing to prebuild and nothing to keep in sync
- Kilo Code and its two-model OpenRouter setup
- **the OpenRouter key written into `/root/.kilocode/config.json` on the machine.** This is the
  one that matters: a provider credential on a box running an autonomous agent is the exact thing
  the proxy exists to prevent. If you read one line of this directory, read that one and notice
  that the current design has no equivalent file.

## Lost on purpose

The five `claude/*` branches were deleted with this commit. Four were fully merged into `main`.
The fifth, `claude/review-tasks-migration-ESRPT`, held one unmerged commit, `e44d9d9`, recording
a task-migration review; it described a design that is no longer being built. The commit object
may still be reachable by SHA through the GitHub API for a while.

The full specification this implemented was `docs/grove-bloom-spec.md` in this repository. The
surviving copy is `lattice/docs/specs/verge-spec.md`, and the current plan's carry-over table
reads it for what is worth keeping.
