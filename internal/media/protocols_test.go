package media

import (
	"strings"
	"testing"
)

// Unit-level tests for individual detectors and the decode helpers. The
// walk_test.go file covers end-to-end shapes; this file pins down edge
// cases that are easy to miss when only testing top-level traces.

func TestDecodePayload_DataURL_StandardPNG(t *testing.T) {
	in := "data:image/png;base64,aGVsbG8="
	data, mime, ok := decodePayload(in, "")
	if !ok {
		t.Fatal("expected ok")
	}
	if mime != "image/png" {
		t.Errorf("mime %q", mime)
	}
	if string(data) != "hello" {
		t.Errorf("data %q", data)
	}
}

func TestDecodePayload_DataURL_NoMimeHeader(t *testing.T) {
	// data:;base64,... — RFC 2397 allows omitted mediatype. We should
	// still extract; mime falls back to octet-stream.
	in := "data:;base64,aGVsbG8="
	data, mime, ok := decodePayload(in, "")
	if !ok {
		t.Fatal("expected ok for empty mediatype")
	}
	if mime != "application/octet-stream" {
		t.Errorf("mime %q, want application/octet-stream", mime)
	}
	if string(data) != "hello" {
		t.Errorf("data %q", data)
	}
}

func TestDecodePayload_DataURL_PlaintextRejected(t *testing.T) {
	// data: URL without ;base64 — not media we extract.
	in := "data:text/plain,hello%20world"
	_, _, ok := decodePayload(in, "")
	if ok {
		t.Fatal("plaintext data: URL should not be extracted")
	}
}

func TestDecodePayload_BareBase64_UsesDeclaredMime(t *testing.T) {
	data, mime, ok := decodePayload("aGVsbG8=", "image/jpeg")
	if !ok {
		t.Fatal("ok")
	}
	if mime != "image/jpeg" {
		t.Errorf("mime %q", mime)
	}
	if string(data) != "hello" {
		t.Errorf("data %q", data)
	}
}

func TestDecodePayload_BareBase64_DefaultsToOctetStream(t *testing.T) {
	_, mime, ok := decodePayload("aGVsbG8=", "")
	if !ok {
		t.Fatal("ok")
	}
	if mime != "application/octet-stream" {
		t.Errorf("mime %q", mime)
	}
}

func TestDecodePayload_HTTPSSkipped(t *testing.T) {
	_, _, ok := decodePayload("https://example.com/x.png", "image/png")
	if ok {
		t.Fatal("https URL must NOT be decoded")
	}
}

func TestDecodePayload_HTTPSkipped(t *testing.T) {
	_, _, ok := decodePayload("http://example.com/x.png", "image/png")
	if ok {
		t.Fatal("http URL must NOT be decoded")
	}
}

func TestDecodePayload_GSSkipped(t *testing.T) {
	_, _, ok := decodePayload("gs://bucket/path/x.png", "image/png")
	if ok {
		t.Fatal("gs:// URI must NOT be decoded")
	}
}

func TestDecodePayload_EmptySkipped(t *testing.T) {
	_, _, ok := decodePayload("", "image/png")
	if ok {
		t.Fatal("empty payload must be skipped")
	}
	_, _, ok = decodePayload("   \n\t", "image/png")
	if ok {
		t.Fatal("whitespace payload must be skipped")
	}
}

func TestDecodePayload_BadBase64Skipped(t *testing.T) {
	_, _, ok := decodePayload("not!!!valid!!!base64", "image/png")
	if ok {
		t.Fatal("malformed base64 must be skipped (no extraction)")
	}
}

func TestDecodePayload_LineWrappedBase64(t *testing.T) {
	// MIME line-wrapping at 76 cols is allowed by some encoders.
	wrapped := "aGVsbG8t\ncG5nLWJ5\ndGVz"
	data, _, ok := decodePayload(wrapped, "image/png")
	if !ok {
		t.Fatal("line-wrapped base64 should still decode")
	}
	if string(data) != "hello-png-bytes" {
		t.Errorf("data %q", data)
	}
}

