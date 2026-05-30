package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leoyun/api-log/internal/capture"
	pluginv2 "github.com/leoyun/api-log/internal/plugin/v2"
	"github.com/leoyun/api-log/internal/proxy"
)

// TestRunAfterChainOnIntercept_AfterPluginDecoratesBeforeIntercept is
// the BEFORE-intercept + AFTER-chain contract from spec §2.2 / §2.4:
// when a BEFORE plugin produces an intercept response, the AFTER chain
// MUST still run on the synthesized response so decorators
// (text-append, watermark, etc.) get to mutate it before the client
// sees it.
func TestRunAfterChainOnIntercept_AfterPluginDecoratesBeforeIntercept(t *testing.T) {
	// Build a registry with one AFTER plugin (text-append) that
	// appends " [decorated]" to assistant content. The BEFORE-side
	// intercept is built by hand below.
	ctor, ok := pluginv2.LookupBuiltin("text-append")
	if !ok {
		t.Fatal("text-append not registered (init order broken?)")
	}
	built, err := ctor(map[string]any{
		"down": map[string]any{"suffix": " [decorated]", "target": "content"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if _, ok := built.(pluginv2.AfterPlugin); !ok {
		t.Fatal("text-append should implement AfterPlugin")
	}
	reg, errs := pluginv2.NewRegistry([]pluginv2.InstanceConfig{
		{Type: "text-append", ID: "footer", Enabled: true, Config: map[string]any{
			"down": map[string]any{"suffix": " [decorated]", "target": "content"},
		}},
	})
	if len(errs) > 0 {
		t.Fatalf("registry errs: %v", errs)
	}

	// Synthesized intercept response in Anthropic Messages shape so
	// ParsedResponseFromBody decodes Content cleanly.
	interceptBody := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"blocked"}],"model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	src := &pluginv2.InterceptInfo{
		Type: "before-blocker",
		ID:   "test",
		Hook: "before",
		Response: &pluginv2.InterceptResponse{
			Status:  403,
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    interceptBody,
		},
	}
	req := &pluginv2.ParsedRequest{
		Protocol: pluginv2.ProtocolMessages,
		Path:     "/v1/messages",
	}

	final := runAfterChainOnIntercept(context.Background(), reg, req, src)
	if final == nil || final.Response == nil {
		t.Fatal("final intercept nil")
	}
	if final.Response.Status != 403 {
		t.Errorf("status drift: %d, want 403", final.Response.Status)
	}
	if final.Type != "before-blocker" || final.Hook != "before" {
		t.Errorf("intercept marker should preserve originator; got type=%q hook=%q", final.Type, final.Hook)
	}

	// The AFTER chain should have appended the suffix into the
	// re-serialized content[0].text.
	var shape struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(final.Response.Body, &shape); err != nil {
		t.Fatalf("re-serialized body invalid JSON: %v\nbody=%s", err, final.Response.Body)
	}
	if len(shape.Content) == 0 {
		t.Fatalf("content array empty: %s", final.Response.Body)
	}
	if !strings.Contains(shape.Content[0].Text, "blocked [decorated]") {
		t.Errorf("AFTER plugin did not decorate intercept content; got %q", shape.Content[0].Text)
	}
}

// TestRunAfterChainOnIntercept_NoAfterPluginsPassesThrough confirms
// the helper is a no-op when nothing is registered on the AFTER side.
func TestRunAfterChainOnIntercept_NoAfterPluginsPassesThrough(t *testing.T) {
	reg, errs := pluginv2.NewRegistry(nil)
	if len(errs) > 0 {
		t.Fatalf("registry errs: %v", errs)
	}
	src := &pluginv2.InterceptInfo{
		Response: &pluginv2.InterceptResponse{
			Status: 403,
			Body:   []byte(`{"error":"blocked"}`),
		},
	}
	req := &pluginv2.ParsedRequest{Protocol: pluginv2.ProtocolMessages, Path: "/v1/messages"}
	final := runAfterChainOnIntercept(context.Background(), reg, req, src)
	if final != src {
		t.Errorf("with no AFTER plugins, helper should return source intercept verbatim")
	}
}

// streamCaptureSinks is a SinkLookup for tests that mirrors what
// staticSinks does in internal/proxy/proxy_test.go: it materializes a
// reqSink + respSink for one fixed trace ID, and drains both channels
// into byte buffers so the test can assert on captured bytes.
type streamCaptureSinks struct {
	mu       sync.Mutex
	traceID  string
	reqBuf   []byte
	respBuf  []byte
	reqSink  *capture.Sink
	respSink *capture.Sink
	reqDone  chan struct{}
	respDone chan struct{}
}

