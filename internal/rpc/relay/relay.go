// Package relay frames pi's RPC stream: JSON records separated by LF.
//
// It exists because pi's protocol has one framing rule and several ways to get it
// wrong, and every one of them loses a session. From pi's own `docs/rpc.md`:
//
//   - Records are split **only** on `\n`. A generic line reader that also splits on
//     U+2028 or U+2029 — Node's `readline` does — cuts records that are valid JSON,
//     because both characters may appear inside a JSON string.
//   - A `\r` immediately before the `\n` is stripped, so CRLF input is accepted.
//   - Records are bytes, not text. Half a UTF-8 rune arrives across a read boundary and
//     is the caller's business once the record is complete; a reader that decoded would
//     have to decide what to do with the half, and every answer is worse.
//   - Reads are bounded. A reader that grows with its input is a reader that one
//     enormous line can exhaust.
//   - stdout is records and nothing else, and pi honours backpressure, so writes are one
//     record per Write and a short write is an error rather than a retry loop here.
//
// Nothing in this package parses JSON. A relay that validated would have to decide what
// to do with a record it did not understand, and the only correct answer is to forward
// it: the far side is the parser that owns the schema.
package relay

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// DefaultLimit is the longest record this package will read or write.
//
// It is a limit on one record, not on the stream: a session is an unbounded number of
// small records and must not be cut off. Ten megabytes is far past any record pi emits
// and well inside a control plane's memory; a record over it is a machine to look at.
const DefaultLimit = 10 << 20

// ErrRecordTooLong means a record exceeded the reader's limit.
var ErrRecordTooLong = errors.New("relay: record exceeds the limit")

// ErrRecordHasLF means a record being written contains an LF, which would frame it as
// two records and leave the far side with a fragment of JSON to reject.
var ErrRecordHasLF = errors.New("relay: a record cannot contain an LF")

// ErrRecordEndsWithCR means a record being written ends with a CR. Writing it would
// produce CRLF, and the reader would strip that CR as framing, so the record would come
// back one byte shorter -- a silent corruption at the one place the encoding is meant to
// be reversible.
var ErrRecordEndsWithCR = errors.New("relay: a record cannot end with a CR")

// Reader reads LF-framed records.
//
// It is not safe for concurrent use; a stream has one reader, and a relay with two
// directions has two Readers.
type Reader struct {
	br    *bufio.Reader
	limit int
}

// NewReader returns a Reader with DefaultLimit.
func NewReader(r io.Reader) *Reader { return NewReaderLimit(r, DefaultLimit) }

// NewReaderLimit returns a Reader that refuses records longer than limit bytes.
//
// The buffer is deliberately not sized to the limit: bufio grows as records arrive, so a
// stream of small records costs a small buffer and only a genuinely long record is
// accumulated.
func NewReaderLimit(r io.Reader, limit int) *Reader {
	if limit <= 0 {
		limit = DefaultLimit
	}
	return &Reader{br: bufio.NewReader(r), limit: limit}
}

// Next returns the next record, without its LF.
//
// An unterminated final record is returned and the next call reports io.EOF: pi always
// terminates its records, and a process that exited between the record and its newline
// still produced a complete one that a relay must not drop.
//
// A blank line is an empty record, returned as such. The reader does not decide what a
// record means; a caller that rejects empty records can, without this package having
// invented a rule the protocol does not have.
func (r *Reader) Next() ([]byte, error) {
	var rec []byte

	for {
		chunk, err := r.br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			rec = append(rec, chunk...)
			// The bound on the accumulating buffer is one byte past the limit, because
			// the byte before the newline may be the CR that framing will strip: a
			// record whose content is exactly the limit arrives one byte over.
			//
			// This check bounds memory, and that is all it does -- the same input is
			// refused by finish below. It exists so a single enormous line is refused
			// before it is accumulated, and a mutation that removes it is invisible to
			// any test by construction.
			if len(rec) > r.limit+1 {
				return nil, fmt.Errorf("%w: over %d bytes", ErrRecordTooLong, r.limit)
			}
			continue

		case err == nil:
			// The chunk ends with the framing LF, which is not content.
			rec = append(rec, chunk[:len(chunk)-1]...)
			return r.finish(rec)

		case errors.Is(err, io.EOF):
			rec = append(rec, chunk...)
			if len(rec) == 0 {
				return nil, io.EOF
			}
			return r.finish(rec)

		default:
			// A read error is not a shorter record. Forwarding the fragment would hand
			// the far side a piece of JSON to reject, which is the failure this package
			// exists to prevent -- and it is how a broken pipe or a dropped connection
			// would end a session.
			return nil, err
		}
	}
}

// finish strips the framing CR and enforces the limit on the content.
//
// The limit is checked after the CR is stripped, so a record of exactly limit bytes
// arrives accepted whether it was sent with LF or CRLF: the limit is on the record, and
// the CR is framing.
func (r *Reader) finish(rec []byte) ([]byte, error) {
	rec = stripCR(rec)
	if len(rec) > r.limit {
		return nil, fmt.Errorf("%w: over %d bytes", ErrRecordTooLong, r.limit)
	}
	return rec, nil
}

// stripCR removes the carriage return that CRLF framing leaves behind.
func stripCR(rec []byte) []byte {
	if n := len(rec); n > 0 && rec[n-1] == '\r' {
		return rec[:n-1]
	}
	return rec
}

// Writer writes LF-framed records.
//
// One record is one Write: pi reads records from stdout as they arrive, and a writer
// that split a record across several writes would be inviting a reader to act on half of
// it. It is safe for use by one goroutine; a relay that writes from several needs a lock
// of its own, because interleaving is the caller's to prevent rather than this type's to
// guess at.
type Writer struct {
	w io.Writer
}

// NewWriter returns a Writer over w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write sends one record, framed by a single LF.
func (w *Writer) Write(rec []byte) error {
	if len(rec) > DefaultLimit {
		return fmt.Errorf("%w: over %d bytes", ErrRecordTooLong, DefaultLimit)
	}
	for _, b := range rec {
		if b == '\n' {
			return ErrRecordHasLF
		}
	}
	if n := len(rec); n > 0 && rec[n-1] == '\r' {
		return ErrRecordEndsWithCR
	}

	// One buffer, one Write: see the type comment.
	buf := make([]byte, len(rec)+1)
	copy(buf, rec)
	buf[len(rec)] = '\n'

	n, err := w.w.Write(buf)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return fmt.Errorf("relay: short write: %d of %d bytes", n, len(buf))
	}
	return nil
}
