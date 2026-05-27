package parser

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/leoyun/api-log/internal/trace"
)

// decodeEncoding decompresses raw bytes according to Content-Encoding,
// and returns the decoded bytes plus the headers with Content-Encoding
// stripped on success.
//
// Per ARCHITECTURE § 10.3 / § 7.1: stripping Content-Encoding from the
// recorded headers is a deliberate **fidelity tradeoff** — the JSONL
// headers are no longer byte-identical to wire, but resp.body and
// resp.headers stay mutually consistent for the consumer.
//
// Returns:
//   - decoded: bytes after decompression (or original raw if identity).
//   - decodedHeaders: input headers minus Content-Encoding (only on
//     successful decompression).
//   - errMsg: human-readable string when we cannot decompress (unknown
//     encoding, multi-link chain with an unknown link, or decoder error).
//     Empty string means success. When non-empty, the caller keeps the
//     ORIGINAL raw bytes (with original headers) and tags the body as
//     body_b64 with this errMsg as parse_error.
func decodeEncoding(raw []byte, headers trace.Headers) (decoded []byte, decodedHeaders trace.Headers, errMsg string) {
	ce := http.Header(headers).Get("Content-Encoding")
	ce = strings.TrimSpace(ce)
	if ce == "" || strings.EqualFold(ce, "identity") {
		return raw, headers, ""
	}

	// Chained encodings (e.g. "gzip, br"): decode left-to-right.
	links := splitEncodingChain(ce)

	current := raw
	for _, link := range links {
		next, err := decodeOne(link, current)
		if err != nil {
			return nil, nil, err.Error()
		}
		current = next
	}

	// Success: strip Content-Encoding from the captured headers so the
	// consumer sees decoded body next to headers that describe it.
	out := cloneHeadersStripping(headers, "Content-Encoding")
	return current, out, ""
}

func decodeOne(link string, in []byte) ([]byte, error) {
	switch strings.ToLower(link) {
	case "identity":
		return in, nil
	case "gzip", "x-gzip":
		gz, err := gzip.NewReader(bytes.NewReader(in))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		out, err := io.ReadAll(gz)
		if err != nil {
			return nil, fmt.Errorf("gzip read: %w", err)
		}
		return out, nil
	// br and zstd are intentionally NOT supported in v0 — the stdlib
	// doesn't include them and pulling deps for rarely-seen encodings
	// is not worth it. If Content-Encoding names them, we fall back
	// to body_b64 with parse_error per ARCHITECTURE § 10.3.
	case "br":
		return nil, fmt.Errorf("Content-Encoding %q not supported in v0", link)
	case "zstd":
		return nil, fmt.Errorf("Content-Encoding %q not supported in v0", link)
	default:
		return nil, fmt.Errorf("Content-Encoding %q is unknown", link)
	}
}

// splitEncodingChain parses a Content-Encoding value like "gzip, br" or
// "gzip" into individual lower-cased encoding names in order.
func splitEncodingChain(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cloneHeadersStripping returns a copy of h with the named header removed.
// Header lookup is case-insensitive per HTTP semantics (handled by Go's
// http.Header semantics underneath).
func cloneHeadersStripping(h trace.Headers, name string) trace.Headers {
	out := make(trace.Headers, len(h))
	for k, vs := range h {
		if strings.EqualFold(k, name) {
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}
