# Phase I Export Contract: GET /api/export

## Endpoint Specification

### Request

**Method**: `GET`  
**Path**: `/api/export`  
**Content-Type**: `application/json` (responses only)

### Query Parameters

All query parameters are **optional** and follow the same filtering semantics as `GET /api/traces`:

| Parameter | Type | Description |
|-----------|------|-------------|
| `status` | int | HTTP status code to filter (e.g., `status=500`) |
| `model` | string | LLM model name substring match |
| `key_hash` | string | API key hash fingerprint (exact match, multiple allowed) |
| `session_root_id` | string | Root session identifier (exact match) |
| `since` | RFC3339 | Start of time range (inclusive); e.g., `2025-01-15T10:30:00Z` |
| `until` | RFC3339 | End of time range (inclusive); e.g., `2025-01-16T10:30:00Z` |
| `path` | string | URL path substring match (e.g., `/v1/chat/completions`) |
| `limit` | int | Maximum number of matching traces to include in export (default: unlimited, no upper bound — export streams, memory is not the bottleneck; operators may pass any positive integer as an advisory cap) |

**Notes on filtering**:
- Multiple `key_hash` or `session_root_id` parameters are allowed (OR logic within same field, AND logic between fields)
- Filters are applied in combination: all must match for a trace to be included
- Empty filter set returns all traces (bounded only by `limit` if the operator supplies one)

### Authentication

**Header**: `Authorization: Bearer <admin-token>`

- Same admin token requirement as the rest of the api-log API
- 401 Unauthorized if token is missing or invalid
- 403 Forbidden if token lacks admin scope

---

## Response

### Success (200 OK)

**Content-Type**: `application/zip`  
**Content-Disposition**: `attachment; filename="api-log-export-<YYYY-MM-DDTHHMMSSMMMZ>.zip"`

Where `<YYYY-MM-DDTHHMMSSMMMZ>` is the current UTC timestamp at generation time (ISO 8601, including milliseconds for uniqueness).

#### Zip File Layout

```
api-log-export-<timestamp>.zip
├── data/
│   ├── 2025-01-15/
│   │   ├── abc123def456.jsonl
│   │   ├── xyz789.partial.jsonl
│   │   └── [one file per unique key_hash on that date]
│   ├── 2025-01-16/
│   │   └── abc123def456.jsonl
│   └── [one directory per calendar day with matching traces]
├── agent/
│   ├── CLAUDE.md
│   └── jq-cheatsheet.md
└── README.md
```

##### `data/` Directory Structure

- **By calendar day**: Traces are grouped into directories by their UTC date (YYYY-MM-DD)
- **By key_hash**: Within each day, one JSONL file per unique API key hash
  - **Complete files**: Named `<keyhash>.jsonl` if all traces for that key on that day match the filter
  - **Partial files**: Named `<keyhash>.partial.jsonl` if the source day has both matching and non-matching traces; this file contains only matching lines with order and whitespace preserved
- **JSONL format**: One valid JSON object per line (newline-delimited); no trailing newline required on the last line
  - Line order preserved from query result
  - Original whitespace in JSON preserved (no pretty-printing; compact form)
  - Each line is a complete HTTP transaction (see schema below)

##### `agent/` Directory

