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

// MetaCapture is an optional hook that receives the outbound request
// headers (post-Director) and the upstream response status + headers
// as soon as they are known. Headers are passed as clones; callers may
// retain them safely. Used by the forwarding handler to populate the
// trace.Body objects at finalize.
type MetaCapture interface {
	OnReqHeaders(traceID string, h http.Header)
	OnRespMeta(traceID string, statusCode int, h http.Header)
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
	// Meta receives request/response metadata callbacks. Optional;
	// when nil, the Transport only does body tee.
	Meta MetaCapture
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

	// 1) Capture outbound request headers (post-Director). Clone so the
	//    caller can retain safely without seeing later mutations.
	if t.Meta != nil && traceID != "" {
		t.Meta.OnReqHeaders(traceID, req.Header.Clone())
	}

	// 2) Wrap req.Body so the inner Transport's read also feeds reqSink.
	//    Per ARCHITECTURE § 10.2: GetBody = nil — we don't support
	//    transport retry because the tee has already consumed bytes once.
	if req.Body != nil && req.Body != http.NoBody && reqSink != nil {
		req.Body = newTeeReadCloser(req.Body, reqSink)
		req.GetBody = nil
	}

	// 3) Forward.
	resp, err := t.Inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// 4) Capture response metadata before wrapping body. Status + headers
	//    are known at this point regardless of how much body has streamed.
	if t.Meta != nil && traceID != "" {
		t.Meta.OnRespMeta(traceID, resp.StatusCode, resp.Header.Clone())
	}

	// 5) Wrap resp.Body so the proxy's read also feeds respSink.
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
