// Package parser is the finalize-time body parser described in
// ARCHITECTURE § 7.1 step 7.
//
// Inputs: a captured-body io.Reader (typically a tmp file the drainer
// wrote) and the corresponding http.Header. Output: a trace.Body in one
// of three structural forms (body / events / body_b64) plus a
// parse_error when degraded to body_b64.
package parser

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/leoyun/api-log/internal/sse"
	"github.com/leoyun/api-log/internal/trace"
)

// ParseRequest parses a captured request body into a trace.Body.
//
// The request side is always non-streaming (clients don't stream POST
// bodies to LLM gateways in practice), so this is just JSON-or-binary.
func ParseRequest(r io.Reader, headers http.Header) (trace.Body, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return trace.Body{}, fmt.Errorf("read req body: %w", err)
	}
	return finalize(body, trace.Headers(headers), false /* not streaming */), nil
}

// ParseResponse parses a captured response body into a trace.Body.
//
// Determines streaming vs non-streaming via the response Content-Type
// (text/event-stream → stream). The header check is the only place
// shape detection happens at finalize; the sub-shape (data-only vs
// event-named) is inferred inside internal/sse.
func ParseResponse(r io.Reader, headers http.Header) (trace.Body, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return trace.Body{}, fmt.Errorf("read resp body: %w", err)
	}
	streaming := isSSEContentType(headers.Get("Content-Type"))
	return finalize(body, trace.Headers(headers), streaming), nil
}

// finalize materializes the trace.Body from raw bytes + headers.
//
// Decompression per ARCHITECTURE § 10.3: known Content-Encoding values
// (gzip / br / zstd / identity) decompress in memory; on success the
// header is stripped so resp.body and resp.headers are mutually
// consistent. Unknown encodings → body stays compressed in body_b64.
func finalize(raw []byte, headers trace.Headers, streaming bool) trace.Body {
	decoded, decodedHeaders, decodeErr := decodeEncoding(raw, headers)
	if decodeErr != "" {
		// Unknown Content-Encoding (or decompression failure): keep raw bytes.
		return trace.Body{
			Headers:    headers,
			BodyB64:    base64.StdEncoding.EncodeToString(raw),
			ParseError: decodeErr,
		}
	}

	if len(decoded) == 0 {
		// Empty body: emit body:null for JSON-shaped, or empty body_b64
		// for stream-shaped (so the consumer knows the stream was empty).
		if streaming {
			return trace.Body{Headers: decodedHeaders}
		}
		// JSON `null` is valid; emit as body:null so the field is present.
		return trace.Body{Headers: decodedHeaders, Body: json.RawMessage(`null`)}
	}

	if streaming {
		return parseSSE(decoded, decodedHeaders)
	}

	if isJSONContentType(http.Header(decodedHeaders).Get("Content-Type")) {
		return parseJSON(decoded, decodedHeaders)
	}

	// Non-JSON, non-SSE → keep raw bytes (rare on LLM gateway traffic,
	// but multipart uploads / images / audio land here).
	return trace.Body{
		Headers: decodedHeaders,
		BodyB64: base64.StdEncoding.EncodeToString(decoded),
	}
}

func parseJSON(decoded []byte, headers trace.Headers) trace.Body {
	if !json.Valid(decoded) {
		return trace.Body{
			Headers:    headers,
			BodyB64:    base64.StdEncoding.EncodeToString(decoded),
			ParseError: "invalid JSON",
		}
	}
	return trace.Body{
		Headers: headers,
		Body:    json.RawMessage(decoded),
	}
}

func parseSSE(decoded []byte, headers trace.Headers) trace.Body {
	res := sse.Parse(bytes.NewReader(decoded))
	if res.ParseError != "" {
		// SSE parsing failed; preserve raw bytes for the consumer to inspect.
		return trace.Body{
			Headers:    headers,
			BodyB64:    base64.StdEncoding.EncodeToString(decoded),
			ParseError: "sse: " + res.ParseError,
		}
	}
	streamDone := res.StreamDone
	return trace.Body{
		Headers:    headers,
		Events:     res.Events,
		StreamDone: &streamDone,
	}
}

func isSSEContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.EqualFold(mt, "text/event-stream")
}

func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	// Cover both application/json and application/*+json variants.
	mt = strings.ToLower(mt)
	if mt == "application/json" || mt == "text/json" {
		return true
	}
	if strings.HasPrefix(mt, "application/") && strings.HasSuffix(mt, "+json") {
		return true
	}
	return false
}

