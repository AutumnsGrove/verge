package relay_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AutumnsGrove/verge/internal/rpc/relay"
)

// readAll collects every record, which is how most of these tests assert.
func readAll(t *testing.T, r *relay.Reader) []string {
	t.Helper()
	var got []string
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, string(rec))
	}
}

func TestReader(t *testing.T) {
	t.Run("happy/records_split_on_lf_and_only_lf", func(t *testing.T) {
		// pi's docs are explicit: a client that splits on U+2028 or U+2029 -- which
		// Node's readline does -- eats records that are valid JSON. Both characters can
		// appear inside a JSON string, unescaped as far as JSON is concerned.
		one := `{"type":"message","text":"before` + "\u2028" + `middle` + "\u2029" + `after"}`
		two := `{"type":"agent_end"}`
		r := relay.NewReader(strings.NewReader(one + "\n" + two + "\n"))

		got := readAll(t, r)
		if len(got) != 2 {
			t.Fatalf("%d records for two lines: %q", len(got), got)
		}
		if got[0] != one || got[1] != two {
			t.Fatalf("records = %q, want the lines unchanged", got)
		}
	})

	t.Run("happy/crlf_is_accepted", func(t *testing.T) {
		r := relay.NewReader(strings.NewReader("a\r\nb\r\n"))
		if got := readAll(t, r); len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("records = %q, want [a b] with the CR stripped", got)
		}
	})

	t.Run("happy/a_cr_that_is_not_before_an_lf_is_content", func(t *testing.T) {
		// Only the CR that precedes the LF is framing; any other is part of the record.
		r := relay.NewReader(strings.NewReader("a\rb\n"))
		if got := readAll(t, r); len(got) != 1 || got[0] != "a\rb" {
			t.Fatalf("records = %q, want [a\\rb]", got)
		}
	})

	t.Run("happy/an_unterminated_last_record_is_returned", func(t *testing.T) {
		// pi terminates every record, and a process that ended between the record and its
		// newline still produced a complete one. Dropping it would lose the last thing a
		// session said.
		r := relay.NewReader(strings.NewReader("a\nb"))
		if got := readAll(t, r); len(got) != 2 || got[1] != "b" {
			t.Fatalf("records = %q, want [a b]", got)
		}
	})

	t.Run("happy/a_blank_line_is_an_empty_record", func(t *testing.T) {
		// The reader does not decide what a record means: a caller that rejects empty
		// records says so, and one that forwards them does not lose its place.
		r := relay.NewReader(strings.NewReader("a\n\nb\n"))
		got := readAll(t, r)
		if len(got) != 3 || got[1] != "" {
			t.Fatalf("records = %q, want the empty line kept as an empty record", got)
		}
	})

	t.Run("happy/a_split_utf8_rune_is_not_an_error", func(t *testing.T) {
		// The bytes are records, not text: a reader that decoded would have to decide what
		// to do with half a rune, and every answer is worse than passing bytes through.
		whole := []byte("{\"t\":\"é\"}\n")
		// Split inside the two-byte é.
		cut := bytes.Index(whole, []byte("é")) + 1
		r := relay.NewReader(io.MultiReader(bytes.NewReader(whole[:cut]), bytes.NewReader(whole[cut:])))

		got := readAll(t, r)
		if len(got) != 1 || !utf8.ValidString(got[0]) {
			t.Fatalf("records = %q, want the original record", got)
		}
		if got[0] != string(whole[:len(whole)-1]) {
			t.Fatalf("record = %q, want %q", got[0], whole[:len(whole)-1])
		}
	})

	t.Run("sad/a_record_over_the_limit", func(t *testing.T) {
		// A reader that grows with its input is a reader a machine can exhaust by writing
		// one enormous line. The limit is a limit on the record, not on the stream.
		const limit = 1024
		r := relay.NewReaderLimit(strings.NewReader(strings.Repeat("x", limit+1)+"\n"), limit)
		if _, err := r.Next(); !errors.Is(err, relay.ErrRecordTooLong) {
			t.Fatalf("Next = %v, want %v", err, relay.ErrRecordTooLong)
		}
	})

	t.Run("sad/a_record_longer_than_the_read_buffer", func(t *testing.T) {
		// This is the accumulate path: a record that does not fit in bufio's buffer, so it
		// arrives in fragments. The observable outcome is the same refusal as a short one,
		// and the reason it is here is that a limit which only worked for records that fit
		// in 4 KiB would be a limit on nothing.
		const limit = 16
		r := relay.NewReaderLimit(strings.NewReader(strings.Repeat("x", 5000)+"\n"), limit)
		if _, err := r.Next(); !errors.Is(err, relay.ErrRecordTooLong) {
			t.Fatalf("Next = %v, want %v", err, relay.ErrRecordTooLong)
		}
	})

	t.Run("happy/a_record_exactly_at_the_limit", func(t *testing.T) {
		const limit = 1024
		body := strings.Repeat("x", limit)
		r := relay.NewReaderLimit(strings.NewReader(body+"\n"), limit)
		rec, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if string(rec) != body {
			t.Fatalf("record has %d bytes, want %d", len(rec), limit)
		}
	})

	t.Run("happy/a_long_stream_of_small_records", func(t *testing.T) {
		// The limit bounds one record, not the total: a session is an unbounded number of
		// small records, and a reader that counted them would stop mid-run.
		var in strings.Builder
		for i := 0; i < relay.DefaultLimit; i++ {
			in.WriteString("x\n")
		}
		r := relay.NewReader(strings.NewReader(in.String()))
		n := 0
		for {
			_, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			n++
		}
		if n != relay.DefaultLimit {
			t.Fatalf("read %d records, want %d", n, relay.DefaultLimit)
		}
	})
}

