# API Log Export: What's Inside

## What This Is

You've received an export from **api-log**, a transparent HTTP recording proxy that sits between your application and LLM gateway APIs (OpenAI, Anthropic, etc.). This export contains a subset of recorded HTTP transactions filtered by your query criteria. Each transaction is a single request-response pair: what your app sent, what the LLM replied, when it happened, and which API key was used.

---

## Schema: One Line = One Transaction

Each JSONL file (`data/**/*.jsonl`) contains newline-delimited JSON. Each line is a complete HTTP transaction with this structure:

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "ts_start": "2025-01-15T10:30:45.123Z",
  "ts_end": "2025-01-15T10:30:46.456Z",
  "client": "my-app",
  "method": "POST",
  "path": "/v1/chat/completions",
  "status": 200,
  "req": {
    "headers": { "content-type": "application/json", "authorization": "Bearer sk-..." },
    "body": { "model": "gpt-4", "messages": [...] },
    "body_b64": null
  },
  "resp": {
    "headers": { "content-type": "application/json" },
    "body": { "choices": [{"message": {"content": "..."}}] },
    "body_b64": null,
    "events": []
  },
  "key_hash": "sha256:a1b2c3d4e5f6...",
  "parent_id": null,
  "session_root_id": "550e8400-e29b-41d4-a716-446655440001"
}
```

### Field Reference

| Field | Type | Meaning |
|-------|------|---------|
| `id` | UUID | Unique trace identifier |
| `ts_start` | ISO 8601 | When the request left your app (UTC) |
| `ts_end` | ISO 8601 | When the response arrived back (UTC) |
| `client` | string | Name of your app/service |
| `method` | string | HTTP method: `POST`, `GET`, etc. |
| `path` | string | Request path, e.g., `/v1/chat/completions` |
| `status` | int | HTTP response code (200, 401, 500, etc.) |
| `req` | object | Request sent |
| `req.headers` | object | Request headers (dict) |
| `req.body` | object or null | Parsed JSON body (if applicable) |
| `req.body_b64` | string or null | Base64-encoded body (if binary or unparseable) |
| `resp` | object | Response received |
| `resp.headers` | object | Response headers (dict) |
| `resp.body` | object or null | Parsed JSON response body |
| `resp.body_b64` | string or null | Base64-encoded response (if binary) |
| `resp.events` | array | Streaming chunks (SSE or similar); each has `type`, `ts`, `data` |
| `key_hash` | string | SHA256 hash of the API key used; stable session identifier |
| `parent_id` | UUID or null | ID of the previous request in a conversation chain |
| `session_root_id` | UUID or null | ID of the first request in this conversation chain |

---

## How to Navigate the Data

### By API Key (key_hash)

All transactions using the same API key have the same `key_hash`. This is a stable identifier even if the key is rotated offline. Use it to group transactions by "who used which key and when."

**Example**: Find all requests that used a specific key:
```bash
grep '"key_hash":"sha256:a1b2c3d4e5f6"' data/**/*.jsonl
```

### By Conversation (session_root_id)

If your app makes multiple chained requests (e.g., a multi-turn conversation), they share the same `session_root_id`. The `parent_id` field chains them:

```
Request A (id: X1, parent_id: null, session_root_id: ROOT)
  ↓
Request B (id: X2, parent_id: X1, session_root_id: ROOT)
  ↓
Request C (id: X3, parent_id: X2, session_root_id: ROOT)
```

Walk the chain by following `parent_id` → `id` links.

### By Endpoint (path)

The `path` field shows which API endpoint was called. Common examples:
- `/v1/chat/completions` — chat models
- `/v1/embeddings` — embedding models
- `/v1/images/generations` — image generation
- `/v1/models` — model listing

### Complete vs. Partial Files

- `data/2025-01-15/abc123.jsonl` — All traces for key `abc123` on Jan 15 matched your filter
- `data/2025-01-15/abc123.partial.jsonl` — Some but not all traces for key `abc123` on Jan 15 matched; this file has only the matching subset

### Attached Media (`media/` directory)

If a trace carried inline binary content — images in `image_url.url` data URIs, Anthropic `source.data` base64, Gemini `inlineData.data`, OpenAI `b64_json` — the bytes are extracted into a sibling `media/` tree at the top level of the export:

```
media/<trace_id>/<idx>.<ext>
```

- `<trace_id>` is the same `id` field on the JSONL line.
- `<idx>` is the 0-based ordinal of the media within the trace, in protocol document order (all request media first, then response media).
- `<ext>` is derived from the protocol's MIME type (`png`, `jpg`, `pdf`, `wav`, …); never from a filename.

To walk media alongside the JSONL for a given trace, read each line, take `id`, and list `media/<id>/`. Example:

```bash
# every PNG attached to a successful chat completion
jq -r 'select(.path=="/v1/chat/completions" and .status==200) | .id' data/**/*.jsonl \
  | while read id; do ls media/$id/*.png 2>/dev/null; done
```

Notes:
- `media/` is only present when at least one matching trace had extractable media. If you don't see it, no trace in this export had attachments (or extraction was disabled at capture time).
- `req.body_b64` / `resp.body_b64` are NOT attachments — they are an unparseable-body fallback that wraps raw JSON bytes. They stay inside the JSONL line and have no file in `media/`.
- URL-only references (`https://…`, `gs://…`, `file_id`) are not fetched; only inline base64 content is extracted.

---

## Quick Start: jq Recipes

The `jq-cheatsheet.md` file in this export contains copy-paste recipes for common questions:

- Extract all errors (HTTP status ≥ 500)
- Count requests per endpoint
- Reconstruct a conversation from one session
- Sum tokens across requests
- Find slowest requests
- Walk a conversation chain
- Extract images from responses

**Quick example** (find status 500 errors):
```bash
jq 'select(.status >= 500)' data/**/*.jsonl
```

---

## What is NOT Here

The export contains **raw transaction data only**. The following are computed downstream and not included:

- **Cost**: Token counts and pricing are not embedded; you compute cost using the `req.body` (model, tokens) and your rate card
- **Model family classification**: We record the model name from `req.body.model`; you classify it
- **Retry metadata**: If a request was retried, each attempt is a separate transaction; the export doesn't mark retries
- **Scoring or quality metrics**: No tags like "hallucination" or "off-topic"; those require evaluation systems
- **Aggregated statistics**: No sums, averages, or pre-computed reports; use jq or your analytics tool

---

## Where to Find More

- **api-log philosophy & design**: https://github.com/your-org/api-log
- **jq tutorial**: https://stedolan.github.io/jq/manual/
- **JSON spec**: https://www.json.org/

---

## Tips for Analysis

1. **Large exports**: If the export is >100 MB, use `jq` filters (run on disk) rather than loading everything into memory
2. **Streaming**: Process JSONL line-by-line; each line is a complete transaction
3. **Whitespace**: JSON in req/resp is compact; pretty-print with `jq .` if needed
4. **Timestamps**: Always in UTC (ISO 8601 with `Z` suffix); convert to your timezone as needed
5. **Sensitive data**: The export may contain API keys, user messages, or PII; handle as you would any production log

