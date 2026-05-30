package media

import (
	"encoding/base64"
	"mime"
	"strings"
)

// decodePayload turns the raw protocol value (a data: URL, an https URL, or
// a bare base64 string) into decoded bytes plus a resolved MIME type. It
// returns (nil, "", false) for any of:
//
//   - empty / whitespace-only payload
//   - http://, https://, gs://, file: scheme URLs (URL-only references)
//   - base64 that fails to decode
//
// declaredMime is whatever the protocol gave us directly (e.g. Anthropic's
// source.media_type or Gemini's inlineData.mimeType). If empty AND the
// payload is a data: URL, the mime is taken from the URL header. Otherwise
// it falls back to "application/octet-stream" so callers still get a
// resolvable extension table entry.
func decodePayload(payload, declaredMime string) (data []byte, mimeType string, ok bool) {
	s := strings.TrimSpace(payload)
	if s == "" {
		return nil, "", false
	}

	// URL-only references: do not fetch. The viewer / exporter only carries
	// what's actually present in the trace; remote fetch is a different
	// product per PHILOSOPHY § 1 (no synthesis).
	switch {
	case strings.HasPrefix(s, "http://"),
		strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "gs://"),
		strings.HasPrefix(s, "file:"):
		return nil, "", false
	}

	// data: URL form — RFC 2397. Example:
	//   data:image/png;base64,iVBOR...
	// We accept ";base64" anywhere before the comma (some encoders put
	// charset= first). Non-base64 data: URLs are rare in LLM traffic and
	// we don't bother with them.
	if strings.HasPrefix(s, "data:") {
		comma := strings.IndexByte(s, ',')
		if comma < 0 {
			return nil, "", false
		}
		header := s[5:comma]
		body := s[comma+1:]
		if !strings.Contains(header, "base64") {
			// data: URL with %-encoded plaintext — not media we extract.
			return nil, "", false
		}
		// Strip ";base64" and parse the remaining mime spec via mime.ParseMediaType.
		mediaSpec := strings.Replace(header, ";base64", "", 1)
		mediaSpec = strings.TrimSpace(mediaSpec)
		if mediaSpec == "" {
			// Per RFC 2397 default is text/plain;charset=US-ASCII, but for
			// a base64 payload with no header the safer assumption is
			// generic octet-stream.
			mimeType = "application/octet-stream"
		} else if mt, _, err := mime.ParseMediaType(mediaSpec); err == nil {
			mimeType = mt
		} else {
			mimeType = mediaSpec
		}
		raw, err := decodeBase64Loose(body)
		if err != nil {
			return nil, "", false
		}
		// declaredMime wins ONLY if data URL didn't carry one. The data:
		// header is closer to ground truth (it's literally next to the
		// bytes) so we keep it on conflict.
		if mimeType == "" {
			mimeType = declaredMime
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		return raw, mimeType, true
	}

	// Bare base64 — sibling mime field is the source of truth.
	raw, err := decodeBase64Loose(s)
	if err != nil {
		return nil, "", false
	}
	mimeType = declaredMime
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return raw, mimeType, true
}

// decodeBase64Loose tries StdEncoding then URLEncoding, with and without
// padding. LLM SDKs are inconsistent about padding (Anthropic strips it
// occasionally, Gemini doesn't) and rare clients use URL-safe alphabet for
// binary payloads. We try the common variants before giving up.
func decodeBase64Loose(s string) ([]byte, error) {
	// Strip stray whitespace (some clients line-wrap at 76 cols per MIME).
	if strings.ContainsAny(s, " \n\r\t") {
		s = stripWhitespace(s)
	}
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	if data, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	if data, err := base64.URLEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// extensionFor maps a MIME type to a lower-case file extension WITHOUT a
// leading dot (Phase K § 3: "lowercase, no leading dot in this field —
// used as `${idx}.${ext}`").
//
// The table covers the formats LLM traffic actually carries (images,
// PDFs, audio, plain text). For unknown MIME types we fall back to "bin",
// which keeps the filename well-formed and signals "we don't know" to
// downstream tools — preferable to silently picking an incorrect ext.
func extensionFor(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/svg+xml":
		return "svg"
	case "image/bmp":
		return "bmp"
	case "image/tiff":
		return "tiff"
	case "image/heic":
		return "heic"
	case "image/heif":
		return "heif"
	case "application/pdf":
		return "pdf"
	case "audio/wav", "audio/x-wav":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/ogg":
		return "ogg"
	case "audio/webm":
		return "webm"
	case "audio/flac":
		return "flac"
	case "audio/aac":
		return "aac"
	case "audio/opus":
		return "opus"
	case "video/mp4":
		return "mp4"
	case "video/webm":
		return "webm"
	case "video/quicktime":
		return "mov"
	case "text/plain":
		return "txt"
	case "text/html":
		return "html"
	case "text/csv":
		return "csv"
	case "text/markdown":
		return "md"
	case "application/json":
		return "json"
	case "application/xml", "text/xml":
		return "xml"
	case "application/zip":
		return "zip"
	case "application/octet-stream", "":
		return "bin"
	}
	// Unknown but well-formed: bin is the deliberate fallback. Don't try
	// to be clever with mime.ExtensionsByType — its results vary by host
	// mime.types and we want deterministic output.
	return "bin"
}
