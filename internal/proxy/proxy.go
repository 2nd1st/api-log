package proxy

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// NewReverseProxy builds the httputil.ReverseProxy described in
// ARCHITECTURE § 10.1 and § 10.4. The returned proxy:
//
//   - Forwards to upstream with FlushInterval = -1 so SSE chunks flush
//     to the client without buffering.
//   - Director rewrites req.URL.Scheme/Host and req.Host only; does
//     not touch path, query, or other headers. X-Forwarded-For is
//     suppressed via the nil-assignment trick (Del does NOT work).
//   - Uses the provided Transport (typically a CaptureTransport).
//
// On upstream error, the default ReverseProxy.ErrorHandler is replaced
// with a quiet variant: it writes a 502 to the client and logs nothing
// (the caller is responsible for trace-level error logging).
func NewReverseProxy(upstream *url.URL, transport http.RoundTripper) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Director:      director(upstream),
		Transport:     transport,
		FlushInterval: -1, // flush after every Write — required for SSE
		ErrorHandler:  errorHandler,
	}
	return rp
}

func director(upstream *url.URL) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = upstream.Scheme
		req.URL.Host = upstream.Host
		req.Host = upstream.Host

		// SUPPRESS X-Forwarded-For — see ARCHITECTURE § 10.4 and
		// experiments/x-forwarded-test/. Header.Del() does NOT prevent
		// ReverseProxy.ServeHTTP from appending later; only nil-value
		// assignment does. Verified by Go test in experiments/.
		req.Header["X-Forwarded-For"] = nil
	}
}

func errorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	// Some idiomatic mapping: a context cancellation should produce 499
	// (nginx-style "client closed request") rather than 502, but Go's
	// http package doesn't ship a 499 constant and many tools don't know
	// it either. Use 502 uniformly for now; record cause via the trace.
	_ = err
	if errors.Is(err, http.ErrAbortHandler) {
		// re-panic so net/http server handles it (logs + closes conn).
		panic(http.ErrAbortHandler)
	}
	http.Error(w, "bad gateway", http.StatusBadGateway)
}