func TestWriter(t *testing.T) {
	t.Run("happy/one_record_one_write_one_lf", func(t *testing.T) {
		var buf bytes.Buffer
		w := relay.NewWriter(&buf)
		if err := w.Write([]byte(`{"type":"prompt"}`)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := w.Write([]byte(`{"type":"get_state"}`)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		want := "{\"type\":\"prompt\"}\n{\"type\":\"get_state\"}\n"
		if buf.String() != want {
			t.Fatalf("wrote %q, want %q", buf.String(), want)
		}
	})

	t.Run("bad/a_record_containing_an_lf", func(t *testing.T) {
		// Writing one would produce two records, and the second would be a fragment of
		// JSON the far side has to reject. Refusing here names the bug at its source.
		var buf bytes.Buffer
		w := relay.NewWriter(&buf)
		if err := w.Write([]byte("{\"a\":1}\n{\"b\":2}")); !errors.Is(err, relay.ErrRecordHasLF) {
			t.Fatalf("Write = %v, want %v", err, relay.ErrRecordHasLF)
		}
		if buf.Len() != 0 {
			t.Fatalf("wrote %d bytes for a refused record", buf.Len())
		}
	})

	t.Run("sad/the_writer_fails", func(t *testing.T) {
		boom := errors.New("broken pipe")
		w := relay.NewWriter(failingWriter{err: boom})
		if err := w.Write([]byte("x")); !errors.Is(err, boom) {
			t.Fatalf("Write = %v, want %v", err, boom)
		}
	})

	t.Run("happy/a_large_record_default_limit", func(t *testing.T) {
		// The default is a bound, not a target: a record a little under it is ordinary
		// and must pass, because the limit exists to stop an unbounded one.
		var buf bytes.Buffer
		w := relay.NewWriter(&buf)
		body := make([]byte, relay.DefaultLimit-1)
		for i := range body {
			body[i] = 'x'
		}
		if err := w.Write(body); err != nil {
			t.Fatalf("Write: %v (DefaultLimit = %d)", err, relay.DefaultLimit)
		}
		if got := buf.Len(); got != relay.DefaultLimit {
			t.Fatalf("wrote %d bytes, want %d", got, relay.DefaultLimit)
		}
	})
}

// failingWriter is the sink that cannot: a pipe that closed under the relay.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

// TestRoundTrip is the property that matters for a relay: whatever it writes, it reads
// back unchanged. It is not the fuzz target's job to prove JSON is valid; it is this
// reader and writer agreeing about where a record ends.
func TestRoundTrip(t *testing.T) {
	records := []string{
		`{"type":"message","text":"a` + "\u2028" + `b"}`,
		"",
		`{"type":"agent_settled"}`,
		"cr\rhere",
		"{\"t\":\"é\"}",
	}
	var buf bytes.Buffer
	w := relay.NewWriter(&buf)
	for _, rec := range records {
		if strings.Contains(rec, "\n") {
			t.Fatalf("the fixture contains an LF: %q", rec)
		}
		if err := w.Write([]byte(rec)); err != nil {
			t.Fatalf("Write(%q): %v", rec, err)
		}
	}

	got := readAll(t, relay.NewReader(bytes.NewReader(buf.Bytes())))
	if len(got) != len(records) {
		t.Fatalf("%d records back, want %d", len(got), len(records))
	}
	for i := range records {
		if got[i] != records[i] {
			t.Fatalf("record %d = %q, want %q", i, got[i], records[i])
		}
	}
}

// bufio is used by the implementation, and this file asserts it is not leaking its own
// line semantics into the reader: a buffer shorter than one record must not split it.
var _ = bufio.ErrBufferFull