func newStreamCaptureSinks(traceID string) *streamCaptureSinks {
	s := &streamCaptureSinks{traceID: traceID}
	reqCh := make(chan capture.Chunk, 256)
	respCh := make(chan capture.Chunk, 256)
	s.reqSink = &capture.Sink{Ch: reqCh}
	s.respSink = &capture.Sink{Ch: respCh}
	s.reqDone = make(chan struct{})
	s.respDone = make(chan struct{})
	go func() {
		defer close(s.reqDone)
		for c := range reqCh {
			s.mu.Lock()
			s.reqBuf = append(s.reqBuf, c.Data...)
			s.mu.Unlock()
		}
	}()
	go func() {
		defer close(s.respDone)
		for c := range respCh {
			s.mu.Lock()
			s.respBuf = append(s.respBuf, c.Data...)
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *streamCaptureSinks) SinksFor(traceID string) (*capture.Sink, *capture.Sink) {
	if traceID != s.traceID {
		return nil, nil
	}
	return s.reqSink, s.respSink
}

func (s *streamCaptureSinks) closeAndWait() {
	close(s.reqSink.Ch)
	close(s.respSink.Ch)
	<-s.reqDone
	<-s.respDone
}

func (s *streamCaptureSinks) bytes() (req, resp []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req = append([]byte(nil), s.reqBuf...)
	resp = append([]byte(nil), s.respBuf...)
	return
}

// TestStreamingAfterMutation_CaptureReflectsClientBytes is the v0.1.0
// regression test for the "respSink records original upstream bytes"
// bug (docs/reviews/v0.1.0-pre-release.md Critical #2).
//
// Setup: a fake upstream sends one Chat-protocol SSE event carrying
// the assistant content delta "Hello" and then closes without a
// terminator. A text-append AFTER plugin is registered with
// down.suffix = " cat" and down.target = "content"; the
// StreamDispatcher's EOF flush is what fires OnLastTextDelta, so the
// last-delta becomes "Hello cat".
//
// Pre-fix the response capture tee is wrapped around resp.Body BEFORE
// ModifyResponse runs, so respSink would record the original
// upstream bytes ("Hello") while the client receives the mutated
// bytes ("Hello cat"). Post-fix the tee moves to the downstream pipe
// reader and respSink reflects what the client actually got.
//
// We assert on the suffix substring (not exact equality) because
// SSE encoding adds "data: " prefixes / blank-line terminators that
// are orthogonal to the bug.
func TestStreamingAfterMutation_CaptureReflectsClientBytes(t *testing.T) {
	const (
		traceID = "trace-stream-1"
		suffix  = " cat"
	)

	// 1. Fake upstream: one Chat SSE event whose delta.content is
	//    "Hello", flushed, then connection close. No "[DONE]"
	//    terminator — the EOF flush is what we want to exercise.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w,
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n",
		); err != nil {
			t.Logf("backend write: %v", err)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer backend.Close()
	upstreamURL, _ := url.Parse(backend.URL)

	// 2. Registry with one AFTER plugin (text-append) that appends
	//    suffix to content. Routes empty = matches all paths.
	reg, errs := pluginv2.NewRegistry([]pluginv2.InstanceConfig{
		{
			Type:    "text-append",
			ID:      "footer",
			Enabled: true,
			Config: map[string]any{
				"down": map[string]any{
					"suffix": suffix,
					"target": "content",
				},
			},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("registry errs: %v", errs)
	}

	// 3. CaptureTransport + ReverseProxy wired the same way main.go
	//    wires them: capture sinks via streamCaptureSinks, AFTER chain
	//    via makeModifyResponse.
	sinks := newStreamCaptureSinks(traceID)
	rt := &proxy.CaptureTransport{Inner: http.DefaultTransport, Sinks: sinks}
	rp := proxy.NewReverseProxy(upstreamURL, rt)
	rp.ModifyResponse = makeModifyResponse(reg)

	// 4. Front handler attaches the trace ID + a post-BEFORE-chain
	//    ParsedRequest (Chat protocol). makeModifyResponse short-
	//    circuits to passthrough when parsedRequestFromContext is nil,
	//    so this stash is what arms the streaming-AFTER path.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := proxy.WithTraceID(r.Context(), traceID)
		ctx = withParsedRequest(ctx, &pluginv2.ParsedRequest{
			Protocol: pluginv2.ProtocolChat,
			Path:     "/v1/chat/completions",
		})
		rp.ServeHTTP(w, r.WithContext(ctx))
	})
	front := httptest.NewServer(handler)
	defer front.Close()

	// 5. Drive a request and read the client-visible body.
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	clientBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read client body: %v", err)
	}

	// 6. The client must see the mutated content. This passes on both
	//    buggy and fixed code (mutation pipe was always wired into
	//    resp.Body before the client copy); it serves as a sanity
	//    check that the dispatcher actually ran.
	if !strings.Contains(string(clientBody), "Hello"+suffix) {
		t.Fatalf("client body missing suffix; got %q", string(clientBody))
	}

	// 7. The actual regression assertion: respSink must contain the
	//    mutated content too. Pre-fix this fails because the tee saw
	//    raw upstream bytes ("Hello", no suffix).
	//
	//    Give the capture drainer a moment to flush — Sink.Write is a
	//    non-blocking channel send and the mirroring goroutine runs
	//    asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, respBuf := sinks.bytes()
		if strings.Contains(string(respBuf), "Hello"+suffix) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sinks.closeAndWait()

	_, respBuf := sinks.bytes()
	if !strings.Contains(string(respBuf), "Hello"+suffix) {
		t.Errorf("captured response stream should contain post-mutation suffix %q; got %q",
			suffix, string(respBuf))
	}
	// Also confirm the captured stream looks like an SSE frame, not
	// a re-encoded blob — guards against a regression that
	// accidentally tees into a JSON marshaler instead.
	if !strings.Contains(string(respBuf), "data:") {
		t.Errorf("captured response stream lost SSE framing; got %q", string(respBuf))
	}
}
