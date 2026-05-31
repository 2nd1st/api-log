// Package trace is the JSONL line shape described in ARCHITECTURE § 3.
//
// One Trace marshals to exactly one line in `data/<date>/<key_hash>.jsonl`.
// The struct is purely the schema; building it from raw capture artifacts
// happens in internal/parser, persisting it in internal/writer.
package trace

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/2nd1st/api-log/internal/sse"
)

// Trace is one completed HTTP transaction. JSON tags here are the on-disk
// contract; ARCHITECTURE § 3 is the source of truth.
type Trace struct {
	ID            string    `json:"id"`
	TsStart       time.Time `json:"ts_start"`
	TsEnd         time.Time `json:"ts_end"`
	Client        string    `json:"client"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Upstream      string    `json:"upstream"`
	Status        int       `json:"status"`
	Req           Body      `json:"req"`
	Resp          Body      `json:"resp"`
	Disconnected  bool      `json:"disconnected"`
	TruncatedReq  bool      `json:"truncated_req"`
	TruncatedResp bool      `json:"truncated_resp"`

	// PluginIntercepted is the marker for traces whose request was
	// short-circuited by a v2 hook plugin (plugin-b-c-spec §5.2). Absent
	// on the common path; set only when a BEFORE or AFTER hook returned
	// ActionIntercept. Without this field, intercepted traces would be
	// indistinguishable from genuine upstream responses.
	//
	// omitempty + pointer keeps the JSONL line shape stable: the field
	// is *absent* (not "null") on every trace the plugin chain did not
	// intercept, matching the rest of the trace schema's
	// presence-or-absent convention.
	PluginIntercepted *PluginInterceptMarker `json:"plugin_intercepted,omitempty"`
}

// PluginInterceptMarker identifies the plugin instance that produced an
// intercept response. The shape is frozen by plugin-b-c-spec §5.2.
type PluginInterceptMarker struct {
	Type string `json:"type"` // plugin type name, e.g. "rate_limit_ip"
	ID   string `json:"id"`   // operator-chosen instance id
	Hook string `json:"hook"` // "before" | "after"
}

// Body is one side of the trace. Exactly one of Body/Events/BodyB64 is set:
//
//   - Body       : parsed JSON object (Content-Type application/json)
//   - Events     : parsed SSE events (Content-Type text/event-stream)
//   - BodyB64    : base64 of raw bytes (binary / non-JSON / JSON parse failed)
//
// StreamDone is set only when Events is non-empty.
// ParseError is set only when BodyB64 is set due to a parse failure
// (not when BodyB64 is set because the content type was non-JSON).
type Body struct {
	Headers    Headers         `json:"headers"`
	Body       json.RawMessage `json:"body,omitempty"`
	Events     []sse.Event     `json:"events,omitempty"`
	StreamDone *bool           `json:"stream_done,omitempty"`
	BodyB64    string          `json:"body_b64,omitempty"`
	ParseError string          `json:"parse_error,omitempty"`
}

// Headers is http.Header with a v0 marshal that emits each header as a
// single flat string value (multi-value headers joined with ", " per
// RFC 7230 § 3.2.2). LLM gateway traffic is overwhelmingly single-valued
// per header; the flat form is friendlier for jq.
type Headers http.Header

// MarshalJSON emits headers as a flat object of string→string. Header
// names are preserved in whatever case http.Header has canonicalized them
// to on receipt (typically canonical case, e.g. "Content-Type").
func (h Headers) MarshalJSON() ([]byte, error) {
	if h == nil {
		return []byte("{}"), nil
	}
	flat := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) == 0 {
			continue
		}
		if len(vs) == 1 {
			flat[k] = vs[0]
			continue
		}
		flat[k] = strings.Join(vs, ", ")
	}
	return json.Marshal(flat)
}

// UnmarshalJSON accepts both the flat-string form (produced by MarshalJSON)
// and the array form (Go's http.Header default), so JSONL files written
// by any version of api-log round-trip cleanly.
func (h *Headers) UnmarshalJSON(data []byte) error {
	// Try array form first (string → []string), since that's what Go's
	// json package would produce for a plain http.Header.
	var asArr map[string][]string
	if err := json.Unmarshal(data, &asArr); err == nil {
		*h = Headers(asArr)
		return nil
	}
	// Fall back to flat form.
	var asFlat map[string]string
	if err := json.Unmarshal(data, &asFlat); err != nil {
		return err
	}
	out := make(Headers, len(asFlat))
	for k, v := range asFlat {
		out[k] = []string{v}
	}
	*h = out
	return nil
}
