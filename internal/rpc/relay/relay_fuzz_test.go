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
	for _, seed := range []struct {
		data  []byte
		limit int
	}{
		{[]byte(""), 8},
		{[]byte("\n"), 8},
		{[]byte("a\nb\n"), 8},
		{[]byte("a\r\nb\r\n"), 8},
		{[]byte("a\rb\n"), 8},                         // a CR that is content
		{[]byte("{\"t\":\"x\u2028y\u2029z\"}\n"), 64}, // the separators Node's readline splits on
		{[]byte("{\"t\":\"é\"}\n"), 64},               // a two-byte rune
		{[]byte("unterminated"), 8},
		{[]byte("\r\n\r\n"), 8},
		{[]byte(strings.Repeat("x", 64) + "\n"), 16},   // over a small limit
		{[]byte(strings.Repeat("x", 64) + "\r\n"), 64}, // exactly the limit, with CRLF
	} {
		f.Add(seed.data, seed.limit)
	}

	// The limit is a parameter rather than DefaultLimit, because the limit's branches --
	// the accumulate bound, the resync, and the CRLF-at-the-limit rule -- are where the
	// interesting failures live, and a target that only ever sees ten megabytes never
	// reaches any of them.
	f.Fuzz(func(t *testing.T, data []byte, limit int) {
		if limit < 1 {
			limit = 1
		}
		if limit > 256 {
			limit = 256
		}

		want := reference(data)
		r := relay.NewReaderLimit(bytes.NewReader(data), limit)

		var got [][]byte
		for {
			rec, err := r.Next()
			switch {
			case errors.Is(err, io.EOF):
				if len(want) != 0 {
					t.Fatalf("%q with limit %d: ended with %d records left over: %q", data, limit, len(want), want)
				}
				if len(got) == 0 && len(data) > 0 && len(reference(data)) == 0 {
					t.Fatalf("%q with limit %d: ended without a record", data, limit)
				}
				return

			case errors.Is(err, relay.ErrRecordTooLong), errors.Is(err, relay.ErrTruncatedRecord):
				// A refusal is only correct for the record that did not fit: the next one
				// in the reference is the first over the limit.
				if len(want) == 0 || len(want[0]) <= limit {
					t.Fatalf("%q with limit %d: refused a record that fits (%q): %v", data, limit, want, err)
				}
				return

			case err != nil:
				t.Fatalf("Next(%q) = %v", data, err)
			}

			if bytes.IndexByte(rec, '\n') >= 0 {
				t.Fatalf("Next(%q) returned a record containing an LF: %q", data, rec)
			}
			if len(rec) > limit {
				t.Fatalf("Next(%q) with limit %d returned a %d-byte record", data, limit, len(rec))
			}
			if len(want) == 0 || !bytes.Equal(rec, want[0]) {
				t.Fatalf("%q record %d = %q, want %q", data, len(got), rec, want)
			}
			want = want[1:]
			got = append(got, rec)
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
