// Package proxy is the model proxy: the one thing on the control plane that holds a
// provider key, counts spend, and enforces the caps.
//
// The rule it exists for is the README's: **no model credentials on the machine.** A
// session's machine holds a revocable per-session token and talks to this proxy; the proxy
// authenticates the token, attaches the real credential, relays the upstream's bytes
// unchanged, and records what the request cost.
//
// The relay is deliberately protocol-transparent. It does not re-encode, because a proxy
// that understood the bodies would be a second implementation of three APIs, and every
// streaming bug in it would be a session somebody lost. What it does understand is
// *usage* -- tokens and money -- which it reads from a copy of the bytes on their way past.
//
// # Three wire shapes, one ledger
//
// The deployment's upstreams are fixed: OpenCode Go speaks Anthropic's message protocol and
// OpenAI's completions protocol on two routes, and OpenRouter speaks an OpenAI-compatible
// completions protocol that also reports its own cost. Usage lives in a different place in
// each:
//
//	Anthropic      message_start.usage (input, cache reads) + message_delta.usage (output)
//	OpenAI         the final chunk's usage, when stream_options.include_usage asked for it
//	OpenRouter     the same object, plus usage.cost in dollars
//
// Protocol is the enum those three routes need, Usage is the one row they all become, and
// Ledger is what a cap is checked against.
package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Protocol is the wire shape of an upstream, not the name of a company: the two OpenCode
// Go routes speak two protocols, and OpenRouter speaks a third dialect of the second.
type Protocol int

const (
	// Anthropic is the message protocol: usage arrives in events, not in a body.
	Anthropic Protocol = iota + 1

	// OpenAI is the chat-completions protocol: usage arrives in the final chunk when the
	// request asked for it, and in the body when the request was not streamed.
	OpenAI

	// OpenRouter is the same shape with one addition that matters: the final chunk can
	// carry the cost the router actually charged, which is better than any table we keep.
	OpenRouter
)

// String names the protocol for logs and errors.
func (p Protocol) String() string {
	switch p {
	case Anthropic:
		return "anthropic"
	case OpenAI:
		return "openai"
	case OpenRouter:
		return "openrouter"
	default:
		return fmt.Sprintf("protocol(%d)", int(p))
	}
}

// Valid reports whether p is one of the three shapes.
func (p Protocol) Valid() bool { return p >= Anthropic && p <= OpenRouter }

var (
	// ErrConfig means the proxy cannot be built from its configuration.
	ErrConfig = errors.New("proxy: invalid configuration")

	// ErrNotJSON means the body was not JSON at all. The case that matters is a gateway
	// answering 200 with an HTML error page: reading that as a response with no usage
	// would record a free request and hand the client markup to parse.
	ErrNotJSON = errors.New("proxy: the body is not JSON")

	// ErrNoUsage means the body is JSON but carries no usage a caller can count. It is not
	// a transport failure and not free either: the caller records the tokens it can see
	// and says the row is unpriced.
	ErrNoUsage = errors.New("proxy: the response carries no usage")
)

// Usage is what one request cost, in tokens.
//
// Input is the whole prompt: every token the request sent, of which Cached and CacheWrite
// are the two subsets a provider charges differently for. The three protocols disagree
// about whether that total is stated or implied, which is why the parser computes it -- see
// usageObject -- rather than copying a field.
//
// Priced and Cost are the upstream's own answer, when it gave one (OpenRouter does); an
// empty Cost means the ledger prices the tokens itself. CacheWrite is Anthropic-only:
// OpenAI and OpenRouter report cache reads and nothing about writes.
//
// Every field is clamped at zero in parsing. A negative token count from an upstream would
// otherwise be a way to *lower* a session's spend and walk straight through its cap.
type Usage struct {
	Input      int64
	Output     int64
	Cached     int64
	CacheWrite int64

	// Cost is the upstream's charge in USD, when it reported one.
	Cost float64

	// Reported says Cost came from the upstream rather than from a price table.
	Reported bool
}

// merge folds a later event's usage into this one.
//
// The three protocols report running totals, so the later value wins for anything that grew.
// Costs arrive only at the end, so the last non-zero cost wins too.
func (u *Usage) merge(next Usage) {
	if next.Input > u.Input {
		u.Input = next.Input
	}
	if next.Output > u.Output {
		u.Output = next.Output
	}
	if next.Cached > u.Cached {
		u.Cached = next.Cached
	}
	if next.CacheWrite > u.CacheWrite {
		u.CacheWrite = next.CacheWrite
	}
	// The last cost the upstream reported wins, zero included. A stream whose final usage
	// says cost 0 is a free request, not an absent answer -- and treating the two differently
	// meant the same response was billed from the price table when it streamed and recorded
	// as free when it did not.
	if next.Reported {
		u.Cost = next.Cost
		u.Reported = true
	}
}

// UsageFromJSON reads the usage out of a complete (non-streamed) response body.
//
// It returns ErrNotJSON for a body that is not JSON -- an HTML error page behind a 200 is
// exactly this -- and ErrNoUsage when the JSON carries no usage at all.
func UsageFromJSON(protocol Protocol, body []byte) (Usage, error) {
	if !protocol.Valid() {
		return Usage{}, fmt.Errorf("%w: %s", ErrConfig, protocol)
	}
	if !json.Valid(body) {
		return Usage{}, ErrNotJSON
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		// A JSON array or scalar is valid JSON and not a response object.
		return Usage{}, ErrNoUsage
	}
	u, ok := usageObject(protocol, root)
	if !ok {
		return Usage{}, ErrNoUsage
	}
	return u, nil
}