func TestExtensionFor_KnownTypes(t *testing.T) {
	cases := map[string]string{
		"image/png":                "png",
		"image/jpeg":               "jpg",
		"IMAGE/JPEG":               "jpg", // case-insensitive
		"image/webp":               "webp",
		"application/pdf":          "pdf",
		"audio/wav":                "wav",
		"audio/mpeg":               "mp3",
		"video/mp4":                "mp4",
		"application/octet-stream": "bin",
		"":                         "bin",
		"x-mystery/whatever":       "bin", // unknown → bin
	}
	for mime, want := range cases {
		got := extensionFor(mime)
		if got != want {
			t.Errorf("extensionFor(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestDetectOpenAIImageURL_BareURLString(t *testing.T) {
	// The detector only fires on object-form image_url ({"url":"..."}).
	// A bare-string image_url is the Responses-protocol shape; that's
	// detectResponsesInputImg's job.
	claimed, cands := detectOpenAIImageURL(map[string]any{"url": "data:image/png;base64,aGVsbG8="}, "messages[0].content[0].image_url")
	if !claimed {
		t.Fatal("should claim image_url object")
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	if !strings.HasSuffix(cands[0].path, ".url") {
		t.Errorf("path %q", cands[0].path)
	}
}

func TestDetectOpenAIImageURL_PathMismatch(t *testing.T) {
	// Object that LOOKS like image_url but isn't at an image_url path:
	// don't claim. (Defensive — protects against generic {"url":"..."}
	// objects elsewhere in the body, like tool-call results.)
	claimed, _ := detectOpenAIImageURL(map[string]any{"url": "data:image/png;base64,aGVsbG8="}, "tools[0].function")
	if claimed {
		t.Fatal("must not claim non-image_url path")
	}
}

func TestDetectAnthropicSource_UnknownTypeNotClaimed(t *testing.T) {
	// Anthropic adds new source types over time; an unknown discriminator
	// should not be claimed so the walker can keep descending.
	claimed, _ := detectAnthropicSource(map[string]any{"type": "future_kind", "data": "..."}, "messages[0].content[0].source")
	if claimed {
		t.Fatal("unknown source.type must not be claimed (defensive)")
	}
}

func TestDetectAnthropicSource_MissingMediaTypeStillExtracts(t *testing.T) {
	// media_type is optional in some malformed payloads — extractor should
	// still produce a candidate (decoder fills in octet-stream).
	claimed, cands := detectAnthropicSource(map[string]any{"type": "base64", "data": "aGVsbG8="}, "messages[0].content[0].source")
	if !claimed {
		t.Fatal("should claim")
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(cands))
	}
	if cands[0].declaredMime != "" {
		t.Errorf("declaredMime should be empty for missing media_type, got %q", cands[0].declaredMime)
	}
}

func TestDetectGeminiInlineData_PrefersCamelCaseOverSnake(t *testing.T) {
	// When both keys are present (unlikely but possible after re-marshal),
	// camelCase wins because it's the documented native form.
	claimed, cands := detectGeminiInlineData(
		map[string]any{"mimeType": "image/png", "mime_type": "image/jpeg", "data": "aGVsbG8="},
		"contents[0].parts[0].inlineData",
	)
	if !claimed {
		t.Fatal("should claim")
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates", len(cands))
	}
	if cands[0].declaredMime != "image/png" {
		t.Errorf("camelCase mimeType should win, got %q", cands[0].declaredMime)
	}
}

func TestDetectResponsesInputImg_ClaimsAndSkips(t *testing.T) {
	claimed, cands := detectResponsesInputImg(
		map[string]any{"type": "input_image", "image_url": "https://example.com/x"},
		"input[0].content[0]",
	)
	if !claimed {
		t.Fatal("must claim input_image type")
	}
	if len(cands) != 0 {
		t.Errorf("must produce no candidates (URL-only), got %d", len(cands))
	}
}

func TestEndsWith(t *testing.T) {
	if !endsWith("a.b.c", ".c") {
		t.Error("a.b.c ends with .c")
	}
	if endsWith("a.b.c", ".d") {
		t.Error("a.b.c does not end with .d")
	}
	if endsWith("a", ".aa") {
		t.Error("path shorter than suffix cannot match")
	}
	if !endsWith("inlineData", "inlineData") {
		t.Error("equal strings end with themselves")
	}
}
