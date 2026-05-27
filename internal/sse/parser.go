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
	// ts_start. M2 leaves this nil; M5 wires it from the drainer's
	// per-chunk timestamps. See ARCHITECTURE § 3.3.
	TDeltaMs *int64 `json:"t_delta_ms,omitempty"`
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
	scanner := bufio.NewScanner(r)
	// SSE frames can be large (final OpenAI Responses event-named
	// frames carry full response objects with usage; some are 100+ KB).
	// 1 MB per-line is a comfortable ceiling.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		events      []Event
		streamDone  bool
		curEvent    string
		curDataBuf  strings.Builder
		hasData     bool
		sawAnyLine  bool
		parseErrMsg string
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
			// Reset accumulators; don't emit an Event.
			curEvent = ""
			curDataBuf.Reset()
			hasData = false
			return
		}

		ev := Event{Name: curEvent}
		if data != "" {
			// Parse data as JSON; if it doesn't parse, keep the raw
			// bytes so the consumer can still see them.
			if json.Valid([]byte(data)) {
				ev.Data = json.RawMessage(data)
			} else {
				// Wrap the raw text as a JSON string so the line stays
				// valid JSON; consumers can detect non-JSON by the
				// type of `data`.
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
	}

	for scanner.Scan() {
		sawAnyLine = true
		line := scanner.Bytes()

		// Blank line = frame boundary → emit accumulator.
		if len(line) == 0 {
			flush()
			continue
		}

		// Comment lines start with ':' per the SSE spec. Ignore.
		if line[0] == ':' {
			continue
		}

		field, value, ok := splitField(line)
		if !ok {
			// Not a recognizable SSE line. Skip but note it; if NO
			// SSE-shaped lines appear at all, we'll mark parseError.
			continue
		}

		switch field {
		case "event":
			curEvent = value
		case "data":
			if hasData {
				// Multiple `data:` lines per frame → join with newline
				// per SSE spec.
				curDataBuf.WriteByte('\n')
			}
			curDataBuf.WriteString(value)
			hasData = true
		case "id", "retry":
			// SSE-spec fields we don't preserve in v0 (replay
			// reconstructs frames from {event, data} only — see
			// ARCHITECTURE § 6.4 caveat). Silently consumed.
			continue
		default:
			// Unknown field; ignored per SSE spec.
			continue
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		parseErrMsg = "scan: " + err.Error()
	}

	// EOF without trailing blank line: flush pending accumulator
	// (ARCHITECTURE § 10.6 explicit behavior).
	if hasData || curEvent != "" {
		flush()
	}

	// If we read bytes but produced no events and no streamDone, the
	// body wasn't actually SSE — caller will fall back to body_b64.
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
