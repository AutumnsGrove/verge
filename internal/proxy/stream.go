package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// MaxEventBytes is the longest single SSE line, or event, this parser will hold.
//
// The proxy relays every byte regardless: this bounds only the copy that is being read
// for usage, because a body this process does not control is a body that can be enormous.
// Ten megabytes is past any event a model emits and well inside a control plane's memory.
const MaxEventBytes = 10 << 20

// ErrEventTooLong means one line or event of the stream exceeded MaxEventBytes.
//
// It is reported after the fact rather than thrown: the relay has already passed the bytes
// to the client, and a ledger that cannot read one event is a row with a warning, not a
// session that died.
var ErrEventTooLong = errors.New("proxy: an event exceeded the line limit")

// Stream reads one upstream response's usage as the relay copies it past.
//
// It is an io.Writer, not an io.Reader, for the reason the package doc gives: the proxy is
// the thing that ships the bytes to the client, and this is a copy of them. A stream this
// parser does not understand is still a stream the client received -- the parser's only two
// answers are "here is the usage" and "I could not read it".
//
// It is not safe for concurrent use: a response body has one reader, and the Usage call
// happens after the copy finishes.
type Stream struct {
	protocol Protocol

	line      []byte // the line being read
	data      []byte // the event being assembled from its data: lines
	skipping  bool   // the rest of the current line is refused
	dropEvent bool   // the current event is refused

	err   error // the first reason the stream could not be read in full
	usage Usage
	seen  bool
}

// NewStream returns a parser for one response.
func NewStream(protocol Protocol) (*Stream, error) {
	if !protocol.Valid() {
		return nil, fmt.Errorf("%w: %s", ErrConfig, protocol)
	}
	return &Stream{protocol: protocol}, nil
}

// Write consumes a chunk of the response body.
//
// It never fails: the bytes are the client's, and a parser that could refuse them would be
// a parser that could break a session. A line or event over the limit is recorded (Err())
// and discarded to the next boundary.
func (s *Stream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			break
		}
		s.appendLine(p[:i])
		p = p[i+1:]
		s.endLine()
	}
	return n, nil
}

// appendLine adds to the current line, unless the line is being skipped.
func (s *Stream) appendLine(b []byte) {
	if s.skipping {
		return
	}
	s.line = append(s.line, b...)
	if len(s.line) > MaxEventBytes {
		s.line = s.line[:0]
		s.skipping = true
		s.fail(ErrEventTooLong)
	}
}

// endLine handles one complete line, SSE rules only: a blank line ends the event, a
// colon-first line is a comment, data: lines are the payload, and every other field is
// framing the ledger does not need.
func (s *Stream) endLine() {
	line := s.line
	s.line = s.line[:0]

	if s.skipping {
		s.skipping = false
		return
	}

	// CRLF framing leaves a CR, which is not content.
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}

	switch {
	case len(line) == 0:
		s.dispatch()
	case line[0] == ':':
		// A comment, including the keep-alive ones a gateway sends.
	default:
		field, value := splitField(line)
		if field != "data" {
			return
		}
		if len(s.data) > 0 {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, value...)
		if len(s.data) > MaxEventBytes {
			s.data = nil
			s.dropEvent = true
			s.fail(ErrEventTooLong)
		}
	}
}

// dispatch reads the assembled event, if it is one.
func (s *Stream) dispatch() {
	data := s.data
	s.data = nil

	drop := s.dropEvent
	s.dropEvent = false
	if drop || len(data) == 0 {
		return
	}

	// "[DONE]" is OpenAI's terminator, not JSON. A non-JSON event is tolerated for the same
	// reason everything else here is: the client got it, and this parser is not the judge.
	u, err := StreamEvent(s.protocol, data)
	if err != nil {
		return
	}
	s.usage.merge(u)
	s.seen = true
}

// Usage returns what the stream said it cost, and whether it said anything at all.
//
// It dispatches an event that ended without its blank line: a connection cut between the
// last record and its terminator still produced a complete one that a ledger should not
// drop. A second call changes nothing.
func (s *Stream) Usage() (Usage, bool) {
	if len(s.line) > 0 && !s.skipping {
		s.endLine()
	}
	s.dispatch()
	return s.usage, s.seen
}

// Err reports a stream this parser could not read in full.
func (s *Stream) Err() error { return s.err }

func (s *Stream) fail(err error) {
	if s.err == nil {
		s.err = err
	}
}

// splitField splits one SSE line into its field and value, per the spec: the first colon
// separates them, and one leading space in the value is not content.
func splitField(line []byte) (string, []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return string(line), nil
	}
	value := line[i+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return string(line[:i]), value
}

// ParseStream reads a whole body and returns the usage it reported.
//
// It exists for callers that already hold the body -- a non-streamed response, a test, a
// recorded transcript -- and for the fuzz target, which needs the streaming path to agree
// with the one-shot one.
func ParseStream(protocol Protocol, r io.Reader) (Usage, bool, error) {
	s, err := NewStream(protocol)
	if err != nil {
		return Usage{}, false, err
	}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			_, _ = s.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Usage{}, false, rerr
		}
	}
	u, ok := s.Usage()
	return u, ok, s.Err()
}
