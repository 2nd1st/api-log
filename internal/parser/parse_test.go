package parser

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"testing"

	"github.com/leoyun/api-log/internal/trace"
)

func TestParseRequestJSONBody(t *testing.T) {
	body := strings.NewReader(`{"model":"claude","messages":[]}`)
	h := http.Header{"Content-Type": {"application/json"}}
	got, err := ParseRequest(body, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.BodyB64 != "" {
		t.Errorf("BodyB64 should be empty for parseable JSON")
	}
	if got.ParseError != "" {
		t.Errorf("ParseError should be empty: %q", got.ParseError)
	}
	if string(got.Body) != `{"model":"claude","messages":[]}` {
		t.Errorf("Body = %s", got.Body)
	}
}

func TestParseRequestInvalidJSONFallsBackToB64(t *testing.T) {
	body := strings.NewReader(`{not valid json}`)
	h := http.Header{"Content-Type": {"application/json"}}
	got, _ := ParseRequest(body, h)
	if got.BodyB64 == "" {
		t.Errorf("BodyB64 should be set for invalid JSON")
	}
	if got.ParseError == "" {
		t.Errorf("ParseError should be set for invalid JSON")
	}
}

func TestParseRequestNonJSONContentType(t *testing.T) {
	body := strings.NewReader("multipart-boundary-content")
	h := http.Header{"Content-Type": {"multipart/form-data; boundary=xyz"}}
	got, _ := ParseRequest(body, h)
	if got.BodyB64 == "" {
		t.Errorf("non-JSON content should land in BodyB64")
	}
	if got.ParseError != "" {
		t.Errorf("non-JSON content type is not a parse error: %q", got.ParseError)
	}
}

func TestParseResponseJSON(t *testing.T) {
	body := strings.NewReader(`{"id":"msg_1","content":[]}`)
	h := http.Header{"Content-Type": {"application/json"}}
	got, err := ParseResponse(body, h, ParseOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != `{"id":"msg_1","content":[]}` {
		t.Errorf("Body = %s", got.Body)
	}
}

func TestParseResponseSSE(t *testing.T) {
	body := strings.NewReader(`event: message_start
data: {"id":"msg_1"}

event: message_stop
data: {}

`)
	h := http.Header{"Content-Type": {"text/event-stream"}}
	got, err := ParseResponse(body, h, ParseOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 2 {
		t.Errorf("events count = %d, want 2", len(got.Events))
	}
	if got.StreamDone == nil || !*got.StreamDone {
		t.Errorf("StreamDone should be true (saw message_stop)")
	}
	if got.Body != nil {
		t.Errorf("Body should be nil when Events set")
	}
}

func TestParseResponseSSEMidStreamCut(t *testing.T) {
	body := strings.NewReader(`event: message_start
data: {"id":"msg_1"}

event: content_block_delta
data: {"text":"hi"}`)
	h := http.Header{"Content-Type": {"text/event-stream"}}
	got, _ := ParseResponse(body, h, ParseOpts{})
	if got.StreamDone == nil || *got.StreamDone {
		t.Errorf("StreamDone should be false (no terminator)")
	}
	if len(got.Events) != 2 {
		t.Errorf("expected pending accumulator to flush; got %d events", len(got.Events))
	}
}

func TestParseResponseGzipDecompresses(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(`{"id":"msg_1"}`))
	_ = gz.Close()

	h := http.Header{
		"Content-Type":     {"application/json"},
		"Content-Encoding": {"gzip"},
	}
	got, err := ParseResponse(&buf, h, ParseOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != `{"id":"msg_1"}` {
		t.Errorf("decompressed body = %s", got.Body)
	}
	// Content-Encoding must be stripped on successful decompression.
	if http.Header(got.Headers).Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding should be stripped, got %q",
			http.Header(got.Headers).Get("Content-Encoding"))
	}
	// Other headers preserved.
	if http.Header(got.Headers).Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type lost")
	}
}

func TestParseResponseUnsupportedEncodingFallsBack(t *testing.T) {
	// Unknown encoding → keep original bytes, set parse_error, KEEP original
	// Content-Encoding header (because we didn't actually decompress).
	body := strings.NewReader("some-raw-bytes")
	h := http.Header{
		"Content-Type":     {"application/json"},
		"Content-Encoding": {"zstd"},
	}
	got, _ := ParseResponse(body, h, ParseOpts{})
	if got.BodyB64 == "" {
		t.Errorf("BodyB64 should be set")
	}
	if got.ParseError == "" {
		t.Errorf("ParseError should be set")
	}
	if !strings.Contains(got.ParseError, "zstd") {
		t.Errorf("ParseError should mention zstd: %q", got.ParseError)
	}
	if http.Header(got.Headers).Get("Content-Encoding") != "zstd" {
		t.Errorf("on fallback we must keep original Content-Encoding")
	}
}

func TestParseResponseEmptyBody(t *testing.T) {
	h := http.Header{"Content-Type": {"application/json"}}
	got, _ := ParseResponse(strings.NewReader(""), h, ParseOpts{})
	if string(got.Body) != `null` {
		t.Errorf("empty JSON body should marshal as null, got %s", got.Body)
	}
}

func TestSplitEncodingChain(t *testing.T) {
	got := splitEncodingChain("gzip, br ,  zstd")
	want := []string{"gzip", "br", "zstd"}
	if len(got) != len(want) {
		t.Fatalf("chain split = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("chain[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStripHeadersIsCaseInsensitive(t *testing.T) {
	h := trace.Headers{"content-encoding": {"gzip"}, "Content-Type": {"application/json"}}
	out := cloneHeadersStripping(h, "Content-Encoding")
	if _, ok := out["content-encoding"]; ok {
		t.Errorf("lowercase Content-Encoding header should be stripped (case-insensitive)")
	}
	if _, ok := out["Content-Type"]; !ok {
		t.Errorf("Content-Type should survive")
	}
}
