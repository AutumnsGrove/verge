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
	var (
		rec  []byte
		err  error
		done bool
	)

	for {
		var chunk []byte
		chunk, err = r.br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// No newline yet: this is a fragment of a long record.
			//
			// The check here is a memory bound rather than a behaviour one, and that is
			// worth saying because it cannot be tested: removing it changes nothing a
			// caller can observe, since the record is still refused at the end. What it
			// prevents is accumulating a line of any size in memory before the end is
			// reached, which on a stream from a machine is the difference between a
			// refused record and an exhausted control plane.
			rec = append(rec, chunk...)
			if len(rec) > r.limit {
				return nil, fmt.Errorf("%w: over %d bytes", ErrRecordTooLong, r.limit)
			}
			continue
		case errors.Is(err, io.EOF):
			rec = append(rec, chunk...)
			done = true
		default:
			rec = append(rec, chunk...)
		}

		if done {
			if len(rec) == 0 {
				return nil, io.EOF
			}
			if len(rec) > r.limit {
				return nil, fmt.Errorf("%w: over %d bytes", ErrRecordTooLong, r.limit)
			}
			return stripCR(rec), nil
		}

		// Strip the framing LF, then the CR that may precede it.
		rec = rec[:len(rec)-1]
		if len(rec) > r.limit {
			return nil, fmt.Errorf("%w: over %d bytes", ErrRecordTooLong, r.limit)
		}
		return stripCR(rec), nil
	}
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
