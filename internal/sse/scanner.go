package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Scanner is the incremental counterpart to Parse. It reads SSE frames
// one at a time from an io.Reader; callers drive a per-event loop via
// Next() and react to io.EOF in the usual way.
//
// The shape recognized here mirrors Parse exactly — same field syntax,
// same terminator vocabulary, same offset bookkeeping. The split exists
// so the AFTER-hook stream dispatcher can transform-and-forward events
// as they arrive rather than buffering the whole response.
//
// One Scanner is single-goroutine-safe only. Wrap the upstream
// io.ReadCloser in a sync layer if multiple goroutines need to consume.
type Scanner struct {
	br *bufio.Reader

	// offset bookkeeping mirrors Parse.
	offset     int64
	eventStart int64
	curEvent   string
	curDataBuf bytes.Buffer
	hasData    bool

	pending Event
	hasPend bool

	// streamDone latches once a terminator vocabulary item fires so the
	// caller can ask whether the upstream ended cleanly.
	streamDone bool
}

// NewScanner returns a Scanner over r. Buffer size matches Parse's 64
// KiB so single-event payloads (Anthropic message_stop final text,
// OpenAI Responses output_text final delta) fit on the bufio path
// without expansion.
func NewScanner(r io.Reader) *Scanner {
	return &Scanner{
		br:         bufio.NewReaderSize(r, 64*1024),
		eventStart: -1,
	}
}

// Next reads frames until one full event is buffered, then returns it.
// Returns io.EOF when the upstream is fully drained AND no pending
// event remains (the SSE spec allows the final frame to lack a trailing
// blank line; Next flushes that case on EOF).
//
// The returned Event mirrors Parse's Event: Name + Data + Offset. Data
// is verbatim raw JSON when the data line parsed as JSON, otherwise
// JSON-string-escaped so the resulting Data is always valid JSON. This
// is the same contract Parse upholds; the AFTER dispatcher's classifier
// expects it.
func (s *Scanner) Next() (Event, error) {
	if s.hasPend {
		ev := s.pending
		s.pending = Event{}
		s.hasPend = false
		return ev, nil
	}
	for {
		lineStart := s.offset
		line, err := s.br.ReadBytes('\n')
		if len(line) > 0 {
			s.offset += int64(len(line))
			trimmed := line
			if n := len(trimmed); n > 0 && trimmed[n-1] == '\n' {
				trimmed = trimmed[:n-1]
			}
			if n := len(trimmed); n > 0 && trimmed[n-1] == '\r' {
				trimmed = trimmed[:n-1]
			}
			if len(trimmed) == 0 {
				if ev, ok := s.flushFrame(); ok {
					return ev, nil
				}
				continue
			}
			if trimmed[0] == ':' {
				continue
			}
			field, value, ok := splitField(trimmed)
			if !ok {
				continue
			}
			if s.eventStart == -1 {
				s.eventStart = lineStart
			}
			switch field {
			case "event":
				s.curEvent = value
			case "data":
				if s.hasData {
					s.curDataBuf.WriteByte('\n')
				}
				s.curDataBuf.WriteString(value)
				s.hasData = true
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return Event{}, fmt.Errorf("sse scanner read: %w", err)
			}
			// EOF: flush trailing accumulator if any.
			if s.hasData || s.curEvent != "" {
				if ev, ok := s.flushFrame(); ok {
					// Keep io.EOF for the NEXT call so the caller sees
					// this last event first, then EOF.
					s.pending = ev
					s.hasPend = true
					ev2 := s.pending
					s.pending = Event{}
					s.hasPend = false
					return ev2, nil
				}
			}
			return Event{}, io.EOF
		}
	}
}

// StreamDone reports whether a terminator vocabulary item (data:[DONE],
// event:response.completed, event:message_stop) has been observed.
// Mirrors Parse.ParseResult.StreamDone.
func (s *Scanner) StreamDone() bool { return s.streamDone }

// flushFrame turns the field accumulator into one Event when the frame
// has either a name or a data payload. Returns ok=false when the
// accumulator was empty (consecutive blank lines).
func (s *Scanner) flushFrame() (Event, bool) {
	if !s.hasData && s.curEvent == "" {
		return Event{}, false
	}
	data := s.curDataBuf.String()
	// data:[DONE] sentinel — eat it; do not surface as an Event.
	if s.curEvent == "" && data == "[DONE]" {
		s.streamDone = true
		s.resetFrame()
		return Event{}, false
	}
	ev := Event{Name: s.curEvent, Offset: s.eventStart}
	if data != "" {
		if json.Valid([]byte(data)) {
			ev.Data = json.RawMessage(data)
		} else {
			escaped, _ := json.Marshal(data)
			ev.Data = json.RawMessage(escaped)
		}
	}
	if s.curEvent == "response.completed" || s.curEvent == "message_stop" {
		s.streamDone = true
	}
	s.resetFrame()
	return ev, true
}

func (s *Scanner) resetFrame() {
	s.curEvent = ""
	s.curDataBuf.Reset()
	s.hasData = false
	s.eventStart = -1
}

// WriteEvent serializes one Event back to wire bytes — the inverse of
// Parse / Scanner. Used by the AFTER-hook stream dispatcher to re-emit
// events (mutated or otherwise) to the client.
//
// Wire shape per RFC: optional `event: NAME\n`, then `data: ...\n` for
// every line of the JSON payload, then a single blank line.
//
// One Event = one frame. Multi-line data payloads (rare; we round-trip
// json.RawMessage which is always single-line) split on `\n`.
func WriteEvent(w io.Writer, ev Event) error {
	bw, isBuf := w.(*bufio.Writer)
	if !isBuf {
		bw = bufio.NewWriter(w)
	}
	if ev.Name != "" {
		if _, err := bw.WriteString("event: "); err != nil {
			return err
		}
		if _, err := bw.WriteString(ev.Name); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	if len(ev.Data) > 0 {
		// Split on \n so multi-line data payloads round-trip per SSE spec.
		for _, line := range bytes.Split(ev.Data, []byte{'\n'}) {
			if _, err := bw.WriteString("data: "); err != nil {
				return err
			}
			if _, err := bw.Write(line); err != nil {
				return err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return err
			}
		}
	}
	if err := bw.WriteByte('\n'); err != nil {
		return err
	}
	if !isBuf {
		return bw.Flush()
	}
	return nil
}
