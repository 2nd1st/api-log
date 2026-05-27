// Package sse implements the SSE parser described in ARCHITECTURE § 10.6.
//
// The parser is dispatch-free in the wire-shape sense: a single line
// reader handles all three SSE shapes api-log records (OpenAI Chat
// data-only, OpenAI Responses event-named, Anthropic Messages event-named).
// Only the **terminator vocabulary** is shape-aware — the parser
// recognizes `data: [DONE]`, `event: response.completed`, and
// `event: message_stop` as clean-end markers and reports `stream_done`
// accordingly. It does NOT interpret the meaning of the events; that
// is the consumer's job.
//
// This parser is the FINALIZE-time parser. The capture-time drainer
// (internal/capture) is shape-aware at a different layer (raw `\n\n`
// frame boundaries for timing). See ARCHITECTURE § 10.6 for the
// two-layer distinction.
package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Event is one parsed SSE event. Data is captured as a raw JSON value
// when the `data:` line was parseable JSON, or left nil if not (the
// trace falls back to body_b64 in that case at the caller's discretion).
type Event struct {
	// Name is the `event:` field. Empty string for data-only frames
	// (OpenAI Chat Completions). The empty-string sentinel is documented
	// in ARCHITECTURE § 3 sentinels table.
	Name string `json:"event"`
	// Data is the raw JSON of the `data:` line. Captured verbatim so
	// downstream consumers see exactly what the upstream emitted.
	Data json.RawMessage `json:"data"`
	// TDeltaMs is the per-event arrival timing in ms from the trace's
	// ts_start. nil when the trace was reparse-reconstructed (sentinel
	// per ARCHITECTURE § 3) or when the upstream response was content-
	// encoded (drainer can't see frame boundaries through compression).
	TDeltaMs *int64 `json:"t_delta_ms,omitempty"`
	// Offset is the byte offset of this event's first field line in the
	// parsed input. Used at finalize to look up the chunk-arrival
	// timestamp; not serialized to JSONL.
	Offset int64 `json:"-"`
}

// ParseResult is the output of Parse over a complete SSE stream.
type ParseResult struct {
	Events     []Event
	StreamDone bool
	// ParseError is non-empty if the body could not be parsed as any
	// SSE shape (e.g. random bytes that don't look like `data:` /
	// `event:` lines). The caller falls back to body_b64 in that case.
	ParseError string
}

// Parse reads an SSE stream from r until EOF and returns the parsed
// events plus stream-done status.
//
// Each Event carries its byte Offset in the input so finalize can attach
// per-event t_delta_ms by looking up the chunk that contained the byte
// (capture.LookupChunkTime). The offset is the byte position of the
// event's FIRST non-blank, non-comment field line.
//
// The parser tolerates the three terminator shapes ARCHITECTURE § 10.6
// names:
//   - OpenAI Chat Completions: terminates on `data: [DONE]`.
//   - OpenAI Responses:        terminates on `event: response.completed`.
//   - Anthropic Messages:      terminates on `event: message_stop`.
//
// Stream EOF without seeing a terminator → StreamDone=false. Pending
// `event:` / `data:` accumulator at EOF → emit one final event (some
// servers don't terminate the last frame with a blank line).
func Parse(r io.Reader) ParseResult {
	// bufio.Reader lets us track byte offsets ourselves; bufio.Scanner
	// hides them. SSE frames can be 100+ KB on Responses final-event;
	// give the reader a generous buffer.
	br := bufio.NewReaderSize(r, 64*1024)

	var (
		events                  []Event
		streamDone              bool
		curEvent                string
		curDataBuf              strings.Builder
		hasData                 bool
		currentEventStartOffset int64 = -1
		offset                  int64
		sawAnyLine              bool
		parseErrMsg             string
	)

	flush := func() {
		if !hasData && curEvent == "" {
			return
		}
		data := curDataBuf.String()
		// Special sentinel: `data: [DONE]` is OpenAI Chat's terminator,
		// not a real JSON event payload.
		if curEvent == "" && data == "[DONE]" {
			streamDone = true
			curEvent = ""
			curDataBuf.Reset()
			hasData = false
			currentEventStartOffset = -1
			return
		}

		ev := Event{Name: curEvent, Offset: currentEventStartOffset}
		if data != "" {
			if json.Valid([]byte(data)) {
				ev.Data = json.RawMessage(data)
			} else {
				escaped, _ := json.Marshal(data)
				ev.Data = json.RawMessage(escaped)
			}
		}
		events = append(events, ev)

		// Terminator vocabulary recognition.
		if curEvent == "response.completed" || curEvent == "message_stop" {
			streamDone = true
		}

		curEvent = ""
		curDataBuf.Reset()
		hasData = false
		currentEventStartOffset = -1
	}

	for {
		lineStart := offset
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			sawAnyLine = true
			offset += int64(len(line))

			// Strip trailing \r\n or \n for processing.
			trimmed := line
			if n := len(trimmed); n > 0 && trimmed[n-1] == '\n' {
				trimmed = trimmed[:n-1]
			}
			if n := len(trimmed); n > 0 && trimmed[n-1] == '\r' {
				trimmed = trimmed[:n-1]
			}

			// Blank line = frame boundary.
			if len(trimmed) == 0 {
				flush()
			} else if trimmed[0] == ':' {
				// SSE comment; ignored, doesn't start an event.
			} else {
				field, value, ok := splitField(trimmed)
				if ok {
					// First field line of this event: capture its start offset.
					if currentEventStartOffset == -1 {
						currentEventStartOffset = lineStart
					}
					switch field {
					case "event":
						curEvent = value
					case "data":
						if hasData {
							curDataBuf.WriteByte('\n')
						}
						curDataBuf.WriteString(value)
						hasData = true
					case "id", "retry":
						// SSE-spec fields we don't preserve in v0; see
						// ARCHITECTURE § 6.4 caveat.
					default:
						// Unknown field; ignored per SSE spec.
					}
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				parseErrMsg = "read: " + err.Error()
			}
			break
		}
	}

	// EOF without trailing blank line: flush pending accumulator
	// (ARCHITECTURE § 10.6 explicit behavior).
	if hasData || curEvent != "" {
		flush()
	}

	if sawAnyLine && len(events) == 0 && !streamDone && parseErrMsg == "" {
		parseErrMsg = "no SSE frames found"
	}

	return ParseResult{
		Events:     events,
		StreamDone: streamDone,
		ParseError: parseErrMsg,
	}
}

// splitField parses one SSE line "field: value" → ("field", "value", true).
// Per the SSE spec, a single space after the colon is part of the
// separator and is stripped. Returns false for lines that don't contain
// a colon at all (not a recognizable field line).
func splitField(line []byte) (field, value string, ok bool) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		// Per SSE spec, a line with no colon is treated as a field
		// with empty value. v0 ignores these.
		return "", "", false
	}
	field = string(line[:i])
	v := line[i+1:]
	// Strip exactly one leading space if present.
	if len(v) > 0 && v[0] == ' ' {
		v = v[1:]
	}
	value = string(v)
	return field, value, true
}
