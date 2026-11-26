package proxy_test

import (
	"strings"
	"testing"

	"github.com/AutumnsGrove/verge/internal/proxy"
)

// FuzzStream is the proxy's highest-value target after the relay: the bytes are an
// upstream's, the ledger reads a copy of them while the client gets the originals, and the
// two must not disagree about where a record ends.
//
// Three properties over every input, for each of the three wire shapes:
//
//  1. **Chunking does not change the answer.** A body delivered one byte at a time parses
//     to the same usage as the same body delivered whole. This is the property a tee needs:
//     TCP decides the boundaries, not the parser.
//  2. **No usage is negative.** A negative token or cost count would lower a session's spend
//     and walk it through its cap. (TestUsageFromJSON pins the readable instance; this
//     checks it over arbitrary input.)
//  3. **Parsing never panics and never refuses bytes.** Write always returns len(p), so a
//     copy of a stream cannot fail a relay.
func FuzzStream(f *testing.F) {
	seeds := []string{
		// Anthropic, the shape usage actually takes in each event.
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":10}}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n",
		// OpenAI, usage in the final chunk and a [DONE] terminator.
		`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
			`data: {"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":12}}}` + "\n\n" +
			"data: [DONE]\n\n",
		// OpenRouter, with its own cost.
		`data: {"usage":{"prompt_tokens":30,"completion_tokens":5,"cost":0.00042}}` + "\n\n",
		// The hostile shapes the seeds should keep reaching for.
		`<!doctype html><html><body>502 Bad Gateway</body></html>`,
		"data: {\"usage\":{\"prompt_tokens\":-1,\"cost\":-9}}\n\n",
		"data: {\"usage\":{\"prompt_tokens\":\"30\"}}\n\n",
		"data: ", "\n", "\r\n\r\n", "", "data: [DONE]\n\n",
		"data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n" + strings.Repeat(": keep-alive\n\n", 3),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body string) {
		for _, protocol := range []proxy.Protocol{proxy.Anthropic, proxy.OpenAI, proxy.OpenRouter} {
			whole, wholeOK, wholeErr := proxy.ParseStream(protocol, strings.NewReader(body))

			s, err := proxy.NewStream(protocol)
			if err != nil {
				t.Fatalf("NewStream: %v", err)
			}
			for i := 0; i < len(body); i++ {
				n, werr := s.Write([]byte{body[i]})
				if werr != nil || n != 1 {
					t.Fatalf("Write = (%d, %v), want (1, nil)", n, werr)
				}
			}
			chunked, chunkedOK := s.Usage()

			if whole != chunked || wholeOK != chunkedOK {
				t.Fatalf("%s: one write = %+v (%v), byte at a time = %+v (%v)",
					protocol, whole, wholeOK, chunked, chunkedOK)
			}

			for _, u := range []proxy.Usage{whole, chunked} {
				if u.Input < 0 || u.Output < 0 || u.Cached < 0 || u.Cost < 0 {
					t.Fatalf("%s: negative usage %+v", protocol, u)
				}
			}

			// A one-shot body cannot be too long by this parser's definition unless a line
			// in it was, and the streaming path must agree about that too.
			if (wholeErr != nil) != (s.Err() != nil) {
				t.Fatalf("%s: ParseStream err = %v, streaming err = %v", protocol, wholeErr, s.Err())
			}
		}
	})
}
