// Verify how httputil.ReverseProxy handles X-Forwarded-For under three
// suppression strategies. Tests the claim from the 4th-pass review that
// Director.Del() does NOT prevent ReverseProxy from appending the header.
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
)

func main() {
	// Backend echoes the headers it actually received.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "X-Forwarded-For:   %q\n", r.Header.Get("X-Forwarded-For"))
		_, _ = fmt.Fprintf(w, "X-Forwarded-Host:  %q\n", r.Header.Get("X-Forwarded-Host"))
		_, _ = fmt.Fprintf(w, "X-Forwarded-Proto: %q\n", r.Header.Get("X-Forwarded-Proto"))
	}))
	defer backend.Close()

	upstream, _ := url.Parse(backend.URL)

	run := func(label string, proxy *httputil.ReverseProxy) {
		srv := httptest.NewServer(proxy)
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			fmt.Printf("=== %s ===\nERROR: %v\n\n", label, err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("=== %s ===\n%s\n", label, string(body))
	}

	// Test 1: Director with Header.Del — the claim is that this does NOT
	// suppress because ReverseProxy.ServeHTTP appends X-Forwarded-For
	// AFTER Director runs.
	run("Test 1: Director with Header.Del", &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
			req.Header.Del("X-Forwarded-For")
			req.Header.Del("X-Forwarded-Host")
			req.Header.Del("X-Forwarded-Proto")
		},
	})

	// Test 2: Director with nil-value assignment — the claim is that
	// req.Header[name] = nil DOES suppress, because ReverseProxy treats
	// a present-but-nil header value as "do not modify".
	run("Test 2: Director with req.Header[name] = nil", &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
			req.Header["X-Forwarded-For"] = nil
			req.Header["X-Forwarded-Host"] = nil
			req.Header["X-Forwarded-Proto"] = nil
		},
	})

	// Test 3: Rewrite (Go 1.20+) without SetXForwarded — the documented
	// modern way to fully control X-Forwarded-* injection.
	run("Test 3: Rewrite without SetXForwarded", &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(upstream)
			// Intentionally NOT calling req.SetXForwarded()
		},
	})

	// Test 4: Default (no Director suppression at all)
	run("Test 4: Default Director, no suppression", &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
		},
	})
}
