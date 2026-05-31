package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2nd1st/api-log/internal/capture"
)

// staticSinks implements SinkLookup with a fixed pair.
type staticSinks struct {
	mu       sync.Mutex
	traceID  string
	reqBuf   []byte
	respBuf  []byte
	reqSink  *capture.Sink
	respSink *capture.Sink
}

func newStaticSinks(traceID string) *staticSinks {
	s := &staticSinks{traceID: traceID}
	reqCh := make(chan capture.Chunk, 256)
	respCh := make(chan capture.Chunk, 256)
	s.reqSink = &capture.Sink{Ch: reqCh}
	s.respSink = &capture.Sink{Ch: respCh}

	// Background goroutines that mirror channel contents into byte buffers
	// so the test can assert on captured bytes.
	go func() {
		for c := range reqCh {
			s.mu.Lock()
			s.reqBuf = append(s.reqBuf, c.Data...)
			s.mu.Unlock()
		}
	}()
	go func() {
		for c := range respCh {
			s.mu.Lock()
			s.respBuf = append(s.respBuf, c.Data...)
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *staticSinks) SinksFor(traceID string) (*capture.Sink, *capture.Sink) {
	if traceID != s.traceID {
		return nil, nil
	}
	return s.reqSink, s.respSink
}

func (s *staticSinks) bytes() (req, resp []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req = append([]byte(nil), s.reqBuf...)
	resp = append([]byte(nil), s.respBuf...)
	return
}

func TestReverseProxyForwardsAndTees(t *testing.T) {
	// Backend echoes "HELLO_<req body>" and records what it received.
	var backendGotBody string
	var backendGotXFF string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		backendGotBody = string(body)
		backendGotXFF = r.Header.Get("X-Forwarded-For")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "HELLO_"+backendGotBody)
	}))
	defer backend.Close()
	upstreamURL, _ := url.Parse(backend.URL)

	sinks := newStaticSinks("trace-1")
	rt := &CaptureTransport{Inner: http.DefaultTransport, Sinks: sinks}
	rp := NewReverseProxy(upstreamURL, rt)

	// Wrap RP in a handler that attaches the trace ID context.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.WithContext(WithTraceID(r.Context(), "trace-1"))
		rp.ServeHTTP(w, r2)
	})
	front := httptest.NewServer(handler)
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/messages", "text/plain", strings.NewReader("WORLD"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// 1. Body flowed through to the backend.
	if backendGotBody != "WORLD" {
		t.Errorf("backend got body = %q, want WORLD", backendGotBody)
	}
	if string(respBody) != "HELLO_WORLD" {
		t.Errorf("client got body = %q, want HELLO_WORLD", string(respBody))
	}
	// 2. X-Forwarded-For suppressed (ARCHITECTURE § 10.4).
	if backendGotXFF != "" {
		t.Errorf("X-Forwarded-For = %q, expected to be suppressed", backendGotXFF)
	}

	// 3. Bytes were captured on both sides.
	// Allow the drain goroutines a tick to flush the channel buffers.
	time.Sleep(50 * time.Millisecond)
	close(sinks.reqSink.Ch)
	close(sinks.respSink.Ch)
	time.Sleep(50 * time.Millisecond)

	gotReq, gotResp := sinks.bytes()
	if string(gotReq) != "WORLD" {
		t.Errorf("captured req = %q, want WORLD", gotReq)
	}
	if string(gotResp) != "HELLO_WORLD" {
		t.Errorf("captured resp = %q, want HELLO_WORLD", gotResp)
	}
}

func TestReverseProxyForwardsBodyEvenWhenSinkAbsent(t *testing.T) {
	// If SinkLookup returns nil sinks (e.g. trace already finalized),
	// the proxy must still forward correctly — capture is best-effort,
	// forwarding is not.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte("ack:" + string(body)))
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	// No SinkLookup attached at all.
	rt := &CaptureTransport{Inner: http.DefaultTransport, Sinks: nil}
	rp := NewReverseProxy(u, rt)
	front := httptest.NewServer(rp)
	defer front.Close()

	resp, err := http.Post(front.URL, "text/plain", strings.NewReader("PAYLOAD"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "ack:PAYLOAD" {
		t.Errorf("response without sinks = %q, want ack:PAYLOAD", got)
	}
}
