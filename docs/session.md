# The session layer

`internal/session` is the control plane's half of one pi session. It places the session on a
machine, runs `pi --mode rpc` there, relays records in both directions, and gives the machine back
when the stream ends.

The data path is the decision this package exists to implement (Autumn, 2026-11-26): **pi runs on the
machine, and the control plane relays its JSONL over ssh.** The phone never connects to a machine,
pi never runs on the control plane, and the tailnet never has to expose an agent to a phone.

```
 phone ──► control plane ──ssh──► machine
              │  relay JSONL        └─ pi --mode rpc, in /workspace/<id>
              │  hold the lease
              └── release when the stream ends
```

## What it joins

Nothing in the package is new machinery except the joining, which is why it is small:

| Piece | From | Role here |
|---|---|---|
| `placement.Place` | `internal/placement` | decides which machine, and takes the lease |
| `Client.Run` | `internal/machine/ssh` | starts one command on the machine, both directions |
| `relay.Reader`, `relay.Writer` | `internal/rpc/relay` | LF framing; the records themselves are opaque |
| `firefly.Renew`, `Release` | `internal/firefly` | the lease the session holds |

## Lifecycle

**Open** returns one of three things, matching placement's answers because this is where they become a
caller's — and it takes no instant: the manager owns the clock, because placement and the renewal
window are the same arithmetic on the same lease.

- a `*Session`: pi is running and the lease is held;
- `ErrPending`: the machine is booting, and the caller asks again — a retry is cheap and does not buy a
  second machine;
- an error: the deployment is at its cap, or something failed.

Then `Next` reads one record from pi and `Send` writes one to it. `Close` ends the session.

The lease is held for the session's life. That is the part firefly was built for: while a session
holds it, the machine is not draining, and when the last one lets go, `DrainWindow` starts.

## Decisions this package made

The specification settled the data path and the lifetimes. These seven are judgment calls, recorded
because a later document would otherwise invent a different answer.

1. **The lease is renewed by stream activity, not by existence.** A record in either direction renews
   it, at most once per half TTL. A goroutine with a ticker was rejected: it is a second clock in a
   design that injects the single one it has, and "the agent is thinking" is exactly the activity the
   previous design could not detect. The cost is stated rather than hidden — a session that says
   nothing for a whole TTL loses its machine to the drain window, and the next record reports the
   lapse. A renewal that fails *without* a lapse leaves the window where it was, so the next record
   tries again: moving it on failure would put the retry at the lease's own expiry, the one moment a
   retry cannot help.

2. **The lease is released on a context that cannot be cancelled.** The usual reason a stream ended is
   that the caller gave up, which cancels the session's context; a release that inherited it would fail
   exactly when giving the machine back matters most. `MemStore` refuses a cancelled context, so this
   is a tested rule rather than a comment.

3. **A process that cannot start does not keep its machine.** The lease was taken to run something.
   Releasing it on the failure path is the difference between a machine that drains and one that bills
   for a session nobody can reach.

4. **The session id is validated once, as an id.** The same string is a holder name, a directory under
   the workspace, and pi's session id. Lowercase letters, digits, dashes and underscores, starting with
   a letter or digit — refused rather than sanitised, because a mangled id is a session nobody finds
   again. Uppercase is refused as well as punctuation: the value becomes a path, and two ids differing
   only in case are one directory on a case-insensitive filesystem.

5. **One directory per session under one workspace root.** `/workspace/<id>`, because a machine holds up
   to four sessions by policy and two pi processes in one checkout would edit each other's files.
   `verge-save` archives the whole root, so every session's directory travels with the save.

6. **pi's session directory is inside the workspace.** `--session-dir /workspace/<id>/.pi/sessions`
   plus `--session-id <id>`, because the session file is what a resume replays and the only state that
   survives a machine is what the save archives. Outside the workspace it would be lost with the
   machine, which is the failure the whole design is arranged to avoid.

7. **A clean end of stream is not a clean end of session.** pi can emit its last record and then exit
   non-zero, so `Next` asks the process why when the stream ends, and reports the exit status in place
   of `io.EOF`. A record that arrives together with a lapsed lease is still delivered, and the lapse is
   the next call's answer: the last thing a session said is worth more than the tidiness of failing
   early.

`Close` follows pi's own contract: close stdin to ask for an orderly shutdown, wait bounded by the
caller's deadline, kill what is still running at that deadline, then release the lease. It reports the
exit status too, so a session that ended by crashing says so rather than looking like a clean stop.

## What is not built

Named so this document is not read as a claim:

- **The phone-facing API.** Nothing serves a WebSocket, so `Manager.Open` has no caller outside tests
  and `cmd/verge` does not construct one. The interface it will grow — attach, list, detach — belongs
  with that server, not here.
- **The model proxy.** No provider key reaches the machine today because no pi on a machine can reach a
  model yet. The proxy and its ledger are the next subsystem, and `docs/testing.md` lists the cases.
- **The image's `pi`.** The command assumes `/usr/local/bin/pi`-on-`PATH` and the versions the plan
  pins; no image installs it.
- **A per-session Unix user.** Today every session runs as the one account Tailscale SSH admits. The
  plan's per-session uid is what would separate two sessions' files and memory, and it is not built.
- **Resume.** The session file is in the workspace, so the save carries it, but nothing restores it into
  a fresh machine yet.

## Tests

The suite is an integration test against the real placement, the real firefly, and the shared fake
cloud and fake clock. Only the machine's process is a double — the far side is a network and a
computer, which is the one seam `docs/testing.md` allows a double at. The fake's pipes are real, so a
read the session makes blocks and is interrupted as it would be against ssh.

The lease's write rate is asserted by counting store writes, not by watching liveness: three records
inside the half TTL write nothing, three past it write once, and a failed renewal is retried by the
very next record while the old lease is still live.

What those tests prove: pending starts no process and buys no second machine; the command is the
image's contract and names the session's own directory; records cross both ways; the lease is renewed
by activity and a lapse is reported; the end of the stream releases the lease and the machine fades;
a failed start releases it too; an orderly close never kills, and a close past its deadline does; a
cancelled caller still gets its machine back; and an id that would be a path or a flag is refused
before anything is provisioned.

What they cannot prove, and the contract tier would: that a real Tailscale SSH session admits the
control plane, that the machine's `pi` speaks this framing, and that a real workspace survives the
save.
