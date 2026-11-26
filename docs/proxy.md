# The model proxy

The proxy is the one thing on the control plane that holds a provider key. The README's rule is
that **no model credentials reach the machine**: a session's machine holds a revocable
per-session token and talks to this proxy, which authenticates the token, attaches the real
credential, relays the upstream's bytes, and records what the request cost.

Three upstreams, fixed by the deployment rather than by a registry:

| Route | Upstream | Wire shape |
|---|---|---|
| `/go/anthropic/*` | OpenCode Go | Anthropic's message protocol |
| `/go/openai/*` | OpenCode Go | OpenAI's chat-completions protocol |
| `/or/*` | OpenRouter | an OpenAI-compatible dialect that also reports its own cost |

Autumn settled that on 2026-11-26. The route prefixes are passed through to the upstream base URL
with the prefix rewritten; model IDs are not interpreted, because a proxy with a model registry is
a proxy that has to be updated on the day a model ships.

## Relaying is protocol-transparent, usage is not

The proxy does not re-encode bodies. A proxy that understood three APIs would be three
implementations to keep in step, and every streaming bug in it would be a session somebody lost.
It copies the upstream's bytes to the client and reads a **copy** on the way past.

What it understands is usage, from the place each protocol puts it:

| Shape | Where the counts are | What they are called |
|---|---|---|
| Anthropic | `message_start.message.usage` (input, cache reads), `message_delta.usage` (output) | `input_tokens`, `output_tokens`, `cache_read_input_tokens` |
| OpenAI | the final chunk, when `stream_options.include_usage` asked for it | `prompt_tokens`, `completion_tokens`, `prompt_tokens_details.cached_tokens` |
| OpenRouter | the same object, plus the charge | `usage.cost`, in USD |

They collapse into one `Usage`: `Input` (the whole prompt), `Output`, `Cached` (the cache-read subset),
`CacheWrite` (Anthropic only), and optionally the upstream's own cost.

The one trap in that table is Anthropic's: its three prompt counts are **disjoint**, not nested.
`input_tokens` is the uncached remainder, so a parser that read it as the total would bill a
905,010-token cached prompt as 10 tokens -- and a long cached session is exactly where a cap has
to be honest. `Input` is therefore computed, not copied, and the two subsets are billed at their
own rates.

## Decisions this package made

1. **The parser never refuses the client's bytes.** `Stream.Write` always succeeds. An event this
   package cannot read is a row with a warning, never a session that broke: the relay already sent
   it. A line or event over ten megabytes is recorded (`ErrEventTooLong`) and skipped.
2. **A usage the ledger cannot read is not a free request and not a crash.** An HTML error page
   behind a 200 is `ErrNotJSON`; a JSON body with no usage is `ErrNoUsage`; a body whose counts are
   strings is `ErrNoUsage` rather than a guessed number. `encoding/json` accepts a quoted number into
   a `json.Number`, so the refusal is explicit.
3. **A negative count is zero.** Both in parsing and again in `Ledger.Record`, because spend must not
   go *down* when an upstream says so — a negative that reached the ledger would walk a session
   straight through its cap. A `null` or non-numeric cost is *not* an answer: `encoding/json`
   decodes `"cost": null` into a zero without an error, which read as an answer would make a million
   tokens a priced request at nothing. It falls back to the price table instead.
4. **The upstream's own cost beats the table.** OpenRouter charges and says so; its number is better
   than any price list kept here. The last reported cost wins, **zero included** — a stream whose
   final usage says `cost: 0` is a free request, not an absent answer, and the same response must be
   billed the same way whether it streamed or not.
5. **An unknown model is recorded unpriced.** Refusing would fail on every model that shipped this
   week; inventing a price would be a lie. The row carries its tokens, `Priced: false`, and zero
   cost, so the hole is visible instead of hidden.
6. **A cap is a gate, not a hard ceiling.** The check happens before the upstream request, because a
   request refused after it was sent was billed anyway. The request that crosses the line finishes.
   Making it exact would need the cost of output that has not been generated.
7. **The day is UTC.** Machines and the control plane have to agree on when a day ends, and only
   UTC is a fact for both.
8. **Chunk boundaries do not matter.** SSE framing is parsed from a byte stream, so TCP decides
   where records split and the parser must not. `FuzzStream` asserts that the same body parses the
   same way one byte at a time as in one write.

## Not built yet

- **The HTTP server.** No route is served, no token is authenticated, no upstream is dialled. This
  package is the ledger and the parser the server will sit on.
- **Token minting and authentication.** The plan's `X-Verge-Session` token is minted when a session
  starts and revoked when it stops; neither exists.
- **Durable spend.** The ledger is in memory, so a restart forgets the day's spend and the day cap
  is per process lifetime. The plan's SQLite is the next piece; bolting durability onto an
  in-memory ledger would be two sources of truth on the day it mattered.
- **Price data.** The table is configuration with nothing in it yet, because prices are not this
  repository's to guess. `CacheWrite` has no default on purpose: a table that leaves it zero charges
  cache writes nothing, and "zero means free" and "zero means input" are both plausible guesses.

## Tests

Offline, against the three wire shapes as the protocols actually put them. `TestUsageFromJSON`
covers the non-streamed bodies (including Anthropic's disjoint counts and a `null` cost),
`TestStream` the event streams (chunking, CRLF, comments, multi-line `data:`, an unterminated last
event, a refused enormous line, and a reported cost handled the same way streamed or not),
`TestPricing` and `TestLedger` the money (cached-read and cache-write rates, an unknown model, an
upstream cost, both caps, the day boundary, concurrent requests under `-race`).

`FuzzStream` asserts chunking-independence, non-negative usage, and that `Write` never refuses
bytes, over all three protocols. Seeds include the hostile shapes: HTML, negative counts, quoted
numbers.

Mutation-checked: removing the quoted-number refusal, the `null`-cost refusal, either negative
clamp, the Anthropic total, the upstream-cost preference (or its zero rule), the day rollover, or the
dispatch of an unterminated final event each fails a case that exists for it.
