# Phase K: Media Extraction Contract Specification

**Date:** 2026-05-30  
**Status:** Implementation contract for all backend agents  
**Scope:** Deterministic media file extraction from API request/response bodies per protocol

---

## 1. Protocol Field Coverage Table

Media can appear in the following named protocol fields across the 4 supported adapters (chat, messages, responses, gemini). This table enumerates every field where extraction must happen.

### 1.1 OpenAI Chat Completions (`/v1/chat/completions`)

| Side | Field Path | Field Name | Type | Example |
|------|-----------|-----------|------|---------|
| **REQ** | `req.body.messages[i].content[j].image_url.url` | Inline image URL (data or https) | URL or data:image | `data:image/png;base64,iVBORw0KGgo…` or `https://example.com/img.jpg` |
| **REQ** | `req.body.messages[i].content[j].input_audio.data` | Inline audio base64 | base64 | `"iZmlsZS9hdWRpby93YXY="` |
| **REQ** | `req.body_b64` | Fallback request body | base64-JSON | Rare; only if req.body is not JSON-decodable |
| **RESP** | `resp.body_b64` | Fallback response body | base64-JSON | ~50% of chat traffic; unparseable SSE wrapped in b64 |

**Notes:**
- Chat response (`resp.body.choices[].message.content`) contains text/tool_call, NOT media.
- `body_b64` fallback is used when upstream fails JSON parsing and wraps raw bytes.
- Image URL extraction: if `url` starts with `data:`, extract base64 part; if https, log as URL-only (not extracted per §5.2).

---

### 1.2 Anthropic Messages (`/v1/messages`)

| Side | Field Path | Field Name | Type | Example |
|------|-----------|-----------|------|---------|
| **REQ** | `req.body.messages[i].content[j].type="image"` | Image content block | JSON object | `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"…"}}` |
| **REQ** | `req.body.messages[i].content[j].type="document"` | Document content block | JSON object | `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"…"}}` |
| **REQ** | `req.body.messages[i].content[j].source.data` | Base64 binary for image/document | base64 | (within image/document block) |
| **REQ** | `req.body.messages[i].content[j].source.url` | URL reference (file_id or https) | URL | (within image/document block) |
| **RESP** | `resp.body.content[i].type="image"` | Response image block | JSON object | Same shape as request |
| **RESP** | `resp.body.content[i].type="document"` | Response document block | JSON object | Same shape as request |

**Notes:**
- Images and documents both use the `.source` indirection with `.type` discriminator.
- `.source.type` can be: `"base64"` (→ extract `data` field), `"url"` (→ skip, URL-only), `"text"` (document plaintext, not extractable as binary), `"content"` (document with inner Parts array, only for nested structures).
- `media_type` field on source is the MIME type; use it if present.

---

### 1.3 OpenAI Responses (`/v1/responses`)

| Side | Field Path | Field Name | Type | Example |
|------|-----------|-----------|------|---------|
| **REQ** | `req.body.input[i].type="message"` | Message item | JSON array | `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"..."}]}` |
| **REQ** | `req.body.input[i].content[j].type="input_image"` | Input image reference | JSON | `{"type":"input_image","image_url":"https://…"}` |
| **REQ** | `req.body.input[i].content[j].image_url` | Image URL (bare string or object) | URL or string | `"https://…"` or `{"url":"https://…"}` |
| **RESP** | `resp.events[k].data.item` (type="message") | SSE: message item | JSON | Streamed in `output_item.added` and finalized in `output_item.done` |

**Notes:**
- Responses protocol uses SSE events, not a single body. No media in event streaming deltas; media is attached to the final item shape.
- `input_image` in request is URL-only; skip extraction per §5.2.
- Response message items do not carry media in this protocol (assistant outputs text/tool_calls, not images).

---

### 1.4 Google Gemini (`/v1beta/models/*:generateContent` and SSE)

