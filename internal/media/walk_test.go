package media

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2nd1st/api-log/internal/storage"
	"github.com/2nd1st/api-log/internal/trace"
)

// extractTest wraps Extract with a synthetic bucket derived from the
// Extractor's DataDir + the trace's TsStart date + a placeholder
// KeyHash8. Tests glob with `*` for the keyhash component so any
// 8-hex value works.
func extractTest(e *Extractor, tr trace.Trace) []MediaFile {
	bucket := storage.FileID{
		DataDir:  e.cfg.DataDir,
		Date:     tr.TsStart.UTC().Format("2006-01-02"),
		KeyHash8: "00000000",
	}
	return e.Extract(tr, bucket)
}

// b64 is the base64 of "hello-png-bytes". Decoded length: 15.
const helloB64 = "aGVsbG8tcG5nLWJ5dGVz"

// Tiny 1x1 PNG (89 bytes decoded). Useful as a "real" data: URL payload.
const tinyPNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABAQMAAAAl21bKAAAAA1BMVEX/AAAZ4gk3AAAAAXRSTlMAQObYZgAAAApJREFUCNdjYAAAAAIAAeIhvDMAAAAASUVORK5CYII="

func mkTrace(reqBody, respBody string) trace.Trace {
	t := trace.Trace{
		ID:      "01TEST00000000000000000000",
		TsStart: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
		TsEnd:   time.Date(2026, 5, 30, 12, 0, 1, 0, time.UTC),
		Req: trace.Body{
			Headers: trace.Headers(http.Header{"Authorization": []string{"Bearer sk-test"}}),
		},
		Resp: trace.Body{
			Headers: trace.Headers(http.Header{"Content-Type": []string{"application/json"}}),
		},
	}
	if reqBody != "" {
		t.Req.Body = json.RawMessage(reqBody)
	}
	if respBody != "" {
		t.Resp.Body = json.RawMessage(respBody)
	}
	return t
}

func TestExtract_EmptyBodies(t *testing.T) {
	dir := t.TempDir()
	e := New(Config{DataDir: dir})
	got := extractTest(e, mkTrace("", ""))
	if len(got) != 0 {
		t.Fatalf("expected 0 media, got %d: %+v", len(got), got)
	}
}

func TestExtract_BodyB64IsIgnored(t *testing.T) {
	// body_b64 lives in trace.Body.BodyB64, not in Body. We construct a
	// trace whose Body is nil but BodyB64 is set — extractor must NOT
	// peek at it.
	tr := mkTrace("", "")
	tr.Req.BodyB64 = helloB64
	tr.Resp.BodyB64 = helloB64
	e := New(Config{DataDir: t.TempDir()})
	got := extractTest(e, tr)
	if len(got) != 0 {
		t.Fatalf("body_b64 should be skipped entirely, got %d files: %+v", len(got), got)
	}
}

func TestExtract_NonJSONBodyIsSilentlySkipped(t *testing.T) {
	// Non-JSON in Body shouldn't crash the extractor (in practice capture
	// would have put it in body_b64, but be defensive).
	tr := mkTrace(`not json at all`, "")
	e := New(Config{DataDir: t.TempDir()})
	got := extractTest(e, tr)
	if len(got) != 0 {
		t.Fatalf("non-JSON should produce no candidates, got %+v", got)
	}
}

func TestExtract_OpenAIChat_DataURLImage(t *testing.T) {
	req := `{
	  "model": "gpt-4o",
	  "messages": [
	    {"role":"user","content":[
	      {"type":"text","text":"what is this"},
	      {"type":"image_url","image_url":{"url":"` + tinyPNGDataURL + `"}}
	    ]}
	  ]
	}`
	dir := t.TempDir()
	got := extractTest(New(Config{DataDir: dir}), mkTrace(req, ""))
	if len(got) != 1 {
		t.Fatalf("want 1 media, got %d: %+v", len(got), got)
	}
	m := got[0]
	if m.Side != "req" {
		t.Errorf("side = %q, want req", m.Side)
	}
	if m.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", m.MimeType)
	}
	if m.Extension != "png" {
		t.Errorf("ext = %q, want png", m.Extension)
	}
	if !strings.HasSuffix(m.SourceField, "image_url.url") {
		t.Errorf("path %q should end with image_url.url", m.SourceField)
	}
	if m.Idx != 0 {
		t.Errorf("idx = %d, want 0", m.Idx)
	}
	// File should exist on disk under data/<date>/<hash>/media/<id>/0.png.
	matches, _ := filepath.Glob(filepath.Join(dir, "2026-05-30", "*", "media", "01TEST00000000000000000000", "0.png"))
	if len(matches) != 1 {
		t.Errorf("expected exactly 1 file on disk, got %d (matches=%v)", len(matches), matches)
	}
	if len(matches) == 1 {
		info, err := os.Stat(matches[0])
		if err != nil {
			t.Errorf("stat: %v", err)
		} else if info.Size() != m.Size {
			t.Errorf("disk size %d != metadata size %d", info.Size(), m.Size)
		}
	}
}

