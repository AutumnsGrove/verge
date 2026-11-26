package proxy_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/AutumnsGrove/verge/internal/proxy"
)

// The three shapes are taken from the protocols themselves, trimmed to the fields the
// ledger reads. Each is a real response's skeleton rather than a shape this package
// invented, because a ledger that only parses invented shapes is a ledger that parses
// nothing in production.

const anthropicBody = `{"id":"msg_01","type":"message","role":"assistant",
 "content":[{"type":"text","text":"hi"}],
 "usage":{"input_tokens":25,"output_tokens":7,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}`

const openAIBody = `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}],
 "usage":{"prompt_tokens":30,"completion_tokens":5,"total_tokens":35,"prompt_tokens_details":{"cached_tokens":12}}}`

const openRouterBody = `{"id":"gen-1","choices":[{"message":{"role":"assistant","content":"hi"}}],
 "usage":{"prompt_tokens":30,"completion_tokens":5,"total_tokens":35,"cost":0.00042}}`

func TestUsageFromJSON(t *testing.T) {
	t.Run("happy/anthropic_counts_cache_reads_separately", func(t *testing.T) {
		u, err := proxy.UsageFromJSON(proxy.Anthropic, []byte(anthropicBody))
		if err != nil {
			t.Fatalf("UsageFromJSON: %v", err)
		}
		want := proxy.Usage{Input: 25, Output: 7, Cached: 10}
		if u != want {
			t.Fatalf("usage = %+v, want %+v", u, want)
		}
	})

	t.Run("happy/openai_nests_the_cached_count", func(t *testing.T) {
		u, err := proxy.UsageFromJSON(proxy.OpenAI, []byte(openAIBody))
		if err != nil {
			t.Fatalf("UsageFromJSON: %v", err)
		}
		want := proxy.Usage{Input: 30, Output: 5, Cached: 12}
		if u != want {
			t.Fatalf("usage = %+v, want %+v", u, want)
		}
	})

	t.Run("happy/openrouter_reports_its_own_cost", func(t *testing.T) {
		// A router's number is better than any table kept here: it is what was charged,
		// including whichever upstream served the request.
		u, err := proxy.UsageFromJSON(proxy.OpenRouter, []byte(openRouterBody))
		if err != nil {
			t.Fatalf("UsageFromJSON: %v", err)
		}
		if !u.Reported || u.Cost != 0.00042 {
			t.Fatalf("usage = %+v, want the router's own cost", u)
		}
	})

	t.Run("sad/an_html_error_page_is_not_a_response", func(t *testing.T) {
		// The bad case docs/testing.md names: a gateway answering 200 with HTML. Reading it
		// as a response with no usage would record a free request and hand the client markup
		// to parse.
		body := `<!doctype html><html><body><h1>502 Bad Gateway</h1></body></html>`
		if _, err := proxy.UsageFromJSON(proxy.OpenAI, []byte(body)); !errors.Is(err, proxy.ErrNotJSON) {
			t.Fatalf("UsageFromJSON = %v, want %v", err, proxy.ErrNotJSON)
		}
	})

	t.Run("sad/a_body_with_no_usage_is_not_an_error_but_is_not_a_cost", func(t *testing.T) {
		for name, body := range map[string]string{
			"no usage object": `{"id":"chatcmpl-1","choices":[]}`,
			"empty usage":     `{"usage":{}}`,
			"empty body":      ``,
			"a JSON array":    `[1,2,3]`,
			"a JSON string":   `"hi"`,
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := proxy.UsageFromJSON(proxy.OpenAI, []byte(body)); !errors.Is(err, proxy.ErrNotJSON) && !errors.Is(err, proxy.ErrNoUsage) {
					t.Fatalf("UsageFromJSON = %v, want ErrNotJSON or ErrNoUsage", err)
				}
			})
		}
	})

	t.Run("bad/a_string_where_a_number_belongs_does_not_crash", func(t *testing.T) {
		// docs/testing.md: a type change upstream must not take the ledger down with it. The
		// row is then unpriced and visible, rather than the request being free or fatal.
		body := `{"usage":{"prompt_tokens":"30","completion_tokens":"5"}}`
		if _, err := proxy.UsageFromJSON(proxy.OpenAI, []byte(body)); !errors.Is(err, proxy.ErrNoUsage) {
			t.Fatalf("UsageFromJSON = %v, want %v", err, proxy.ErrNoUsage)
		}
	})

	t.Run("bad/a_negative_token_count_is_zero", func(t *testing.T) {
		// Spend must not go down because an upstream said so: a negative that reached the
		// ledger would lower a session's total and walk it through its cap.
		body := `{"usage":{"prompt_tokens":-500,"completion_tokens":3,"cost":-2}}`
		u, err := proxy.UsageFromJSON(proxy.OpenRouter, []byte(body))
		if err != nil {
			t.Fatalf("UsageFromJSON: %v", err)
		}
		if u.Input != 0 || u.Cost != 0 {
			t.Fatalf("usage = %+v, want the negatives clamped to zero", u)
		}
		if u.Output != 3 {
			t.Fatalf("Output = %d, want the readable field kept", u.Output)
		}
	})

	t.Run("bad/an_unknown_protocol", func(t *testing.T) {
		if _, err := proxy.UsageFromJSON(proxy.Protocol(9), []byte(openAIBody)); !errors.Is(err, proxy.ErrConfig) {
			t.Fatalf("UsageFromJSON = %v, want %v", err, proxy.ErrConfig)
		}
	})
}