| Side | Field Path | Field Name | Type | Example |
|------|-----------|-----------|------|---------|
| **REQ** | `req.body.systemInstruction.parts[i].inlineData` | Inline system media | JSON | `{"mimeType":"image/png","data":"base64_string"}` |
| **REQ** | `req.body.systemInstruction.parts[i].fileData` | File reference in system | JSON | `{"mimeType":"image/png","fileUri":"gs://…"}` |
| **REQ** | `req.body.contents[i].parts[j].inlineData` | Inline message media | JSON | `{"mimeType":"image/jpeg","data":"base64_…"}` |
| **REQ** | `req.body.contents[i].parts[j].fileData` | File reference in message | JSON | `{"mimeType":"image/jpeg","fileUri":"gs://…"}` |
| **RESP** | `resp.body.candidates[0].content.parts[i].inlineData` | Inline response media | JSON | (rare; Gemini doesn't emit base64 in response) |
| **RESP** | `resp.events[k].data.candidates[0].content.parts[i].inlineData` | SSE: inline media | JSON | (same as non-streaming) |

**Notes:**
- `inlineData.data` is base64 string (extract).
- `fileData.fileUri` is URL reference (skip, no local content).
- Gemini camelCase field names; some SDKs use snake_case aliases (e.g., `inline_data`, `file_data`).
- Response media is rare and when present is usually in tool results, not main content.

---

## 2. Skip List with Rationale

The following MUST NOT be extracted:

| Shape | Reason | Reference |
|-------|--------|-----------|
| `body_b64` in request/response when it is the ONLY body (no parsed JSON) | Unparseable JSON container; not an attachment per operator decision 2026-05-30 | Backend §2 |
| `https://…` URLs (image_url.url, fileData.fileUri, etc.) | No local content to extract; remote fetch is out of scope | Backend §1 |
| `file:` URIs (Anthropic file references) | File is on remote provider's server; we don't have it | Implicit |
| Plaintext document content (Anthropic `source.type="text"`) | Not binary; no extraction needed | Design constraint |
| OpenAI Responses `input_image.image_url` | URL-only reference; client-side image, not in trace body | Request shape |
| Empty media fields (null, "", or whitespace) | Noise; skip silently | Hygiene |
| Media in error responses (e.g., `error.body`) | Error payloads are not model content | Protocol semantics |

---

## 3. MediaFile Go Struct

The extractor returns one struct per extracted media file:

```go
type MediaFile struct {
  // 0-based ordinal of appearance in body (across all protocols).
  // Order: all request media first (in document order), then response media.
  Idx int

  // "req" or "resp" — which side of the trace contained this media.
  Side string

  // Full path in the protocol hierarchy where this media came from.
  // Examples:
  //   - "messages[2].content[0].image_url.url"
  //   - "contents[1].parts[3].inlineData.data"
  //   - "input[4].content[1].image_url"
  SourceField string

  // MIME type (e.g. "image/png", "application/pdf").
  // Always filled from the protocol's mimeType/media_type field if present,
  // or guessed from data:image header, or defaulted to "application/octet-stream".
  // NEVER inferred from a filename alone.
  MimeType string

  // File extension derived ONLY from MimeType (.png, .jpg, .pdf, .wav, etc.).
  // Must be lowercase, no leading dot in this field (used as `${idx}.${ext}`).
  // Never trust filename input from the protocol.
  Extension string

  // Decoded byte length of the extracted file (base64 decoded, or raw bytes).
  Size int64

  // Relative path within data dir where the file was written.
  // Format: `media/${trace_id}/${idx}.${ext}`
  // The full disk path becomes: `${data_dir}/${YYYY-MM-DD}/${keyhash_short}/media/${trace_id}/${idx}.${ext}`
  Filename string
}
```

---

## 4. On-Disk Layout

**Concrete path convention:**

```
${data_dir}/${YYYY-MM-DD}/${keyhash_short}/media/${trace_id}/${idx}.${ext}
```

**Worked example:**

Trace `01KSS8E35ZGK4SDH9E63H8HYA1` recorded on 2026-05-29 with keyhash `e461a233`:
- Request message[0].content[0] is a PNG image (base64)
- Request message[1].content[2] is a PDF document (base64)
- Response contains no extractable media

**Result files on disk:**

```
/data/2026-05-29/e461a233/media/01KSS8E35ZGK4SDH9E63H8HYA1/0.png    (request image)
/data/2026-05-29/e461a233/media/01KSS8E35ZGK4SDH9E63H8HYA1/1.pdf    (request document)
```

MediaFile records returned by extractor:

```json
[
  {
    "idx": 0,
    "side": "req",
    "sourceField": "messages[0].content[0].image_url.url",
    "mimeType": "image/png",
    "extension": "png",
    "size": 5432,
    "filename": "media/01KSS8E35ZGK4SDH9E63H8HYA1/0.png"
  },
  {
    "idx": 1,
    "side": "req",
    "sourceField": "messages[1].content[2].document.source.data",
    "mimeType": "application/pdf",
    "extension": "pdf",
    "size": 123456,
    "filename": "media/01KSS8E35ZGK4SDH9E63H8HYA1/1.pdf"
  }
]
```

---

## 5. Endpoint Contracts

### 5.1 GET /api/media/{trace_id}/{idx}

**Purpose:** Stream a single extracted media file.

**Request:**
```http
GET /api/media/01KSS8E35ZGK4SDH9E63H8HYA1/0
Authorization: Bearer <auth_token>
```

**Response (success, 200):**
```http
HTTP/1.1 200 OK
Content-Type: image/png
Content-Length: 5432
Cache-Control: public, immutable

<binary PNG data>
```

**Response (not found, 404):**
```http
HTTP/1.1 404 Not Found
Content-Type: application/json

{"error": "no extracted media for trace_id=01KSS8E35ZGK4SDH9E63H8HYA1, idx=0"}
```

**Auth:** Same as `/api/traces/{id}` — bearer token required.

**Notes:**
- Content-Type is determined from the MediaFile.mimeType.
- If the file doesn't exist on disk (e.g., trace was recorded with `save_attachments=false`), return 404.

---

### 5.2 GET /api/config/media

**Purpose:** Fetch the current media extraction configuration.

**Request:**
```http
GET /api/config/media
Authorization: Bearer <auth_token>
```

**Response:**
```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "save_attachments": true,
  "source": "yaml"
}
```

**Response fields:**
- `save_attachments` (bool): whether media extraction is enabled.
- `source` (string): one of `"default"` (hardcoded true), `"yaml"` (from config.yaml), `"override"` (from runtime_overrides.json), or `"env"` (from MEDIA_SAVE_ATTACHMENTS env var). Tells operator which layer is active.

---

### 5.3 PUT /api/config/media

**Purpose:** Update media extraction configuration at runtime (persists to disk).

**Request:**
```http
PUT /api/config/media
Content-Type: application/json
Authorization: Bearer <auth_token>

{
  "save_attachments": false
}
```

**Response:**
```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "save_attachments": false,
  "source": "override"
}
```

**Behavior:**
- Atomically write the request body to `${data_dir}/runtime_overrides.json` (write temp file + rename).
- Update in-memory config so the change takes effect immediately for subsequent traces.
- Return the new config state including `source: "override"`.
- On error (e.g., permission denied), return 500 with error details.

**Notes:**
- Changes do NOT retroactively extract media from traces recorded before the config change.
- `save_attachments: false` stops extraction for new traces; existing files on disk remain.

---

## 6. Runtime Overrides Schema

**File location:**
```
${data_dir}/runtime_overrides.json
```

**File format (JSON):**
```json
{
  "media": {
    "save_attachments": true
  }
}
```

**Load order (lowest to highest precedence):**
1. Hardcoded default: `save_attachments: true`
2. `config.yaml` (if present)
3. Environment variable `MEDIA_SAVE_ATTACHMENTS` (if set)
4. `runtime_overrides.json` (if present) — loads at startup AFTER YAML/env so it overrides them
5. PUT /api/config/media (updates the file and in-memory config)

**Atomicity:** File is written via atomic write-temp-then-rename so readers never see partial JSON.

---

## 7. SQLite Schema Change

**Alteration:**

```sql
ALTER TABLE traces ADD COLUMN media_count INTEGER DEFAULT 0;
```

**Constraints:**
- Use `IF NOT EXISTS` or catch duplicate column errors (idempotent).
- Default value is 0; filled at finalize time (see §9).
- Allows fast filtering queries: `SELECT * FROM traces WHERE media_count > 0`.

**Backfill:** Not required; NULL/0 for existing rows is acceptable.

---

## 8. Default Config

**Backend decision (2026-05-30):** `media.save_attachments: true`

- Media extraction is ON by default.
- Operators can disable it per trace via PUT /api/config/media.
- For high-volume deployments, operators may set MEDIA_SAVE_ATTACHMENTS=false in the environment.

---

## 9. Bundle Export (WriteZip in Exporter)

**Integration point:** When the exporter's WriteZip function iterates over result rows:

```go
for _, row := range rows {
  // ... export trace JSONL and metadata ...
  
  // NEW: walk media directory for this trace
  mediaDir := filepath.Join(
    dataDir, 
    row.TsStart.Format("2006-01-02"),
    row.KeyHash[:8],
    "media",
    row.ID,
  )
  
  if dir, err := os.Open(mediaDir); err == nil {
    defer dir.Close()
    entries, _ := dir.Readdirnames(-1)
    for _, name := range entries {
      srcPath := filepath.Join(mediaDir, name)
      dstPath := filepath.Join("media", row.ID, name) // inside zip
      // ... add srcPath to zip at dstPath ...
    }
  }
  // Skip silently if directory missing (trace had save_attachments=false)
}
```

**Zip structure:**
```
export.zip
├── traces/
│   ├── 01KSS8E35ZGK4SDH9E63H8HYA1.jsonl
│   └── ...
├── media/
│   ├── 01KSS8E35ZGK4SDH9E63H8HYA1/
│   │   ├── 0.png
│   │   ├── 1.pdf
│   │   └── ...
│   └── ...
└── metadata.json
```

**Notes:**
- Exporter checks for media directory existence; if absent, continues silently.
- No extraction happens at export time; all files are pre-extracted at finalize.
- Bundle must remain <80 KB gzip (with media files included).

---

## 10. Performance Notes

**Extraction timing:**

- **Synchronous context:** Extraction runs immediately after JSONL append, in the finalize goroutine (writer side).
- **Budget:** ~50ms p99 for typical request/response with 1-3 images.
- **Large payloads:** Image-gen traces with multi-MB base64 may take longer (acceptable; writer goroutine means it doesn't block forwarding to upstream).

**File I/O:**

- Base64 decode → file write is buffered (use bufio.Writer).
- Directory creation (`${YYYY-MM-DD}/${keyhash_short}/media/${trace_id}`) must be idempotent (mkdir -p or equivalent).

**Storage:**

- Baseline: 90 traces × ~500 KB average media = ~45 MB per day.
- Configurable via `save_attachments` and can be disabled entirely.

---

## 11. Implementation Checklist

- [ ] **Extractor (Go):** Walk request/response per protocol, extract base64 blobs, write files, return MediaFile[]
- [ ] **SQLite:** Add `media_count` column (idempotent ALTER TABLE)
- [ ] **Config layer:** Load runtime_overrides.json after YAML/env; support PUT /api/config/media
- [ ] **Endpoints:** GET /api/media/{trace_id}/{idx}, GET /api/config/media, PUT /api/config/media
- [ ] **Exporter:** Walk media directories and bundle files in zip
- [ ] **Tests:** Round-trip samples from all 4 protocols; verify file paths, MIME types, idx ordering
- [ ] **Docs:** Update API reference with media endpoints

---

## 12. References

- Backend PHILOSOPHY §1, §2, §6: extraction is deterministic, non-blocking, filesystem-based
- Operator decision 2026-05-30: `body_b64` is not an attachment; `save_attachments: true` by default
- Viewer PHILOSOPHY §5: single accent design; no changes to media display UX beyond loading files from /api/media

