package relay_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/AutumnsGrove/verge/internal/rpc/relay"
)

// FuzzRelay is the highest-value target in this repository (docs/testing.md) because the
// stream it reads is the thing most likely to eat a real session: pi's records are
// LF-framed, they may contain U+2028 and U+2029 inside JSON strings, and they may arrive
// split anywhere -- including inside a UTF-8 rune.
//
// The property is exact equivalence with the protocol's definition, which is small
// enough to state twice: split only on LF, strip one CR before each LF, and drop the
// empty piece that a trailing LF leaves.
func FuzzRelay(f *testing.F) {
	for _, seed := range [][]byte{
		{},
		[]byte("\n"),
		[]byte("a\nb\n"),
		[]byte("a\r\nb\r\n"),
		[]byte("a\rb\n"),                        // a CR that is content
		[]byte("{\"t\":\"x\u2028y\u2029z\"}\n"), // the separators Node's readline would split on
		[]byte("{\"t\":\"é\"}\n"),               // a two-byte rune
		[]byte("unterminated"),
		[]byte("\r\n\r\n"),
		[]byte(strings.Repeat("x", 64) + "\n"),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		want := reference(data)

		r := relay.NewReaderLimit(bytes.NewReader(data), relay.DefaultLimit)
		var got [][]byte
		for {
			rec, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("Next(%q) = %v", data, err)
			}
			if bytes.IndexByte(rec, '\n') >= 0 {
				t.Fatalf("Next(%q) returned a record containing an LF: %q", data, rec)
			}
			if len(rec) > relay.DefaultLimit {
				t.Fatalf("Next(%q) returned a %d-byte record", data, len(rec))
			}
			got = append(got, rec)
		}

		if len(got) != len(want) {
			t.Fatalf("%q gave %d records, want %d: got %q want %q", data, len(got), len(want), got, want)
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("%q record %d = %q, want %q", data, i, got[i], want[i])
			}
		}
	})
}

// reference is the protocol's framing, written the obvious way: it is the oracle the
// reader is compared against, and it is deliberately not the reader's own logic.
func reference(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}

	parts := bytes.Split(data, []byte("\n"))
	// A trailing LF leaves an empty piece that is framing rather than a record. A blank
	// line in the middle does not: it is an empty record, and the reader keeps it.
	if n := len(parts) - 1; n >= 0 && len(parts[n]) == 0 {
		parts = parts[:n]
	}
	for i := range parts {
		if n := len(parts[i]); n > 0 && parts[i][n-1] == '\r' {
			parts[i] = parts[i][:n-1]
		}
	}
	return parts
}

// TestOverTheLimitIsNotAShortRead is the one case the fuzzer cannot reach cheaply: a
// record over the limit must be refused rather than truncated, because a truncated record
// is a fragment of JSON someone downstream has to reject.
func TestOverTheLimitIsNotAShortRead(t *testing.T) {
	const limit = 8
	r := relay.NewReaderLimit(strings.NewReader(strings.Repeat("x", limit+1)+"\n"), limit)
	if _, err := r.Next(); !errors.Is(err, relay.ErrRecordTooLong) {
		t.Fatalf("Next = %v, want %v", err, relay.ErrRecordTooLong)
	}
}