func TestExtract_OpenAIChat_HTTPSImageURLSkipped(t *testing.T) {
	req := `{"messages":[{"role":"user","content":[
	  {"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}
	]}]}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 0 {
		t.Fatalf("https URL must be skipped, got %d: %+v", len(got), got)
	}
}

func TestExtract_OpenAIChat_InputAudio(t *testing.T) {
	req := `{"messages":[{"role":"user","content":[
	  {"type":"input_audio","input_audio":{"data":"` + helloB64 + `","format":"wav"}}
	]}]}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	m := got[0]
	if m.MimeType != "audio/wav" {
		t.Errorf("mime %q, want audio/wav", m.MimeType)
	}
	if m.Extension != "wav" {
		t.Errorf("ext %q, want wav", m.Extension)
	}
	if !strings.HasSuffix(m.SourceField, "input_audio.data") {
		t.Errorf("path %q should end with input_audio.data", m.SourceField)
	}
}

func TestExtract_Anthropic_ImageAndDocument(t *testing.T) {
	req := `{
	  "messages":[
	    {"role":"user","content":[
	      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + helloB64 + `"}},
	      {"type":"text","text":"look"},
	      {"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + helloB64 + `"}}
	    ]}
	  ]
	}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(got), got)
	}
	// Idx 0 = image, idx 1 = document (document order in the content array).
	if got[0].MimeType != "image/png" || got[0].Extension != "png" {
		t.Errorf("first: mime=%q ext=%q", got[0].MimeType, got[0].Extension)
	}
	if got[1].MimeType != "application/pdf" || got[1].Extension != "pdf" {
		t.Errorf("second: mime=%q ext=%q", got[1].MimeType, got[1].Extension)
	}
	if got[0].Idx != 0 || got[1].Idx != 1 {
		t.Errorf("idx ordering broken: %d %d", got[0].Idx, got[1].Idx)
	}
	if !strings.HasSuffix(got[0].SourceField, "source.data") {
		t.Errorf("path %q should end with source.data", got[0].SourceField)
	}
}

func TestExtract_Anthropic_URLSourceSkipped(t *testing.T) {
	req := `{"messages":[{"role":"user","content":[
	  {"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}
	]}]}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 0 {
		t.Fatalf("URL source must be skipped, got %d: %+v", len(got), got)
	}
}

func TestExtract_Anthropic_TextSourceSkipped(t *testing.T) {
	// type=text means plaintext document content — not a binary attachment.
	req := `{"messages":[{"role":"user","content":[
	  {"type":"document","source":{"type":"text","data":"plain text content here"}}
	]}]}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 0 {
		t.Fatalf("text source must be skipped, got %d: %+v", len(got), got)
	}
}

func TestExtract_Gemini_InlineData_CamelCase(t *testing.T) {
	req := `{
	  "contents":[
	    {"parts":[
	      {"text":"describe"},
	      {"inlineData":{"mimeType":"image/jpeg","data":"` + helloB64 + `"}}
	    ]}
	  ]
	}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].MimeType != "image/jpeg" || got[0].Extension != "jpg" {
		t.Errorf("mime/ext = %q/%q", got[0].MimeType, got[0].Extension)
	}
	if !strings.HasSuffix(got[0].SourceField, "inlineData.data") {
		t.Errorf("path %q should end with inlineData.data", got[0].SourceField)
	}
}

func TestExtract_Gemini_InlineData_SnakeCase(t *testing.T) {
	// Some SDKs (python-genai older versions) emit snake_case aliases.
	req := `{
	  "contents":[
	    {"parts":[{"inline_data":{"mime_type":"image/png","data":"` + helloB64 + `"}}]}
	  ]
	}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 1 {
		t.Fatalf("snake_case alias should still extract; got %d", len(got))
	}
	if got[0].MimeType != "image/png" {
		t.Errorf("mime = %q", got[0].MimeType)
	}
}