// StreamEvent reads the usage out of one streamed event payload.
//
// It returns ErrNoUsage for an event that carries none, which is the ordinary case: most
// events are content. It never reports an error for an event it cannot understand, because
// understanding events is not this package's job -- relaying them is.
func StreamEvent(protocol Protocol, payload []byte) (Usage, error) {
	if !json.Valid(payload) {
		return Usage{}, ErrNoUsage
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return Usage{}, ErrNoUsage
	}
	u, ok := usageObject(protocol, root)
	if !ok {
		return Usage{}, ErrNoUsage
	}
	return u, nil
}

// usageObject finds the usage object in one decoded JSON object and reads it.
//
// The shapes are the three the package doc lists, plus Anthropic's habit of nesting the
// first one under "message". A field whose value is not a number is zero rather than an
// error: an upstream that changes a type must not take the ledger down with it.
func usageObject(protocol Protocol, root map[string]json.RawMessage) (Usage, bool) {
	var counts map[string]json.RawMessage

	switch protocol {
	case Anthropic:
		// message_start nests it; message_delta puts it at the top level.
		if msg, ok := object(root["message"]); ok {
			if u, ok := object(msg["usage"]); ok {
				counts = u
			}
		}
		if counts == nil {
			if u, ok := object(root["usage"]); ok {
				counts = u
			}
		}
	case OpenAI, OpenRouter:
		if u, ok := object(root["usage"]); ok {
			counts = u
		}
	}
	if counts == nil {
		return Usage{}, false
	}

	var u Usage
	present := false
	read := func(key string, into *int64) {
		if n, ok := number(counts[key]); ok {
			*into = clampNegative(n)
			present = true
		}
	}

	switch protocol {
	case Anthropic:
		// Anthropic's three prompt counts are disjoint, not nested: input_tokens is the
		// uncached remainder. Reading input_tokens as the total under-bills every cached
		// prompt -- and a long cached session is exactly where a cap has to be honest --
		// while dropping cache_creation_input_tokens loses the most expensive tokens of all.
		var uncached, cached, written int64
		if n, ok := number(counts["input_tokens"]); ok {
			uncached = clampNegative(n)
			present = true
		}
		if n, ok := number(counts["cache_read_input_tokens"]); ok {
			cached = clampNegative(n)
			present = true
		}
		if n, ok := number(counts["cache_creation_input_tokens"]); ok {
			written = clampNegative(n)
			present = true
		}
		// Add rather than multiply in int64: hostile counts can each be MaxInt64 and the sum
		// would wrap negative, which is a cap bypass built out of a type.
		u.Input = saturatingAdd(saturatingAdd(uncached, cached), written)
		u.Cached = cached
		u.CacheWrite = written
		read("output_tokens", &u.Output)
	case OpenAI, OpenRouter:
		read("prompt_tokens", &u.Input)
		read("completion_tokens", &u.Output)
		// OpenAI nests the cache-read count one level down; OpenRouter sometimes reports
		// it flat as well, so both are read.
		if details, ok := object(counts["prompt_tokens_details"]); ok {
			if n, ok := number(details["cached_tokens"]); ok {
				u.Cached = clampNegative(n)
				present = true
			}
		}
		if n, ok := number(counts["cached_tokens"]); ok {
			u.Cached = clampNegative(n)
			present = true
		}
		if protocol == OpenRouter {
			if cost, ok := money(counts["cost"]); ok {
				u.Cost = cost
				u.Reported = true
				present = true
			}
		}
	}
	return u, present
}

// object decodes a value that should be a JSON object.
func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

// number reads a JSON number, and refuses a string that looks like one.
//
// encoding/json would happily decode "12" into a json.Number, so the refusal has to be
// explicit. A quoted number is a schema change, and a ledger that guessed would count
// tokens nobody described: the strict answer makes the row unpriced and visible instead.
func number(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || raw[0] == '"' || raw[0] == 'n' || raw[0] == '[' {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return i, true
}

// money reads a USD amount.
//
// A JSON null decodes into a float64 without an error and leaves it zero, so "cost": null
// would otherwise be the answer "this request was free" -- a million tokens recorded as
// priced at nothing, and a cap that never counts them. Null, a string, a negative and a
// non-number are all "the upstream did not say", which falls back to the price table; an
// explicit zero is an answer and is charged as one.
func money(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || raw[0] == 'n' || raw[0] == '"' || raw[0] == '[' || raw[0] == '{' || raw[0] == 't' || raw[0] == 'f' {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	if f < 0 {
		// A negative charge is not an answer either: fall back to the table rather than
		// recording a free request.
		return 0, false
	}
	return f, true
}

// saturatingAdd adds two non-negative counts without wrapping.
func saturatingAdd(a, b int64) int64 {
	if a > mathMaxInt64-b {
		return mathMaxInt64
	}
	return a + b
}

const mathMaxInt64 = int64(^uint64(0) >> 1)

func clampNegative(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
