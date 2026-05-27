package trace

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/leoyun/api-log/internal/sse"
)

func TestMarshalShape(t *testing.T) {
	tr := Trace{
		ID:      "01HX7K8MSABCDEF",
		TsStart: time.Date(2026, 5, 27, 10, 23, 45, 123_000_000, time.UTC),
		TsEnd:   time.Date(2026, 5, 27, 10, 23, 46, 357_000_000, time.UTC),
		Client:  "172.17.0.5:54321",
		Method:  "POST",
		Path:    "/v1/messages",
		Upstream: "http://gateway:7860",
		Status:  200,
		Req: Body{
			Headers: Headers{
				"Authorization": {"Bearer sk-..."},
				"Content-Type":  {"application/json"},
			},
			Body: json.RawMessage(`{"model":"x","messages":[]}`),
		},
		Resp: Body{
			Headers: Headers{
				"Content-Type": {"application/json"},
			},
			Body: json.RawMessage(`{"id":"msg_1"}`),
		},
	}
	out, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	// Should be single-line.
	if strings.Contains(s, "\n") {
		t.Errorf("marshaled trace contains newline: %s", s)
	}

	// All top-level keys present.
	for _, k := range []string{"id", "ts_start", "ts_end", "client", "method", "path",
		"upstream", "status", "req", "resp", "disconnected", "truncated_req", "truncated_resp"} {
		if !strings.Contains(s, `"`+k+`"`) {
			t.Errorf("missing key %q in marshaled output", k)
		}
	}

	// Headers should be flat strings.
	if !strings.Contains(s, `"Authorization":"Bearer sk-..."`) {
		t.Errorf("headers not flat: %s", s)
	}
}

func TestMarshalOmitsEmptyOptionalFields(t *testing.T) {
	tr := Trace{
		ID:     "01H",
		Status: 200,
		Req: Body{
			Headers: Headers{"X-Y": {"z"}},
			Body:    json.RawMessage(`null`),
		},
		Resp: Body{
			Headers: Headers{"X-Y": {"z"}},
			Body:    json.RawMessage(`null`),
		},
	}
	out, _ := json.Marshal(tr)
	s := string(out)

	if strings.Contains(s, "events") {
		t.Errorf("events should be omitted: %s", s)
	}
	if strings.Contains(s, "body_b64") {
		t.Errorf("body_b64 should be omitted: %s", s)
	}
	if strings.Contains(s, "parse_error") {
		t.Errorf("parse_error should be omitted: %s", s)
	}
	if strings.Contains(s, "stream_done") {
		t.Errorf("stream_done should be omitted when no events: %s", s)
	}
}

func TestMarshalStreamingResponse(t *testing.T) {
	streamDone := true
	tr := Trace{
		ID: "01H",
		Resp: Body{
			Headers: Headers{"Content-Type": {"text/event-stream"}},
			Events: []sse.Event{
				{Name: "message_start", Data: json.RawMessage(`{"id":"msg_1"}`)},
				{Name: "message_stop", Data: json.RawMessage(`{}`)},
			},
			StreamDone: &streamDone,
		},
	}
	out, _ := json.Marshal(tr)
	s := string(out)
	if !strings.Contains(s, `"events":[`) {
		t.Errorf("events array missing: %s", s)
	}
	if !strings.Contains(s, `"stream_done":true`) {
		t.Errorf("stream_done not emitted: %s", s)
	}
	if strings.Contains(s, `"body":`) && !strings.Contains(s, `"body":null`) {
		// Note: req.body and resp.body are both omitempty json.RawMessage.
		// nil RawMessage omits cleanly.
		t.Errorf("body should be omitted alongside events: %s", s)
	}
}

func TestHeadersMultiValueJoin(t *testing.T) {
	h := Headers{
		"X-Multi": {"a", "b", "c"},
	}
	out, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"X-Multi":"a, b, c"}` {
		t.Errorf("multi-value join = %s", string(out))
	}
}

func TestHeadersRoundTripFlatForm(t *testing.T) {
	original := Headers(http.Header{"Authorization": {"Bearer x"}, "Content-Type": {"application/json"}})
	out, _ := json.Marshal(original)

	var got Headers
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Authorization() != "Bearer x" {
		t.Errorf("Authorization round-trip = %q", got.Authorization())
	}
}

func TestHeadersUnmarshalArrayForm(t *testing.T) {
	in := []byte(`{"Authorization":["Bearer x"],"Content-Type":["application/json"]}`)
	var h Headers
	if err := json.Unmarshal(in, &h); err != nil {
		t.Fatal(err)
	}
	if h.Authorization() != "Bearer x" {
		t.Errorf("array form unmarshal got %q", h.Authorization())
	}
}

// helper for tests
func (h Headers) Authorization() string {
	v, ok := h["Authorization"]
	if !ok || len(v) == 0 {
		return ""
	}
	return v[0]
}