func TestExtract_Gemini_SystemInstruction(t *testing.T) {
	req := `{
	  "systemInstruction":{"parts":[
	    {"inlineData":{"mimeType":"image/webp","data":"` + helloB64 + `"}}
	  ]},
	  "contents":[{"parts":[{"text":"go"}]}]
	}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if !strings.HasPrefix(got[0].SourceField, "systemInstruction.") {
		t.Errorf("path %q should start with systemInstruction.", got[0].SourceField)
	}
	if got[0].Extension != "webp" {
		t.Errorf("ext = %q, want webp", got[0].Extension)
	}
}

func TestExtract_Gemini_FileDataSkipped(t *testing.T) {
	req := `{"contents":[{"parts":[
	  {"fileData":{"mimeType":"image/png","fileUri":"gs://bucket/x.png"}}
	]}]}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 0 {
		t.Fatalf("fileData (URL-only) must be skipped, got %d", len(got))
	}
}

func TestExtract_Responses_InputImageURLSkipped(t *testing.T) {
	req := `{
	  "input":[
	    {"type":"message","role":"user","content":[
	      {"type":"input_image","image_url":"https://example.com/x.png"}
	    ]}
	  ]
	}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 0 {
		t.Fatalf("input_image URL must be skipped, got %d: %+v", len(got), got)
	}
}

func TestExtract_OrderingReqBeforeResp(t *testing.T) {
	req := `{"messages":[{"role":"user","content":[
	  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + helloB64 + `"}}
	]}]}`
	resp := `{"content":[
	  {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + helloB64 + `"}}
	]}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, resp))
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].Side != "req" || got[1].Side != "resp" {
		t.Errorf("ordering broken: %s / %s", got[0].Side, got[1].Side)
	}
	if got[0].Idx != 0 || got[1].Idx != 1 {
		t.Errorf("idx ordering: %d %d", got[0].Idx, got[1].Idx)
	}
}

func TestExtract_NoDataDir_StillReturnsMetadata(t *testing.T) {
	// Test mode: empty DataDir means "don't write files but still detect".
	req := `{"messages":[{"role":"user","content":[
	  {"type":"image_url","image_url":{"url":"` + tinyPNGDataURL + `"}}
	]}]}`
	got := extractTest(New(Config{DataDir: ""}), mkTrace(req, ""))
	if len(got) != 1 {
		t.Fatalf("want 1 metadata even with no DataDir, got %d", len(got))
	}
	if got[0].Size == 0 {
		t.Errorf("size should be set from decoded bytes")
	}
}

func TestExtract_FilePathFormat(t *testing.T) {
	req := `{"messages":[{"role":"user","content":[
	  {"type":"image_url","image_url":{"url":"` + tinyPNGDataURL + `"}}
	]}]}`
	got := extractTest(New(Config{DataDir: t.TempDir()}), mkTrace(req, ""))
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	want := "media/01TEST00000000000000000000/0.png"
	if got[0].Filename != want {
		t.Errorf("filename = %q, want %q", got[0].Filename, want)
	}
}

func TestExtract_IdempotentReextract(t *testing.T) {
	// Running Extract twice on the same trace should write the same bytes
	// to the same path and not error (failure should not block the writer;
	// rerun is the recovery path).
	req := `{"messages":[{"role":"user","content":[
	  {"type":"image_url","image_url":{"url":"` + tinyPNGDataURL + `"}}
	]}]}`
	dir := t.TempDir()
	e := New(Config{DataDir: dir})
	tr := mkTrace(req, "")
	first := extractTest(e, tr)
	second := extractTest(e, tr)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("expected 1+1, got %d+%d", len(first), len(second))
	}
	if first[0].Filename != second[0].Filename {
		t.Errorf("filename changed across re-extract: %q vs %q", first[0].Filename, second[0].Filename)
	}
}