func TestStream(t *testing.T) {
	// The Anthropic events that carry usage: input on message_start, output as a running
	// total on message_delta.
	const anthropicStream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_01","usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":10}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n"

	const openAIStream = `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":30,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":12}}}` + "\n\n" +
		"data: [DONE]\n\n"

	parse := func(t *testing.T, protocol proxy.Protocol, body string) (proxy.Usage, bool) {
		t.Helper()
		u, ok, err := proxy.ParseStream(protocol, strings.NewReader(body))
		if err != nil {
			t.Fatalf("ParseStream: %v", err)
		}
		return u, ok
	}

	t.Run("happy/anthropic_input_on_the_first_event_output_on_the_last", func(t *testing.T) {
		u, ok := parse(t, proxy.Anthropic, anthropicStream)
		if !ok {
			t.Fatal("the stream reported no usage")
		}
		want := proxy.Usage{Input: 25, Output: 42, Cached: 10}
		if u != want {
			t.Fatalf("usage = %+v, want %+v", u, want)
		}
	})

	t.Run("happy/openai_usage_in_the_final_chunk_and_DONE_is_not_json", func(t *testing.T) {
		u, ok := parse(t, proxy.OpenAI, openAIStream)
		if !ok {
			t.Fatal("the stream reported no usage")
		}
		want := proxy.Usage{Input: 30, Output: 5, Cached: 12}
		if u != want {
			t.Fatalf("usage = %+v, want %+v", u, want)
		}
	})

	t.Run("happy/chunk_boundaries_do_not_change_the_answer", func(t *testing.T) {
		// The property that matters for a tee: a body delivered one byte at a time is the
		// same body. FuzzStream generalises it; this is the readable instance.
		s, err := proxy.NewStream(proxy.Anthropic)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		for i := 0; i < len(anthropicStream); i++ {
			if _, err := s.Write([]byte{anthropicStream[i]}); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		u, ok := s.Usage()
		if !ok || u.Output != 42 || u.Input != 25 {
			t.Fatalf("byte-at-a-time usage = %+v (%v), want the whole-body answer", u, ok)
		}
	})

	t.Run("happy/crlf_and_comments_and_keepalives", func(t *testing.T) {
		body := ": keep-alive\r\n\r\n" +
			"event: message_delta\r\n" +
			`data: {"type":"message_delta","usage":{"output_tokens":9}}` + "\r\n\r\n"
		u, ok := parse(t, proxy.Anthropic, body)
		if !ok || u.Output != 9 {
			t.Fatalf("usage = %+v (%v), want 9 output tokens", u, ok)
		}
	})

	t.Run("happy/a_multi_line_data_event_is_one_payload", func(t *testing.T) {
		// SSE joins data: lines with a newline before parsing, and JSON does not care where
		// the whitespace is.
		body := "data: {\"usage\":{\n" +
			`data: "prompt_tokens":11,"completion_tokens":2}}` + "\n\n" +
			"data: [DONE]\n\n"
		u, ok := parse(t, proxy.OpenAI, body)
		if !ok || u.Input != 11 || u.Output != 2 {
			t.Fatalf("usage = %+v (%v), want the joined event's usage", u, ok)
		}
	})

	t.Run("happy/an_unterminated_last_event_is_still_read", func(t *testing.T) {
		// A connection cut between the final record and its blank line still produced a
		// complete record, and the relay already gave it to the client.
		body := `data: {"usage":{"prompt_tokens":4,"completion_tokens":1}}`
		u, ok := parse(t, proxy.OpenAI, body)
		if !ok || u.Input != 4 {
			t.Fatalf("usage = %+v (%v), want the cut-off event", u, ok)
		}
	})

	t.Run("sad/a_stream_with_no_usage_says_so", func(t *testing.T) {
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
		if _, ok := parse(t, proxy.OpenAI, body); ok {
			t.Fatal("a stream with no usage reported some")
		}
	})

	t.Run("sad/one_enormous_line_is_refused_and_the_stream_recovers", func(t *testing.T) {
		s, err := proxy.NewStream(proxy.OpenAI)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		if _, err := s.Write([]byte("data: " + strings.Repeat("x", proxy.MaxEventBytes))); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := s.Write([]byte("\n\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		// The stream is back in step: the next event is read normally.
		if _, err := s.Write([]byte(`data: {"usage":{"prompt_tokens":8,"completion_tokens":1}}` + "\n\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		u, ok := s.Usage()
		if !ok || u.Input != 8 {
			t.Fatalf("usage = %+v (%v), want the event after the refused one", u, ok)
		}
		if !errors.Is(s.Err(), proxy.ErrEventTooLong) {
			t.Fatalf("Err = %v, want %v", s.Err(), proxy.ErrEventTooLong)
		}
	})
}