- **CLAUDE.md**: Bundled instruction file (identical template; see contract doc #2)
- **jq-cheatsheet.md**: Common jq patterns for analyzing the data (see contract doc #3)

##### `README.md`

Human-readable summary (≤1 page):
- What this export contains (number of traces, date range, filters applied)
- How to explore the data (structure, key_hash meaning, partial vs. complete files)
- Quick start: "Use jq or any JSON tool to parse the JSONL files"
- Link to the api-log repository

---

## Response Schema (JSONL)

Each line in `data/**/*.jsonl` is a single HTTP transaction. Field names and types:

```json
{
  "id": "uuid-string",
  "ts_start": "2025-01-15T10:30:45.123Z",
  "ts_end": "2025-01-15T10:30:46.456Z",
  "client": "your-app-name",
  "method": "POST",
  "path": "/v1/chat/completions",
  "status": 200,
  "req": {
    "headers": { "content-type": "application/json", "authorization": "Bearer ..." },
    "body": { /* JSON object */ },
    "body_b64": null
  },
  "resp": {
    "headers": { "content-type": "application/json" },
    "body": { /* JSON object */ },
    "body_b64": null,
    "events": [
      { "type": "chunk", "ts": "2025-01-15T10:30:45.200Z", "data": "..." }
    ]
  },
  "key_hash": "sha256:abc123def456...",
  "parent_id": "uuid-string or null",
  "session_root_id": "uuid-string or null"
}
```

**Field Descriptions**:
- `id`: Unique trace ID
- `ts_start`, `ts_end`: UTC timestamps (ISO 8601 with milliseconds)
- `client`: Name/identifier of the client that made the request
- `method`: HTTP method (GET, POST, etc.)
- `path`: Full request path including query string
- `status`: HTTP response status code
- `req`: Request metadata
  - `headers`: Request headers (dict)
  - `body`: Parsed JSON body (if Content-Type: application/json and successfully parsed)
  - `body_b64`: Base64-encoded body (if body is binary or unparseable)
- `resp`: Response metadata (same structure as `req`)
  - `events`: Array of streaming chunks (if response was streamed)
- `key_hash`: SHA256 hash of the API key used (stable identifier for session grouping)
- `parent_id`: ID of the previous trace in a chain (for multi-turn conversation reconstruction)
- `session_root_id`: Trace ID of the root request in the session chain

---

## Error Responses

### 400 Bad Request

Returned if any filter parameter is invalid.

**Response Body**:
```json
{
  "error": "invalid_filter",
  "message": "status must be an integer",
  "field": "status"
}
```

**Common cases**:
- `status` is not a valid integer
- `since` or `until` is not a valid RFC3339 timestamp
- `limit` is not a positive integer (no upper bound is enforced)

### 401 Unauthorized

Returned if the `Authorization` header is missing or invalid.

**Response Body**:
```json
{
  "error": "unauthorized",
  "message": "missing or invalid bearer token"
}
```

### 403 Forbidden

Returned if the token lacks admin scope.

**Response Body**:
```json
{
  "error": "forbidden",
  "message": "token does not have admin scope"
}
```

### 500 Internal Server Error

Returned if the zip cannot be generated (disk read failure, etc.).

**Response Body**:
```json
{
  "error": "export_failed",
  "message": "failed to read trace data: disk I/O error"
}
```

### 200 OK (Empty Export)

Even if zero traces match the filter, return HTTP 200 with an empty zip file. The zip still contains:
- `agent/CLAUDE.md`
- `agent/jq-cheatsheet.md`
- `README.md` (updated to reflect "0 traces matched")

No `data/` directory is created if there are no matches.

---

## Implementation Notes

1. **Streaming vs. Buffering**: The zip can be streamed to the client if the underlying JSONL files are enumerated first. Buffering the entire export in memory is acceptable if total size is < 100 MB; for larger exports, consider streaming.

2. **Date Bucketing**: The export groups by UTC calendar day. A single day's file is considered "complete" iff all traces for that `(day, key_hash)` pair match the filter. If some traces on that day don't match, the file is renamed to `.partial.jsonl`.

3. **Whitespace Preservation**: When writing JSONL, do **not** re-encode or pretty-print the original JSON. If the source trace was compact, output compact. If it had line breaks, preserve them (though JSONL by definition is line-separated, so this is for intra-line content).

4. **Timestamp Format**: Always use ISO 8601 with UTC timezone (`Z` suffix) and millisecond precision.

5. **Filename Collisions**: If two exports are generated in the same millisecond, add a nonce or increment (e.g., `-001`, `-002`) to the filename to ensure uniqueness.

---

## Example Request

```bash
curl -H "Authorization: Bearer admin-token-xyz" \
  "https://api.example.com/api/export?status=500&since=2025-01-15T00:00:00Z&until=2025-01-16T23:59:59Z" \
  -o export.zip
```

---

## Example Response Headers

```
HTTP/1.1 200 OK
Content-Type: application/zip
Content-Disposition: attachment; filename="api-log-export-2025-01-16T10:30:45.123Z.zip"
Content-Length: 45821
Cache-Control: no-store
```

