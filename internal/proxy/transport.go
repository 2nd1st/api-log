// Package proxy assembles the httputil.ReverseProxy + custom Transport
// pipeline documented in ARCHITECTURE § 10.2.
//
// The custom Transport tees req.Body and resp.Body to per-trace capture
// sinks. Headers are NOT written to disk — they are read at finalize time
// directly from the http.Request / http.Response structs.
package proxy

import (
	"context"
	"io"
	"net/http"

	"github.com/leoyun/api-log/internal/capture"
)

type traceIDKey struct{}

// WithTraceID attaches a trace ID to a context, retrievable by the
// custom Transport's RoundTrip via TraceIDFromContext.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// TraceIDFromContext recovers a trace ID attached by WithTraceID. Returns
// empty string if absent (which indicates a wiring bug — the handler
// should always attach a trace ID before calling the proxy).
func TraceIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(traceIDKey{}).(string)
	return v
}

// SinkLookup is how the custom Transport finds the right sinks for a
// given trace. The forwarding handler registers sinks at request start
// and removes them at finalize.
type SinkLookup interface {
	// SinksFor returns the req/resp capture sinks for traceID. Both are
	// non-nil when the trace is active; both are nil if the trace has
	// been finalized (in which case the Transport silently drops capture).
	SinksFor(traceID string) (req, resp *capture.Sink)
}

// CaptureTransport wraps an inner http.RoundTripper and tees req.Body /
// resp.Body to per-trace capture sinks.
//
// Critically: this transport does NOT consume req.Body itself — it wraps
// req.Body in a teeing ReadCloser so the inner Transport's read drives
// the tee. Calling req.Write/req.WriteProxy here would consume the body
// to EOF and the inner RoundTrip would forward an empty body upstream.
type CaptureTransport struct {
	// Inner is the underlying transport (typically a configured
	// *http.Transport with DisableCompression=true).
	Inner http.RoundTripper
	// Sinks looks up the per-trace sinks at RoundTrip time.
	Sinks SinkLookup
}

// RoundTrip wires the body tees and forwards via the inner transport.
//
// On error from the inner transport, the response side never produced
// anything; the resp.Body wrap is not applied. The trace's finalize will
// trigger via the error path.
func (t *CaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	traceID := TraceIDFromContext(req.Context())
	reqSink, respSink := (*capture.Sink)(nil), (*capture.Sink)(nil)
	if t.Sinks != nil {
		reqSink, respSink = t.Sinks.SinksFor(traceID)
	}

	// 1) Wrap req.Body so the inner Transport's read also feeds reqSink.
	//    Per ARCHITECTURE § 10.2: GetBody = nil — we don't support
	//    transport retry because the tee has already consumed bytes once.
	if req.Body != nil && req.Body != http.NoBody && reqSink != nil {
		req.Body = newTeeReadCloser(req.Body, reqSink)
		req.GetBody = nil
	}

	// 2) Forward.
	resp, err := t.Inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// 3) Wrap resp.Body so the proxy's read also feeds respSink.
	if resp.Body != nil && respSink != nil {
		resp.Body = newTeeReadCloser(resp.Body, respSink)
	}
	return resp, nil
}

// teeReadCloser reads from src, writes the read bytes to sink, and
// returns the bytes to the caller. The sink Write is non-blocking
// (sinks drop on full channel); src reads are never short-read due to
// capture behavior.
//
// Close closes src. The sink is owned by the trace and closed by the
// forwarding handler at finalize.
type teeReadCloser struct {
	src  io.ReadCloser
	sink *capture.Sink
}

func newTeeReadCloser(src io.ReadCloser, sink *capture.Sink) *teeReadCloser {
	return &teeReadCloser{src: src, sink: sink}
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		_, _ = t.sink.Write(p[:n])
	}
	return n, err
}

func (t *teeReadCloser) Close() error {
	return t.src.Close()
}
