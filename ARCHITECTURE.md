# Architecture

> This document describes the **backend** — the `api-log` proxy process. The frontend (`api-log-viewer`) is a separate project; its contract with the backend is the read API documented in §6.

---

## 1. Data model overview

Two storage layers, in strict order of authority:

```
┌──────────────────────────────────────────────────────────────────────┐
│  Layer 1: JSONL — SOURCE OF TRUTH                                     │
│    data/<UTC-date>/<key_hash[:8]>.jsonl                              │
│    One line per completed trace. Append-only. Daily rotation.         │
│    Each line: full request + full response, parsed into structured    │
│    JSON. Streaming responses become an `events` array.                │
│    Consumed by: tail/grep/jq pipelines, AI agents, the writer's       │
│    own session-inference lookup.                                      │
└──────────────────────────────────────────────────────────────────────┘
┌──────────────────────────────────────────────────────────────────────┐
│  Layer 2: SQLite — DERIVED CACHE                                      │
│    data/index.sqlite                                                  │
│    Mirrors JSONL columns + adds writer-extracted fields:              │
│    `model`, token counts, `parent_id`, `session_root_id`, `key_hash`. │
│    Filled synchronously by the writer goroutine at append time.       │
│    Deletable; rebuilt from JSONL. Rebuild cost scales linearly with   │
│    trace count (roughly minutes per million traces); see §9.          │
│    Consumed by: the read API, the frontend list view.                 │
└──────────────────────────────────────────────────────────────────────┘
```

**Invariants**:
- The writer goroutine appends one JSONL line **then** upserts one SQLite row, both in the same goroutine — no fan-out, no second consumer race.
- If SQLite is deleted, startup scans `data/**/*.jsonl{,.gz}` and rebuilds everything from scratch.
- If a JSONL line exists with no SQLite row, the rebuild adds the row.
- If a SQLite row exists with no JSONL line — this cannot happen; the JSONL is written first.

---

## 2. On-disk layout

```
data/
├── 2026-05-27/                                ← UTC date
│   ├── a1b2c3d4.jsonl                         ← today, one file per key_hash[:8]
│   ├── e5f6a7b8.jsonl
│   └── 00000000.jsonl                         ← traffic without Authorization
├── 2026-05-26/
│   ├── a1b2c3d4.jsonl.gz                      ← rotated + gzipped
│   ├── e5f6a7b8.jsonl.gz
│   └── ...
├── tmp/                                       ← in-flight capture buffers (§7.1)
│   ├── 01HX7K8MS....req.bin                   ← request body bytes, pre-finalize
│   └── 01HX7K8MS....resp.bin                  ← response body bytes, pre-finalize
│                                                wiped entirely on startup; not "truth"
├── index.sqlite                               ← derived cache
├── admin_token                                ← bearer token for the read API
└── api-log.yaml                               ← optional config
```

**`key_hash`** is defined uniformly as the first **16 hex characters** of `sha256(canonical_auth_string)`. The canonical auth string is whichever the request carried, preferred in this order: `Authorization` header value (including the `Bearer ` prefix or whatever the client sent), then `x-api-key` header value, then the empty string. Traffic with no auth header → all-zero hash. File names use the first 8 chars of the 16-char `key_hash` for brevity (`key_hash[:8]`); SQLite stores the full 16-char `key_hash`; the read-API filter `?key_hash=` accepts either an 8- or 16-char prefix.

**Rotation**: at UTC midnight, the current day's file is closed and `gzip`'d. The next request starts a new file under tomorrow's directory.

**Per-client per-day file** is the right granularity:
- one CC user's daily traffic = one file = easy to grep/jq.
- batch traffic from multiple clients spreads across files.
- file sizes self-limit (most users send < 100 MB/day even with heavy use).

### 2.x Why bodies are inline, not in sidecar files

Earlier design drafts stored bodies in per-trace `bodies/<id>/req.zst` files alongside a metadata JSONL. We moved to **inline bodies inside the JSONL line** deliberately:

- **Ecosystem standard**. Claude Code, Codex CLI, and most LLM observability backends use single-line-per-trace JSONL with parsed body inside the line. Matching that shape means our output is directly consumable by tools that already speak the format.
- **One file = one trace, fully self-contained**. `jq` over a JSONL file lets a human or an AI agent answer any question without joining JSONL metadata to a sidecar directory. `cat` of one line is one whole trace.
- **gzip dictionary effect**. Repeated system prompts, identical tool definitions, and similar `messages` prefixes across the day's traces compress extremely well under per-file gzip (the standard format we rotate into). Per-trace zstd files lose this cross-trace dictionary win.
- **No sidecar lifecycle**. No orphan `bodies/<id>/` directories to GC when a trace fails halfway, no two-step write protocol (write body file, then JSONL line referencing it), no cross-file consistency to enforce.
- **Line size is bounded**. The `max_body_bytes` cap (§11; default 32 MB per direction) keeps any single line under ~64 MB worst-case. `jq` and modern streaming JSON parsers handle this; a single huge line is not the failure mode.

The cost is that very large bodies inflate the JSONL line size. Per `max_body_bytes` this is bounded; if a real deployment is dominated by traffic where most bodies exceed the cap, the right answer is to raise the cap, not reintroduce sidecars.

---

## 3. JSONL line shape

One line per completed trace. Three structural variants based on the response body.

### Pragmatic absent-value sentinels

A few fields use sentinel values to encode "the protocol did not provide this" without forcing every consumer to special-case nulls. These are **encodings, not synthesis** — they live with the schema, not with derived semantics. New sentinels require an entry in this section.

| Field | Sentinel | Meaning |
|---|---|---|
| `status` | `-1` | Upstream never returned response headers (TCP error, timeout before headers). |
| `resp.events[].event` | `""` (empty string) | OpenAI Chat Completions data-only frames have no `event:` prefix on the wire. |
| `resp.events[].t_delta_ms` | `null` | Event was reconstructed from a body file rather than the live capture (e.g. during reparse). Live capture always sets this. |

Sentinels are listed in this table and nowhere else; if you see a magic value somewhere in the JSONL not in this table, that is a bug.

### 3.1 Common envelope

```json
{
  "id":           "01HX7K8MS...",                  ← ULID; lexically time-sortable
  "ts_start":     "2026-05-27T10:23:45.123Z",
  "ts_end":       "2026-05-27T10:23:46.357Z",
  "client":       "172.17.0.5:54321",              ← TCP peer of incoming conn
  "method":       "POST",
  "path":         "/v1/responses",
  "upstream":     "http://gateway:7860",
  "status":       200,
  "req":          { ... see 3.2 ... },
  "resp":         { ... see 3.3 ... },
  "disconnected": false,                           ← either side closed before clean end
  "truncated_req": false,                          ← request capture buffer overflowed
                                                     OR req body exceeded max_body_bytes
  "truncated_resp": false                          ← same for response
}
```

### 3.2 Request shape

Request always carries headers. Body is one of three forms:

```json
"req": {
  "headers": {
    "Authorization":  "Bearer sk-...",
    "Content-Type":   "application/json",
    "User-Agent":     "claude-cli/2.4.1",
    "anthropic-version": "2023-06-01"
  },
  "body": {                                        ← Content-Type: application/json
    "model":    "claude-sonnet-4-6",
    "messages": [...],
    "stream":   true,
    "metadata": {"user_id": "..."},
    "max_tokens": 8192
  }
}
```

Non-JSON content (rare; multipart upload, binary):

```json
"req": {
  "headers": {"Content-Type": "multipart/form-data; boundary=..."},
  "body_b64": "POIeqzWxN..."                       ← base64 of raw bytes
}
```

If a body that should have been JSON fails to parse (malformed JSON, gzip-decompression failure, encoding the parser does not know), we degrade gracefully and emit both fields:

```json
"req": {
  "headers": {...},
  "body_b64":    "<base64 of raw bytes after content-encoding decompression attempt>",
  "parse_error": "unexpected token at offset 184: '}'"
}
```

`body` is mutually exclusive with `body_b64`; `parse_error` only appears alongside `body_b64`.

### 3.3 Response shape

Non-streaming response:

```json
"resp": {
  "headers": {"Content-Type": "application/json", "X-Request-Id": "..."},
  "body": {
    "id":         "msg_c4ef71fc...",
    "type":       "message",
    "role":       "assistant",
    "content":    [...],
    "stop_reason": "end_turn",
    "usage":      {...}
  }
}
```

Streaming response (SSE):

```json
"resp": {
  "headers": {"Content-Type": "text/event-stream", "X-Request-Id": "..."},
  "events": [
    {"event": "message_start",        "data": {...},                          "t_delta_ms":   12},
    {"event": "content_block_start",  "data": {...},                          "t_delta_ms":   18},
    {"event": "content_block_delta",  "data": {"delta": {"text": "1"}, ...},  "t_delta_ms":  234},
    {"event": "content_block_delta",  "data": {"delta": {"text": ",2"}, ...}, "t_delta_ms":  456},
    {"event": "content_block_stop",   "data": {...},                          "t_delta_ms":  511},
    {"event": "message_delta",        "data": {"usage": {...}, ...},          "t_delta_ms":  524},
    {"event": "message_stop",         "data": {...},                          "t_delta_ms":  525}
  ],
  "stream_done": true                              ← saw clean terminator
                                                     (Anthropic `message_stop`,
                                                      OpenAI Responses `response.completed`,
                                                      OpenAI Chat `[DONE]`)
}
```

**`t_delta_ms`** is the milliseconds elapsed between `ts_start` (the trace's connection-accept time) and the moment the capture tee saw the first byte of that SSE event. It is the per-chunk arrival timing recorded on the live capture path. Together with `events[]` order, it lets `/api/traces/:id/replay` (§6.4) re-emit the stream at the original pace — the differentiator vs. SDK-based observability tools, which store a normalized post-hoc message and have no chunk-level timing to replay.

Same `events` structure for OpenAI Responses (`response.created`, `response.output_text.delta`, `response.completed`, ...) and Chat Completions (`data: {"choices": [...]}` repeated, terminated by `data: [DONE]`).

### 3.4 Field reference

| Field          | Type    | Notes |
|----------------|---------|-------|
| `id`           | ULID    | Trace ID, lexically time-sortable. |
| `ts_start`     | RFC3339 | Client connection accepted. |
| `ts_end`       | RFC3339 | Trace finalized. |
| `client`       | string  | `host:port` of incoming TCP. |
| `method`       | string  | HTTP method of the request. |
| `path`         | string  | `:path` + query. Tells you which protocol (`/v1/messages` / `/v1/responses` / `/v1/chat/completions`). |
| `upstream`     | string  | Gateway URL we forwarded to. |
| `status`       | int     | Response status; `-1` if upstream never returned headers. |
| `req.headers`  | object  | Outbound headers as constructed by `ReverseProxy` + our `Director`, immediately before they are serialized for the upstream socket. Standard hop-by-hop headers have been stripped by ReverseProxy; `X-Forwarded-*` suppression is per §10.4. Note: this is **not** byte-identical to what hits the socket — Go's `http.Transport` may still add framing headers (Content-Length, Transfer-Encoding) below this layer. |
| `req.body`     | object  | Parsed JSON body if `Content-Type: application/json` (after `Content-Encoding` decompression). Mutually exclusive with `req.body_b64`. |
| `req.body_b64` | string  | Base64 of the raw body bytes (post-`Content-Encoding` decompression if a known encoding) when JSON parse fails or content type is not JSON. Mutually exclusive with `req.body`. |
| `req.parse_error` | string | Reason a JSON parse failed; only appears with `req.body_b64`. |
| `resp.headers` | object  | As received from upstream, with `Content-Encoding` stripped if we decompressed the body (so `resp.body` and `resp.headers` are mutually consistent: the headers describe the bytes you see). |
| `resp.body`    | object  | Parsed JSON if non-streaming. Mutually exclusive with `resp.events` and `resp.body_b64`. |
| `resp.events`  | array   | Parsed SSE events if streaming. Each element is `{event, data, t_delta_ms}` where `event` is the SSE `event:` name (empty string for OpenAI Chat data-only frames), `data` is the parsed JSON of the `data:` line, and `t_delta_ms` is the ms elapsed from trace `ts_start` to the moment the capture tee saw the first byte of this event. Mutually exclusive with `resp.body`. |
| `resp.body_b64`, `resp.parse_error` | string | Same semantics as request side; used when response body is non-JSON or fails to parse. |
| `resp.stream_done` | bool | Only present when `resp.events` is set. True iff the captured stream ended on the protocol's clean terminator (Anthropic `message_stop`, OpenAI Responses `response.completed`, OpenAI Chat `data: [DONE]`). |
| `disconnected` | bool    | True if either side closed before the response stream's clean end. |
| `truncated_req` | bool   | True if request capture lost bytes — either the capture buffer overflowed under back-pressure (§7.2) **or** the request body exceeded `max_body_bytes`. |
| `truncated_resp` | bool  | Same for response. |

---

## 4. SQLite schema

v0 ships the minimum index set; new indexes are added only when a measured query justifies one.

```sql
CREATE TABLE traces (
  -- Layer 1 mirror (synced by writer goroutine at append time)
  id              TEXT PRIMARY KEY,
  ts_start        INTEGER NOT NULL,             -- unix ms
  ts_end          INTEGER NOT NULL,
  client          TEXT,
  method          TEXT,
  path            TEXT,
  upstream        TEXT,
  status          INTEGER,
  disconnected    INTEGER,                      -- 0/1
  truncated_req   INTEGER,                      -- 0/1
  truncated_resp  INTEGER,                      -- 0/1

  -- Protocol field copies (named protocol fields only — direct extraction, no body synthesis)
  model           TEXT,                         -- copied from req.body.model (or normalized for Responses)
  stream          INTEGER,                      -- copied from req.body.stream (1 / 0 / NULL if absent)
  prompt_tokens   INTEGER,                      -- copied from the protocol's named token field
  completion_tokens INTEGER,
  total_tokens    INTEGER,
  finish_reason   TEXT,                         -- copied from stop_reason / finish_reason

  -- Deterministic encodings of named values (carve-out: named header parsing for stable classification)
  key_hash              TEXT NOT NULL,          -- sha256(Authorization)[:16] — 16 hex chars; see §2 for canonical form
  prefix_len            INTEGER,                -- number of turns in this trace's session prefix; NULL if no session concept
  prefix_canonical_hash TEXT,                   -- sha256(canonicalize(this trace's session prefix))[:16]

  -- Cross-trace structural algorithm output (carve-out: prefix-canonical-hash session inference, §5)
  parent_id             TEXT,                   -- prior trace this one continues (FK to traces.id)
  session_root_id       TEXT NOT NULL,          -- root of the session (= id if no parent)

  -- Pointer back to JSONL for /api/traces/:id reads
  jsonl_path      TEXT NOT NULL,                -- e.g. "data/2026-05-27/a1b2c3d4.jsonl"
                                                -- (no .gz suffix; gzipped reads handled per §6.2)
  jsonl_offset    INTEGER NOT NULL              -- pre-write byte offset where the line starts
);

CREATE INDEX idx_ts             ON traces(ts_start DESC);
CREATE INDEX idx_key_ts         ON traces(key_hash, ts_start DESC);
CREATE INDEX idx_session_root   ON traces(session_root_id);
CREATE INDEX idx_prefix_hash    ON traces(key_hash, prefix_canonical_hash);  -- enables parent lookup without loading JSONL
```

**Additive schema changes after v0** (schema is append-only; new optional columns added via idempotent `ALTER TABLE ADD COLUMN`). Entries are listed in commit order; for the actual statement text see `internal/store/sqlite/sqlite.go` `migrate()`.

```sql
-- Phase K (commit 67142f9): media extraction. Count of extracted media
-- files per trace; 0 when save_attachments is off or the trace has no
-- media fields. Populated by the writer goroutine post-JSONL-append from
-- the length of internal/media.Extract()'s returned slice.
-- NOT NULL DEFAULT 0 — absence is genuinely zero, not unknown.
ALTER TABLE traces ADD COLUMN media_count INTEGER NOT NULL DEFAULT 0;

-- T3 (commit 49e55bb): usage extraction. Deterministic copies of named
-- protocol usage fields (named provider-reported counts) for cache hits,
-- cache-creation tokens, and reasoning tokens — emitted by Anthropic
-- Messages, OpenAI Responses, and (partially) Chat Completions. Nullable
-- (no DEFAULT) so a protocol that does not name the field stays NULL
-- rather than being conflated with a real zero.
ALTER TABLE traces ADD COLUMN cached_tokens          INTEGER;
ALTER TABLE traces ADD COLUMN cache_creation_tokens  INTEGER;
ALTER TABLE traces ADD COLUMN reasoning_tokens       INTEGER;

-- R5a (commit de44b28): client identification at finalize. Taxonomy-driven
-- ExtractClient parses request headers (chiefly User-Agent) into a stable
-- kind / version pair — e.g. `claude-code-desktop` / `1.9659.2`. Nullable
-- TEXT: a request whose headers do not match any taxonomy rule stays
-- NULL (no heuristic synthesis).
ALTER TABLE traces ADD COLUMN client_kind     TEXT;
ALTER TABLE traces ADD COLUMN client_version  TEXT;

-- W4.1 Phase 2 (commit 3c6503d): project context. Deterministic copy of
-- an operator-authored L2 system-prompt field — parser.ExtractProjectContext
-- parses the request body's system / instructions text for a project
-- name. Nullable TEXT so a trace with no project signal stays NULL,
-- distinct from a real empty string. Mirrors the viewer's promptSource.ts
-- so the derived column matches what the UI used to compute at render.
ALTER TABLE traces ADD COLUMN client_project  TEXT;
```

```sql
-- v0.1.1 (B2.1): storage-coordinator index. Backs the retention loop's
-- reconcileOrphans path (SELECT DISTINCT jsonl_path FROM traces) and
-- per-file eviction (DELETE FROM traces WHERE jsonl_path = ?). Without
-- this index a million-row store would have to table-scan on every
-- monitor tick. CREATE INDEX IF NOT EXISTS is idempotent — same shape
-- as the original schema indexes; no separate swallow block needed.
CREATE INDEX IF NOT EXISTS idx_jsonl_path ON traces(jsonl_path);
```

The migration runner runs each `ALTER` unconditionally and swallows the
specific `duplicate column` / `already exists` error so re-startup
against an existing database is idempotent. `CREATE INDEX IF NOT EXISTS`
is intrinsically idempotent and lives in the original schema block.
Any other error propagates.

Indexes deferred to a future when a query justifies them:
- `idx_status_ts`, `idx_model_ts`, `idx_parent` — currently no read-API path requires them.

SQLite operates in WAL mode for concurrent read access while the writer goroutine appends:

```text
DSN (per-connection, every conn the pool hands out):
  ?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)

Process-global (Exec at Open):
  PRAGMA synchronous = NORMAL;     -- index is rebuildable, so NORMAL is fine
  PRAGMA temp_store  = MEMORY;
```

Per-connection pragmas live in the DSN as of v0.1.1 (B2.2) so EVERY connection the pool hands out gets WAL mode + busy_timeout — not just whichever conn happens to serve the first `db.Exec("PRAGMA …")` at startup. Without DSN-embedded pragmas, parallel read traffic that opened a fresh conn could see SQLITE_BUSY.

`db.SetMaxOpenConns(10)` (bumped from 8 in v0.1.1) absorbs the storage monitor's reconcile sweep alongside writer + reader pressure.

---

## 5. Session inference

Goal: given an unordered stream of incoming traces, group them into **conversation trees** — each tree rooted at the first turn, with descendants being follow-ups, and siblings being forks (regenerate).

**"Session" here means prefix-chained turns on the wire, nothing more** — not "one task," not "one agent run," not "one user intent." Application-level grouping is a downstream consumer's job.

### 5.1 The session prefix

We build a normalized **session prefix** from the request body, per protocol:

| Protocol | Endpoint | Session prefix construction |
|---|---|---|
| OpenAI Chat Completions | `/v1/chat/completions` | `req.body.messages` directly (system message is already `messages[0]` if present). |
| Anthropic Messages | `/v1/messages` | `[{role:"__apilog_system__", content: req.body.system}] + req.body.messages` if `req.body.system` is set, else `req.body.messages`. The `__apilog_system__` role name is reserved — no real client will ever send it because Anthropic / OpenAI reject unknown roles — so collisions are not possible. The synthetic turn ensures two conversations sharing `messages[0]` but with different system prompts are distinct sessions. |
| OpenAI Responses | `/v1/responses` | `[{role:"__apilog_system__", content: req.body.instructions}]` (if `instructions` is set) followed by normalized `req.body.input`. Input normalization is intentionally narrow in v0: a string `input` becomes one user message; a homogeneous array of `{type:"message",role,content}` items maps to one message per item. Anything else (function-call items, file-reference items, mixed-type arrays) is hashed as the raw `input` value canonicalized — meaning two byte-identical raw inputs produce the same prefix hash, but the session-tree structure for those traces may be coarser than for chat-shaped traffic. |
| Other endpoints (`/v1/embeddings`, `/v1/images/*`, etc.) | various | No session prefix; the trace's `prefix_len` is NULL and `session_root_id = id`. |

`prefix_canonical_hash` (stored in SQLite) is `h(prefix)` where `h(x) = sha256(canonicalize(x))[:16]` and `canonicalize(x)` is JSON-encode `x` with sorted object keys, no extra whitespace, and `\uXXXX` escapes normalized to lowercase. So each trace stores the hash of *its own full session prefix* — not all sub-prefixes.

To find candidate parents for a new trace T, we compute the hash of T's prefix at each length `k ∈ [1, L_T-1]` (T's strict-prefix hashes) and look up the SQLite index — a single `IN`-clause query, no JSONL reads.

### 5.2 Algorithm

Stateless LLM protocols force the client to resend the full session prefix on every turn. That means **trace T's session prefix begins with trace C's full session prefix iff T is a continuation of C's conversation**.

```
For each new trace T with session prefix of length L_T:
  if L_T is NULL or 0:                                  # no session concept (embeddings, images, ...)
    T.parent_id        = NULL
    T.session_root_id  = T.id
    return

  # Compute T's strict-prefix hashes (lengths L_T-1, L_T-2, ..., 1).
  strict_prefix_hashes = [h(prefix[:k]) for k in (L_T-1, L_T-2, ..., 1)]

  # One SQL query finds any prior trace whose full prefix equals one of these.
  row = SELECT id, session_root_id, prefix_len
        FROM traces
        WHERE key_hash              = T.key_hash
          AND prefix_canonical_hash IN (strict_prefix_hashes)
        ORDER BY prefix_len DESC, ts_start DESC          -- longest match wins
        LIMIT 1

  if row:
    T.parent_id        = row.id
    T.session_root_id  = row.session_root_id
  else:
    T.parent_id        = NULL
    T.session_root_id  = T.id
```

The longest-prefix-then-most-recent ordering is the tiebreaker when multiple ancestors match. Cost: one indexed `IN`-clause query per trace; no JSONL reads needed.

### 5.3 Properties (verified by `experiments/session-inference/`)

- **Multi-turn conversation** → linear chain `T₁ → T₂ → T₃`.
- **Batch (N independent single-shot requests)** → N independent roots.
- **Batch + follow-up** → each follow-up attaches to its correct root.
- **Fork (regenerate)** → siblings under the same root.
- **Different `key_hash`** → never mixed even if `messages` are byte-identical.
- **Anthropic conversations with different `system`** → distinct roots, thanks to the system-as-virtual-turn-0 normalization in §5.1.
- **Cross-protocol** → algorithm body is protocol-agnostic; the per-protocol logic lives only in the §5.1 session-prefix construction.

### 5.4 Known limitation

Two independent conversations whose normalized session prefixes are byte-identical at some length k, and which both then add a follow-up at length k+2 — the algorithm cannot tell which root each follow-up belongs to and may attach both to the more recent of the two.

Mitigations deferred to v0.1+:
- Schema-aware tiebreaker: compare each candidate's recorded response with the new trace's prefix-turn at position `len(C.prefix)` (the assistant turn just before the new user message). This converts the ambiguity into a check that the *response* recorded against C matches what T sees as its prior turn.
- Manual viewer-side "split session" action.

In practice this is rare; v0 documents the limit and proceeds.

### 5.5 Cross-day long sessions

Daily JSONL rotation does **not** break long-running sessions.

- **SQLite is not partitioned by date.** A trace's `parent_id` / `session_root_id` lookups, and the `prefix_canonical_hash` IN-clause query, all hit a single `index.sqlite` regardless of which day the prior traces were written. A session that starts on day N and continues into day N+1 (and N+2, ...) chains correctly across the rotation boundary.
- **Rebuild from JSONL preserves cross-day chains** because rebuild scans files in chronological order: day N inserted before day N+1, so day N+1's session inference can see day N candidates.
- **One session physically spans multiple JSONL files.** `WHERE session_root_id = ?` returns rows whose `jsonl_path` values may be different dates. The viewer (or any consumer) opens each `jsonl_path` and seeks to `jsonl_offset` — there is no single-file requirement.
- **No time-window cutoff on candidate matching, by design.** The algorithm (§5.2) does not bound how old a candidate can be. Two distinct cases share this rule, and we want both behaviors:
  - **Long-lived intentional chains** — a daemon that emits heartbeat probes with the same system prompt + first user turn every day, a multi-day agent run, a saved-session client that resumes a conversation a week later. These *should* match across arbitrary age, and they do.
  - **Accidental cross-year collisions** — two unrelated traces from the same `key_hash` happen to produce byte-identical `prefix_canonical_hash` values. The collision space (sha256[:16] = 64 bits) makes this essentially impossible by accident.

  The trade-off is deliberate: any time-window cutoff would be a *semantic* judgment ("two-year-old prefix matches are probably wrong") that the protocol envelope does not name, and would silently break the long-lived-chain case. We take the rare collision risk over the broken-by-design case.

A consumer rendering "the full conversation" for a long-running session loads `n` JSONL lines from `n` different files — still a one-SQLite-query + `n` seeks operation, no joins, no scans.

Cross-day session continuity is reconstructed at read time via
`parent_id` traversal across `jsonl_path` values. ``SELECT ... WHERE
session_root_id = ?`` returns rows spanning multiple JSONL files and the
consumer opens each + seeks to `jsonl_offset`. No JSONL-boundary
special-casing in the writer; no "long-session compaction" pass
scheduled. The single SQLite `index.sqlite` is the join table.

### 5.6 Query patterns

All traces in one session (any depth, any branch):
```sql
SELECT * FROM traces WHERE session_root_id = ? ORDER BY ts_start;
```

Recursive descendant tree from a given trace:
```sql
WITH RECURSIVE descendants(id) AS (
  SELECT ?
  UNION ALL
  SELECT t.id FROM traces t JOIN descendants d ON t.parent_id = d.id
)
SELECT t.* FROM traces t JOIN descendants ON t.id = descendants.id;
```

---

## 6. Read API

Three endpoints. All require `Authorization: Bearer <token>` where `<token>` is the value at `data/admin_token` (auto-generated on first run; print to stdout once at startup; if the file is deleted, regenerate on next startup and print again).

```
GET  /api/traces?since=&until=&status=&model=&path=&key_hash=&session_root_id=&project=&limit=&cursor=    # §6.2
GET  /api/traces/:id                                                                                      # §6.3
GET  /api/traces/:id/replay                                                                               # §6.4
GET  /healthz                                                                                             # §6.5
GET  /                                                                                                    # §6.6
```

### 6.1 Auth and error contract

- Missing or wrong bearer → `401 Unauthorized` with body `{"error":"unauthorized"}`. Constant-time comparison.
- Malformed query params → `400 Bad Request` with `{"error":"...", "param":"<which>"}`.
- Unknown trace id → `404 Not Found`.
- Server error → `500` with `{"error":"<class>"}` (no stack traces in production).
- All responses are JSON unless explicitly noted.
- `Cache-Control: no-store` on all responses.

### 6.2 `GET /api/traces`

List, backed by SQLite only — never opens JSONL on this path. Returns the SQLite row shape: mirror columns + derived columns + `jsonl_path` / `jsonl_offset` pointers. **The list response intentionally does NOT include `req`/`resp` body content** — those are only available via `/api/traces/:id` (which seeks into the JSONL file).

- `since`, `until`: RFC3339 timestamps; bounds on `ts_start`.
- `status`: either an exact integer code (`200`, `404`, …) or a bucket of the form `2xx` / `4xx` / `5xx`.
- `model`, `session_root_id`, `project`: equality filters. `project` matches the `client_project` column populated by parser.ExtractProjectContext (W4.1 Phase 2).
- `key_hash`: equality filter accepting an 8- or 16-char hex prefix.
- `path`: defaults to exact match. **A trailing `*` flips it to prefix match** — e.g. `path=/v1/*` matches `/v1/messages`, `/v1/responses`, `/v1/chat/completions`. The semantics live in `internal/api/traces.go::parseListFilters`; viewer Landing uses `/v1/*` by default to hide `/api/v1/*` admin UI traffic.
- `limit`: 1..500, default 100.
- `cursor`: opaque base64 of `(ts_start_ms, id)`. Ordering is always `ts_start DESC, id DESC` for stability.
- Row shape (each element of `traces[]`). Nullable columns ship as JSON `null` when the underlying SQLite column is NULL — they are nullable pointer types in `internal/api/traces.go::rowJSON`:
  ```json
  {
    "id":                    "01HX7K8MS...",
    "ts_start":              "2026-05-27T10:23:45.123Z",
    "ts_end":                "2026-05-27T10:23:46.357Z",
    "client":                "172.17.0.5:54321",
    "method":                "POST",
    "path":                  "/v1/messages",
    "upstream":              "http://gateway:7860",
    "status":                200,
    "model":                 "claude-sonnet-4-6",
    "stream":                true,
    "prompt_tokens":         234,
    "completion_tokens":     521,
    "total_tokens":          755,
    "cached_tokens":         128,
    "cache_creation_tokens": 64,
    "reasoning_tokens":      null,
    "finish_reason":         "end_turn",
    "client_kind":           "claude-code-desktop",
    "client_version":        "1.9659.2",
    "client_project":        "api-log",
    "key_hash":              "a1b2c3d4e5f6a7b8",
    "parent_id":             "01HX7K8M9...",
    "session_root_id":       "01HX7K8M0...",
    "disconnected":          false,
    "truncated_req":         false,
    "truncated_resp":        false,
    "media_count":           0,
    "jsonl_path":            "data/2026-05-27/a1b2c3d4.jsonl",
    "jsonl_offset":          18472
  }
  ```
  The seven post-v0 fields (`cached_tokens`, `cache_creation_tokens`, `reasoning_tokens`, `client_kind`, `client_version`, `client_project`, `media_count`) correspond directly to the ALTER TABLE additions in § 4. `media_count` is `int` (NOT NULL DEFAULT 0); the other six are nullable.
- Full response:
  ```json
  { "traces":      [ ...row objects above... ],
    "next_cursor": "<opaque or null when no more pages>" }
  ```

### 6.3 `GET /api/traces/:id`

Detail. Reads SQLite for the row → opens `jsonl_path` → seeks to `jsonl_offset` (the **pre-write** offset, i.e. where the line starts) → reads one line → unmarshals → returns a two-field envelope:

```json
{
  "row":   { ...flat SQLite row, same shape as one element of §6.2's traces[]... },
  "trace": { ...the parsed JSONL line as defined in §3, including req + resp bodies and events[]... }
}
```

Both halves are included so the caller can choose either side without a second round trip. `row` carries the indexed columns the list view already shows (token totals, session linkage, `client_kind`, `jsonl_path` / `jsonl_offset`); `trace` carries the request and response bodies that only live inside the JSONL line. A list-view consumer that already has the row in hand can ignore `row` and read `trace`; a consumer that only needs the bodies can ignore the SQLite-derived columns. Field names and types in each half are stable per §6.2 and §3 respectively.

If `jsonl_path` has been gzipped (rotated to `<path>.gz`), the server opens the gz file and streams forward to `jsonl_offset` in the **uncompressed** stream. Note: gzip preserves uncompressed offsets — the byte offset stored at write time (in the original `.jsonl`) remains valid as an *uncompressed-stream* offset after gzip; SQLite does not need to be updated on rotation. The lookup is `O(jsonl_offset)` per detail fetch on the gzipped file; acceptable because gzipped files only contain *prior* days. If detail latency on old data becomes a real problem, v0.x adds a per-gz random-access index.

### 6.4 `GET /api/traces/:id/replay`

**Visual replay** of a recorded SSE stream, re-emitted to the API caller at the original per-chunk pacing.

- Only applies to streaming responses. If the trace's `resp` has `body` (non-streaming), responds `400 Bad Request` `{"error":"not_streaming"}`.
- Reads the trace's `resp.events` array; for each event, **reconstructs** an SSE frame from the parsed `{event, data}` pair (`event: <name>\n` if name is non-empty, then `data: <json_compact_encode(data)>\n\n`). The reconstructed frame is *semantically* the same event the upstream sent, but is **not** byte-identical to the original wire — JSON whitespace, key order, and any non-`data:`/`event:` SSE fields the upstream emitted (e.g. `id:`, `retry:`) are lost. If you need byte-exact wire bytes, this endpoint is not it; the raw response tmp file does not survive finalize.
- Between consecutive events sleeps for `(events[i].t_delta_ms - events[i-1].t_delta_ms) / speed` ms. The first event is emitted immediately.
- **`speed` semantics**: `speed=2` means **2× faster** (delays halved). `speed=0.5` means half speed (delays doubled). The formula is divisive: `sleep = delta_ms / speed`. Invalid values (`speed <= 0`, `speed > 100`, NaN) → `400 Bad Request` `{"error":"invalid_param","param":"speed"}`.
- **Null `t_delta_ms` fallback**: if either `events[i].t_delta_ms` or `events[i-1].t_delta_ms` is `null` (the sentinel for reparse-reconstructed traces; see §3 sentinels table), emit event `i` immediately with no sleep — effectively degrade to `?nodelay=1` for that gap. Reparsed traces therefore replay at machine speed; only live-captured traces replay at original pacing.
- Response headers: `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no`.
- Query params:
  - `speed=N` (float, default `1.0`, range `(0, 100]`): see above.
  - `nodelay=1`: dump all events back-to-back with no sleeps (test / "I just want the bytes" mode).

This is the differentiator vs. SDK-based observability tools (Langfuse, Phoenix, LangSmith): they store a normalized post-hoc message and discard per-chunk timing, so they can show *what was said* but not *how it was streamed*. api-log records `t_delta_ms` on the live capture path (§7.1), so the original **pacing** is reproducible — even though individual frame bytes are reconstructed, not the wire originals.

**Replay does not re-contact the upstream.** It re-emits reconstructed SSE frames to the API caller. This is explicitly *not* "replay to LLM" — re-contacting upstream is out of scope by design.

### 6.5 `GET /healthz`

Returns `200 OK` with a compact JSON object exposing the listener's identity, version, and **all in-memory drop / overflow counters** so an operator can detect that capture is degrading without grep-ing logs:

```json
{
  "status":  "ok",
  "ts":      "2026-05-27T10:23:45.123Z",
  "version": "0.x.y",
  "counters": {
    "drop_writer_full":     12,    "drop_jsonl_fail":     0,
    "drop_sqlite_fail":      2,    "truncated_req_total":  5,
    "truncated_resp_total": 17,    "writer_chan_high_water": 234
  },
  "uptime_seconds": 86412
}
```

Counters are cumulative since process start. `/healthz` itself does not check disk or SQLite — only HTTP-path liveness and counter exposure. Operator dashboards should alert on these counters increasing, not on disk/SQLite probes done by `/healthz`.

The counter list above is **illustrative, not exhaustive**. Drop / overflow counters, byte and trace totals, per-status-bucket counts, media file totals, token totals (T3), and writer-channel high-water are all emitted; the viewer Landing reads roughly thirty distinct fields. For the authoritative set inspect `internal/counters/counters.go` (the `Snapshot` struct's JSON tags) — adding a counter is a backend-only change that flows through the same `/healthz` payload.

### 6.6 `GET /`

Returns a JSON pointer to the separate viewer project:

```json
{"name":"api-log","viewer":"https://github.com/2nd1st/api-log-viewer","version":"0.x.y"}
```

The binary contains zero HTML. Frontends are separate consumers of the read API.

### 6.7 What's intentionally not here

- No `/req`, no `/resp`. `/api/traces/:id` returns the full parsed line including both.
- **No replay-to-LLM.** The §6.4 `/replay` endpoint emits the recorded response back to the *API caller* (a viewer); it never re-contacts the upstream gateway. Re-sending a recorded request to an LLM on a user's behalf remains in the philosophy "no" list.
- No `/stats`, no `/api/sessions/:root_id/tree`, no aggregate endpoints. Aggregates are SQL the consumer runs against `index.sqlite` (read-only, multiple concurrent readers under WAL). (`/api/sessions` lists root-id-grouped session summaries — see §6.8 — but is intentionally NOT a tree-walking endpoint.)

The "no write-side endpoints" rule articulated against the §6.8
`/api/config/media` precedent is amended a second time for the
`/api/config/plugins` family landing under Plugin Phase B + C (spec
ratified 2026-05-30 — see ROADMAP §11): `GET/PUT /api/config/plugins`,
`PUT /api/config/plugins/{id}`, `DELETE /api/config/plugins`, and
`GET /api/plugins/types`. The justification mirrors §6.8 for
`/api/config/media`: (a) the alternative is process restart per plugin
edit, which loses in-flight traces; (b) operator edits persist to
`<DataDir>/runtime_overrides.json` under a `plugins` block — YAML
remains the declarative truth, runtime overrides are explicit
deviations; (c) the contract is narrow — only the instance list shape
defined in plugin-b-c-spec.md §3 is eligible. The full-list-replace
semantic (PUT with the whole instances array; no merge-by-id) matches
the spec §3.3 ratification.

### 6.8 Post-v0 endpoint additions

Surface that grew after v0 shipped. Each addition is justified against the §6.7 boundary.

| Endpoint | Method | Phase | Notes |
|---|---|---|---|
| `/api/sessions` | GET | M3 / post-v0 | Lists session summaries grouped by `session_root_id`, latest-activity first. Row-level aggregation only — NOT a tree walk; consumers traverse `parent_id` themselves via `/api/traces/:id`. Query params: `limit` (default 100, max 500), `since` (RFC3339; constraint on the session's last-activity timestamp). Response: `{"sessions":[{...}]}` where each entry carries `session_root_id`, `n_turns`, `first_ts`, `last_ts`, `last_path`, `last_status`, `last_model`, `distinct_key_count`, `ok_count`, `err_count`. The three `last_*` fields are the latest trace's path / status / model copied from the indexed columns so the viewer can render a one-line session summary without a second fetch. `distinct_key_count` is the count of distinct `key_hash` values seen in the session — non-zero greater-than-1 means multiple keys touched the same prefix chain (typically the same operator across two clients). |
| `/api/export` | GET | I (2026-05-29) | Streams a zip of matching JSONL lines + bundled `agent/CLAUDE.md` + `agent/jq-cheatsheet.md` + `README.md`. Same filter set as `/api/traces`. Per §6.7 server-stream rule: the response is *stream-shaped* (Content-Type: application/zip) but it is NOT SSE — it's an HTTP/1.1 response with chunked transfer-encoding that the archive/zip writer emits to. This is justified: zip is a byte-level streaming format that lets the server keep memory bounded for large filter results. |
| `/api/media/{trace_id}/{idx}` | GET | K (2026-05-30) | Streams a single extracted media file from disk with Content-Type derived from the extension via `mime.TypeByExtension`. 404 when the trace has no extracted media at that idx (the trace was recorded with `save_attachments=false`, or the field exists in the body but extraction was skipped per §protocol-skip-list). |
| `/api/config/media` | GET | K | Returns `{save_attachments: bool, source: "default"\|"yaml"\|"env"\|"override"}`. Read-only view of the effective runtime configuration. |
| `/api/config/media` | **PUT** | K | This is the **single write-side endpoint in the read API** as of this writing. The §6.7 blanket "no write-side endpoints" is amended here for the narrow case of operator-toggleable runtime overrides: the operator flips an atomic boolean, the handler atomically write-temp + renames `<DataDir>/runtime_overrides.json`, and the writer goroutine reads the new value on its next trace finalize. Body: `{"save_attachments": bool}`. The amendment is justified because: (a) the alternative is a process restart per toggle, which loses in-flight traces; (b) the override is persisted to a separate file from `config.yaml`, so YAML stays the declarative truth and runtime overrides are explicit deviations; (c) the contract is intentionally narrow — only flags that can be safely flipped during a running trace are eligible (no schema fields, no listener ports, no plugin enable lists). The plugin enable list, if it later gains a runtime toggle, will pattern-match this design. |

### 6.9 `/healthz` endpoint policy (decided 2026-05-30)

The `/healthz` endpoint stays. It carries: liveness + uptime +
cumulative counters (bytes, trace counts by status, drop counters,
media files, token totals from T3, future client-kind hits if and only
if added later).

Two consumer classes exist:

- **Third-party adopters**: k8s liveness probes, reverse-proxy
  health checks, observability sinks, alertmanager rules. These all
  expect a `/healthz` to exist. Removing it would break adopter
  workflows the project explicitly targets.

- **The viewer's Landing surface**: now sources aggregations from
  SQLite (Model histograms from `model` column; token totals via
  `SUM(prompt_tokens + completion_tokens + cached_tokens + ...)`;
  client kind distribution from `client_kind`). The "INTERNAL ·
  healthz" collapsible block on Landing becomes a diagnostic-only
  view — it still exists and queries `/healthz` for the truth on
  drop counters and process uptime — but it is no longer the primary
  numeric source for Landing's main strips.

This decision **commits the project to continuing to emit /healthz
counters** even when the viewer doesn't consume them, because adopter
contracts run on stable endpoint surface. New counters added (T3's
token totals, T4's client kind hits if added) keep flowing.

The viewer-side change (Landing aggregates from SQLite + healthz
becomes diagnostic-only) is a viewer-repo change — tracked under T5,
not landed here.

### 6.10 Hosted viewer (`/viewer/*`)

The backend optionally fetches and serves the viewer bundle at
`/viewer/`. The route is unauthenticated (the viewer is a
browser-load static SPA; the API it calls is still admin-bearer
gated).

**Source of truth**: two constants in
`cmd/api-log/viewer_pins.go` — `viewerVersion` and
`viewerSha256` — bind the backend to a specific
`api-log-viewer` release. Bumping the viewer is a single commit
per RELEASING.md § "Hosted viewer — version + SHA bump".

**Fetch path**: on the first request to a `/viewer/` route OR at
startup (whichever fires first):

1. Hash key from `viewerSha256` → resolve cache dir
   `<data_dir>/viewer-cache/<sha-prefix>/dist/`.
2. If the cache dir contains `index.html`, serve from there.
3. Else hit
   `https://api.github.com/repos/<repo>/releases/tags/<version>`
   to find the `dist.zip` asset.
4. Stream the body; compute SHA-256 inline; compare to the
   constant. Mismatch → reject, log, route returns 503; the binary
   stays up.
5. Match → extract `dist.zip` into the cache dir with
   zip-slip-safe path normalization.

**Failure surface**: zip-extract errors, partial download,
github.com unreachable, rate-limit 403, SHA mismatch, cache dir
permission denied. All log at WARN or ERROR; none crash the
binary. `/viewer/` returns 503 with a JSON body in each failure
mode.

**Operator overrides**: see README `Bundled viewer` for the env
knobs. Override of repo or version REQUIRES a matching SHA
override OR `LOCAL_PATH`.

---

## 7. Write path

```
client conn
   │
   ▼
http.Server → http.Handler → httputil.ReverseProxy
                                │
                                │  (Director rewrites Scheme + Host only)
                                │
                                ▼
                          custom Transport → http.DefaultTransport → upstream
                                │ (tees req bytes)
                                │
                                ▼
                          lossy chan ──► capture-drain goroutine ──► tmp file
                                ▲
                                │  (response path: same pattern in reverse)
                                │
                          ResponseWriter (FlushInterval=-1)
```

### 7.1 Step by step

For each inbound HTTP request:

1. **Allocate trace ID** (ULID), record `ts_start` (`time.Now()`), open two per-trace tmp files at `data/tmp/<id>.req.bin` and `data/tmp/<id>.resp.bin`. The `data/tmp/` directory is on the same filesystem volume as `data/<date>/...` so the inner finalize never crosses devices; on startup the entire `data/tmp/` directory is wiped (orphans from a prior crash). The tmp files hold **body bytes only** — headers and start-lines are read directly from Go's `http.Request` / `http.Response` structs at finalize time, not written to disk.
2. **Spawn two capture-drain goroutines** — one for request body, one for response body — each owning its tmp file. Both also count bytes against a per-direction `max_body_bytes` cap (default 32 MB, §11) and set `truncated_req` / `truncated_resp` if they hit it.

   **Per-event timing capture for SSE.** To record `events[i].t_delta_ms` (§3.3) at the *true wire-arrival time* — not the drainer's later read time — the capture channel payload is `(timestamp, []byte)` pairs, not bare `[]byte`. The `captureSink.Write` call (§7.2) takes its own `time.Now()` reading at the moment the inner Transport delivers a chunk and attaches it to the channel send. The drainer is then a passive consumer: it scans the byte stream for `\n\n` (SSE wire syntax per the spec), and at each boundary the recorded `t_delta_ms` is the timestamp of *the chunk that contained the first byte after this boundary* — i.e. when that byte arrived from upstream, not when the drainer woke up to process it.

   This is the smallest shape-awareness the wire format requires — analogous to reading a TCP segment boundary; it is *not* SSE-protocol parsing. The full event split, JSON unmarshal of each `data:` line, and terminator-vocabulary detection (`[DONE]` / `message_stop` / `response.completed`) all happen later in finalize step 7 by the §10.6 parser. The drainer never opens a JSON value.

   **Compressed SSE caveat.** If the upstream returns SSE with `Content-Encoding: gzip` / `br` / `zstd` (rare but legal), the bytes reaching the drainer are compressed and `\n\n` frame-boundary scanning will not match. In this case the drainer skips timing capture entirely; the full event split happens after decompression in finalize step 7, and **every `events[i].t_delta_ms` is set to `null`** (the §3 sentinel) — `/replay` for such traces degrades to immediate emission. This is acceptable: encoded SSE is uncommon in practice, and the body bytes are still captured fully.
3. **Forward via `httputil.ReverseProxy`** with `FlushInterval = -1`. Director rewrites `req.URL.Scheme` / `req.URL.Host`; sets `req.Host = upstream.Host`; assigns `req.Header["X-Forwarded-For"] = nil` to suppress injection (see §10.4). Nothing else is touched.
4. **Custom `http.RoundTripper`** tees `req.Body` and `resp.Body` to the capture channels and forwards via a `DisableCompression = true` Transport. See §10.2 for the full sketch and §10.3 for why compression is disabled. The Transport also disables idempotent retry (`req.GetBody = nil`) — see §10.2 for the tradeoff.
5. **`captureSink.Write(p)` is non-blocking.** It copies `p` and tries a non-blocking send on the capture channel; if the channel is full, bytes are dropped and `truncated_*` is flagged. Forwarding reads return `len(p), nil` either way — the tee Reader never short-reads to upstream because it forwards the bytes it just read, regardless of whether the capture sink accepted them.
6. **Capture-drain goroutines** read from their channels and append to the per-trace tmp file, applying the `max_body_bytes` cap. If disk is slow, the channel backs up; drops happen at step 5.
7. **Finalization** — entered the first time any of these conditions hold:
   - Both `req.Body` and `resp.Body` reach EOF (the happy path).
   - The inner `RoundTrip(req)` returned an error before headers (upstream TCP error, connection refused, etc.) — in which case `req.Body` may not have been fully consumed and the response side never produced anything. Finalize triggers immediately on the error path; the request capture drainer is cancelled rather than waited on.
   - The response-body copy returns with an error after headers were received (mid-stream upstream disconnect, transport read error, decompression error). The `httputil.ReverseProxy`'s body-copy loop already detects this; the forwarding goroutine sees the resulting error and triggers finalize. The response side is marked `disconnected: true` and whatever bytes the capture drainer received are kept.
   - The forwarding goroutine's `ctx` is cancelled. This covers: (a) client disconnect, (b) process shutdown (§7.5), and (c) the `stream_idle_timeout` watchdog (§10.7) firing when an SSE stream goes silent past the configured threshold.
   - The per-trace `req_body_capture_timeout` (default 60 s) fires while waiting for `req.Body` EOF — guards against the pathological case where the inner Transport reads partial req body, the response gets sent and consumed, but `req.Body` is never closed cleanly. After the timeout, the request side is marked `truncated_req: true` and finalize proceeds.

   The finalize trigger is a `sync.Once`-guarded function so all paths converge safely. Inside finalize:
   - Close the capture channels. Each drainer goroutine owns its tmp file and closes it when its loop exits; finalize blocks on the drainer-done channels so `buildTrace`'s later `os.Open` cannot race a torn write.
   - **Parse phase**: read the response tmp file. Determine the body form from `resp.Header.Get("Content-Type")` after stripping `Content-Encoding` if we decompressed (see §10.3): `application/json` → unmarshal as `body`; `text/event-stream` → SSE event parser (§10.6) → `events` array + `stream_done`; everything else → base64 → `body_b64`. JSON parse failure → fall back to `body_b64 + parse_error`. Parse the request body analogously.
   - **Compute derived fields** (named-field extraction only, no body synthesis): `model` from `req.body.model`; `key_hash` from `req.Header.Get("Authorization")` (fallback `x-api-key`); token counts from the relevant final-chunk fields per protocol; `prefix_canonical_hash` from §5.1.
   - **Build the JSONL line** struct (using `req.Header` directly for headers — there is no header-bytes file to read).
   - Send the struct on the writer channel.
8. **Writer goroutine** (single instance):
   - Compute `jsonl_path` for the trace's date and `key_hash[:8]`, opening the file (O_APPEND | O_CREATE) if not already open. See §7.4 for rotation handling.
   - Marshal the line, **record the file's current size as `jsonl_offset` (this is the pre-write offset where the line starts)**, then `Write` the line.
   - Run session inference (§5.2) to compute `parent_id` and `session_root_id`.
   - `INSERT INTO traces` with all mirror + derived + session columns.
   - Delete the per-trace tmp files.

### 7.2 The lossy channel pattern

```go
type captureSink struct {
    ch     chan []byte
    onDrop func()             // sets the trace's `truncated_req` or `truncated_resp` flag
                              // depending on which direction this sink covers
}

func (s *captureSink) Write(p []byte) (int, error) {
    buf := make([]byte, len(p))     // caller may reuse p
    copy(buf, p)
    select {
    case s.ch <- buf:
    default:
        s.onDrop()
    }
    return len(p), nil
}
```

Channel buffer: 64 slots × typical 4-32 KB chunks ≈ 2 MB per direction per trace. Configurable. **Dropping under load is the design**, not a failure — it preserves principle 2 (capture, never interfere).

### 7.3 Edge cases

| Situation                                | Behavior |
|------------------------------------------|----------|
| Client disconnects mid-stream            | Capture goroutines flush what they have; `disconnected: true`. |
| Upstream closes mid-stream               | Same. |
| Upstream times out / TCP error           | `status: -1`, `disconnected: true`, response body empty or partial. |
| Tmp file write fails (EIO, disk full)    | Bytes dropped silently for the rest of that trace; `truncated_*: true`. |
| Parser sees malformed JSON / SSE         | Line is still written with `body_b64` + `parse_error`. The trace is not dropped. |
| Writer channel full (rare)               | Trace is dropped at the writer-channel boundary, logged with trace ID and a `drop_writer_full` counter increment. Body tmp files are deleted. The forwarded response was already delivered to the client; only the metadata is lost. |
| JSONL append fails                       | Logged with `drop_jsonl_fail` counter increment. SQLite upsert skipped. Tmp files deleted. As above, client already got its response. |
| SQLite upsert fails after JSONL succeeds | Logged with `drop_sqlite_fail` counter. On next startup, the rebuild pass re-inserts the missing row from JSONL. |
| SQLite locked / busy past `busy_timeout` | Same as upsert fail. The 5 s busy timeout is a soft ceiling; under sustained read-side load this can fire. The writer keeps going (JSONL was already written); on startup the rebuild pass catches up. |

**Failure ordering**: forwarding ≫ Layer 1 (JSONL) ≫ Layer 2 (SQLite). A failure to the right never affects anything to its left. **But**: because the writer goroutine appends JSONL *and* upserts SQLite in sequence, a slow SQLite (busy_timeout up to 5 s) does delay subsequent traces' JSONL writes — the writer-channel buffer absorbs the burst; if it saturates, subsequent traces' metadata is dropped (logged) while their forwarding has already completed. **The hierarchy is real; it is not free.** Operators monitor the `/healthz` drop / overflow counters (§6.5) and the writer-channel high-water mark to detect when SQLite back-pressure is throttling capture.

### 7.4 Daily rotation

The writer keeps one open file handle per active `(date, key_hash[:8])` pair. Before every line write, it computes `today_date = utc_date(now)` and compares to the file's date; if they differ, it:

1. Closes the current file handle.
2. Schedules a background gzip of the just-closed file (`gzip <path>` → `<path>.gz`, then `rm <path>`; the gzipped file is the canonical name from then on).
3. Opens a new handle for `data/<today_date>/<key_hash[:8]>.jsonl`.
4. Proceeds with the line write.

A trace whose `ts_start` is on day N but whose finalize happens after midnight (day N+1) is appended to day N+1's file. This is by design — `ts_start` records when the request *arrived*, the file path records when we *wrote* the line; consumers reconcile via `ts_start` if they care about request-arrival grouping.

The gzip step is sequential per file (one gzip worker per concurrent rotation), runs at low CPU priority, and does not block the writer goroutine. The read-API `/api/traces/:id` finds either `<path>.jsonl` (today) or `<path>.jsonl.gz` (any past day) — see §6.3.

### 7.5 Graceful shutdown

On SIGTERM:

1. **Stop accept** — close the proxy listener so new connections get refused (existing connections keep working).
2. **Stop API accept** — close the API listener.
3. **Wait for in-flight forwarding** — with a configurable `shutdown_grace_seconds` (default 30 s) hard ceiling. Each forwarding goroutine completes naturally: capture finalize, attempt non-blocking send on the writer channel, return. **The send remains non-blocking during shutdown** — same policy as normal operation; if the writer channel is saturated, the in-flight trace is dropped (logged with `drop_writer_full`) so shutdown is never blocked by a slow consumer.
4. **Close writer channel** — once all forwarding goroutines have returned (or the grace period elapsed).
5. **Drain writer goroutine** — process remaining queued lines, append to JSONL, upsert SQLite, then return.
6. **Wait for in-flight gzip workers** — rotated files being compressed in the background (§7.4) complete or are interrupted at a `shutdown_grace_seconds` ceiling. An interrupted gzip leaves the plain `.jsonl` in place; the next startup detects it (no matching `.jsonl.gz` for a non-current-day plain `.jsonl`) and re-runs the gzip.
7. **Close all open JSONL handles and the SQLite write handle.**
8. **Exit.**

Hard ceiling reached → drop remaining in-flight traces (their forwarded responses, if not yet delivered, are abandoned; this is a tradeoff against shutdown latency). Hitting the ceiling is logged.

---

## 8. Concurrency model

```
http.Server (accept) ──► N × forwarding goroutines (one per request)
                          │
                          │  each spawns 2 capture-drain goroutines (transient)
                          ▼
                       (2N × capture-drain goroutines)
                          │
                          └─ finalize → writer channel (buffer 1024)
                                              │
                                              ▼
                                    1 × writer goroutine ─► JSONL append + SQLite UPSERT
                                              │              + parse_session_parent()
                                              ▼
                                    JSONL files / index.sqlite
                                              ▲
                                              │ (reads only, separate connection pool)
                                              │
                              http.Server (API) ──► M × API handler goroutines
```

| Channel              | Buffer | Producer            | Consumer          | Overflow policy |
|----------------------|--------|---------------------|-------------------|-----------------|
| Per-trace capture (req or resp) | 64 | forwarding goroutine | drain goroutine | non-blocking send, drop with `truncated_req` / `truncated_resp` flagged |
| Writer               | 1024   | forwarding goroutines | single writer    | non-blocking send, drop the metadata line (logged) |

**Why one writer for both JSONL and SQLite**: serializing both writes in the same goroutine avoids per-line locking on the JSONL file, avoids racing on SQLite rows, and removes the temptation to fan out a Go channel (channels do not tee). It is the simplest correct implementation; the throughput ceiling is dictated by SQLite WAL, which is fine for the realistic workload (hundreds to a few thousand traces per second per instance).

---

## 9. Failure modes

| Failure                            | Forwarding affected? | Recovery |
|-----------------------------------|----------------------|----------|
| Disk full                         | No (truncated traces) | Free disk; capture resumes. Affected traces marked `truncated_req` / `truncated_resp`. |
| `index.sqlite` corrupt or deleted | No                   | Restart. Startup rebuild scans `data/**/*.jsonl{,.gz}` and re-inserts all rows. Cost: roughly a few minutes per million traces on commodity SSD, dominated by JSONL line decode + per-line session-inference SQL lookup. |
| `api-log` process crashes         | Yes (client sees connection error) | systemd restart. Tmp files older than `tmp_grace` (default 1h) are deleted at startup. |
| JSON parser bug / new schema      | No                   | The bad trace gets `body_b64` + `parse_error` and is still indexed. Fix the parser, redeploy, optionally re-parse the affected day's JSONL. |
| Upstream gateway down             | No (gateway's error is forwarded as-is) | None — gateway's problem. |
| Writer channel saturated          | No                   | Trace metadata lost; logged with `drop_writer_full` counter. Body tmp files are dropped. |
| SQLite stuck busy beyond timeout  | No (eventually)      | Writer logs and skips that one SQLite upsert; JSONL line already on disk. The next startup's rebuild backfills the missing row. |

Hard rule: **no failure on our side, short of process death, prevents the client from receiving the gateway's response.**

---

## 10. Implementation notes

These are pitfalls worth pinning down once so the v0 implementer does not rediscover them.

### 10.1 `httputil.ReverseProxy` and SSE

`httputil.ReverseProxy`'s default copy buffers bytes before flushing them to the client, which clumps SSE events. Set `FlushInterval = -1`:

```go
proxy := &httputil.ReverseProxy{
    Director:      directorFn,            // rewrites req.URL.Scheme/Host only
    Transport:     captureTransport,      // wraps http.DefaultTransport (§10.2)
    FlushInterval: -1,                    // flush after every Write to client
}
```

The downstream `http.ResponseWriter` must implement `http.Flusher` — the standard `net/http` server's writer does, so this is satisfied automatically.

### 10.2 Capture via custom Transport

The body capture lives inside a custom `http.RoundTripper`, not in `Director` or `ModifyResponse`. The Transport is the only point where both request bytes (about to go upstream) and response bytes (just arrived from upstream) flow through code we own.

**Headers are not captured to tmp files.** They are read at finalize time directly from `req.Header` and `resp.Header` (the in-memory `http.Header` maps), which already contain everything we need. The capture sinks are *body-only*. This removes the temptation to write a half-baked HTTP wire format to disk.

**Critical**: do **not** call `req.Write(w)` or `req.WriteProxy(w)`. Both consume `req.Body` to EOF, which means the inner `RoundTrip(req)` then sees an empty body and forwards a zero-byte request upstream. Instead, replace `req.Body` with a `TeeReadCloser` so the inner Transport's read drives the tee:

```go
type captureTransport struct {
    inner http.RoundTripper
    sinks func(traceID string) (reqSink, respSink chan<- []byte)
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
    traceID := req.Context().Value(traceIDKey{}).(string)
    reqSink, respSink := t.sinks(traceID)

    // 1) Wrap req.Body so the inner Transport's read also feeds the sink.
    if req.Body != nil {
        req.Body = newTeeReadCloser(req.Body, reqSink)
        // GetBody: leave nil for non-idempotent (POST). For idempotent
        // requests where we want retry behavior, the caller is responsible
        // for buffering — out of scope for v0.
        req.GetBody = nil
    }

    // 2) Forward.
    resp, err := t.inner.RoundTrip(req)
    if err != nil {
        return nil, err
    }

    // 3) Wrap resp.Body so the proxy's read also feeds the response sink.
    resp.Body = newTeeReadCloser(resp.Body, respSink)
    return resp, nil
}
```

Both sinks are `captureSink` instances (§7.2) — non-blocking writers feeding per-trace body tmp files. Headers are read at finalize time from `req.Header` / `resp.Header`.

### 10.3 Disable transparent gzip decompression

Go's `http.DefaultTransport` adds `Accept-Encoding: gzip` to outbound requests *only if the request does not already have an `Accept-Encoding` header*, and silently decompresses + strips `Content-Encoding` from the response *only when it added the header itself*. Either way, the captured bytes may or may not match what the upstream actually returned on the wire.

For api-log we want a predictable rule: **whatever the upstream returns lands on disk as-is, and the parser decompresses in memory at finalize**. Achieve this with:

```go
inner := http.DefaultTransport.(*http.Transport).Clone()
inner.DisableCompression = true
```

With `DisableCompression = true`:
- Go's Transport no longer auto-injects `Accept-Encoding: gzip` and no longer auto-decompresses.
- The client's `Accept-Encoding` (if any) is forwarded as-is.
- If the upstream returns gzipped/br/zstd bytes, those bytes land in the response tmp file unchanged, and the response headers retain their `Content-Encoding` value.

At finalize (§7.1 step 7):
- If `Content-Encoding` is a single known encoding (`gzip`, `br`, or `zstd`), decompress the body in memory before parsing JSON / SSE.
- If `Content-Encoding` is a chain (e.g. `gzip, br`), decompress in left-to-right order. If every link is known, treat the result as decoded. If any link is unknown, abort decompression, leave the body as `body_b64`, and keep the original full `Content-Encoding` header.
- **On successful full decompression, strip `Content-Encoding` from `resp.headers` in the JSONL line** so consumers know `resp.body` is decoded.

This is a deliberate **fidelity tradeoff**: the captured `resp.headers` is no longer byte-identical to what arrived from upstream (we have removed the `Content-Encoding` field). The benefit is header-body consistency — a downstream consumer reading the JSONL line sees a `resp.body` of decoded JSON next to headers that describe the bytes you see, not a stale `Content-Encoding: gzip` next to already-decoded JSON. Principle 6 ("filesystem is truth") still holds for **body bytes**, which are recorded faithfully (just decoded); only this one specific header is mutated. If a use case ever needs the original full headers, the raw response tmp file at finalize is the answer, but it is not retained beyond finalize.

### 10.4 Strip default `X-Forwarded-For`

`httputil.ReverseProxy.ServeHTTP` appends `X-Forwarded-For` from `req.RemoteAddr` **after** `Director` runs. Calling `req.Header.Del("X-Forwarded-For")` in `Director` does **not** prevent that append — it just gives ReverseProxy an empty slot to fill (verified by experiment, see `experiments/x-forwarded-test/`).

The correct suppression in `Director` is to assign `nil` (not delete) so ReverseProxy treats the header as "present, empty, do not modify":

```go
director := func(req *http.Request) {
    req.URL.Scheme = upstreamURL.Scheme
    req.URL.Host   = upstreamURL.Host
    req.Host       = upstreamURL.Host                  // override Host header
    req.Header["X-Forwarded-For"] = nil                // suppress XFF injection
    // X-Forwarded-Host and X-Forwarded-Proto are NOT injected by ReverseProxy
    // by default, so no suppression needed for those.
}
```

Alternatively, use `ReverseProxy.Rewrite` (Go 1.20+) instead of `Director` and simply don't call `req.SetXForwarded()`:

```go
proxy.Rewrite = func(req *httputil.ProxyRequest) {
    req.SetURL(upstreamURL)
    // Intentionally NOT calling req.SetXForwarded()
}
```

`Director` and `Rewrite` are mutually exclusive; pick one. v0 uses `Director` because it interoperates more cleanly with explicit Host override.

### 10.5 ULID for trace IDs

Lexically time-sortable. Directory listings are naturally chronological. UUIDv7 is equivalent functionally; pick ULID for ergonomics with `ls`.

### 10.6 SSE event parsing

The parser handles three SSE shapes verified against recorded reference traffic (see `experiments/session-inference/real_api_test.py`):

**A. Data-only (OpenAI Chat Completions):**

```
data: {"id":"...","choices":[{"delta":{"content":"H"}}]}\n
\n
data: {"id":"...","choices":[{"delta":{"content":"i"}}]}\n
\n
data: [DONE]\n
\n
```

Each `data:` line is a JSON value; no `event:` prefix. Emit each as `{"event":"", "data": parsed_json}`. Stream terminates on `data: [DONE]` → `stream_done: true`.

**B. Event-named (OpenAI Responses):**

```
event: response.created\n
data: {"type":"response.created","response":{...},"sequence_number":0}\n
\n
event: response.output_text.delta\n
data: {"type":"response.output_text.delta","delta":"H","sequence_number":4}\n
\n
event: response.completed\n
data: {"type":"response.completed","response":{...,"usage":{...}}}\n
\n
```

Emit `{"event": "response.xxx", "data": parsed_json}`. Stream terminates on `event: response.completed` → `stream_done: true`.

**C. Event-named (Anthropic Messages):**

```
event: message_start\n
data: {"type":"message_start","message":{"id":"msg_..."}}\n
\n
event: content_block_delta\n
data: {"type":"content_block_delta","delta":{"text":"H"}}\n
\n
event: message_stop\n
data: {"type":"message_stop"}\n
\n
```

Same shape as B with different event names. Stream terminates on `event: message_stop` → `stream_done: true`.

Parsing strategy: read line by line, accumulating an `event:` value (default empty) and a `data:` value; on blank line, emit one event object; on stream EOF, **flush any pending accumulator as a final event** (some servers do not terminate the last frame with a blank line) and then check whether a recognized terminator was seen — if so, `stream_done: true`; otherwise `stream_done: false`. The parser is dispatch-free in the shape sense — the same line reader handles all three formats; only the **terminator-vocabulary** detection is shape-aware (`[DONE]` for Chat, `response.completed` for Responses, `message_stop` for Anthropic Messages).

**Two distinct shape-awarenesses in the codebase, not to be confused** (§7.1 step 2 + this section):

- The **live capture drainer** is shape-aware at the *wire-syntax* layer: it scans for SSE `\n\n` frame boundaries to record per-event timing. It never opens a JSON value or knows event names.
- The **finalize parser** (this section) is shape-aware at the *terminator-vocabulary* layer: it knows the names of clean-stream sentinels per protocol so it can set `stream_done`. It does the JSON unmarshal of each `data:` payload.

Neither knows about meaning (model names, token counts, tool calls); those are downstream consumer concerns.

If a body that looked like SSE fails to parse at all (no valid `data:` lines), fall back to `body_b64` + `parse_error`.

### 10.7 Timeouts

- `http.Server.ReadHeaderTimeout`: 10s default.
- `http.Server.WriteTimeout`: **unset.** A real value here would kill long SSE responses (which can legitimately stream for tens of minutes). Instead, we apply per-stream idle detection — see below.
- `http.Server.IdleTimeout`: 120s default (between requests on a kept-alive connection; unrelated to in-flight stream pacing).
- Upstream Transport: `http.DefaultTransport` defaults (no per-request deadline).
- **Per-stream idle timeout** (`stream_idle_timeout`, default 10 min). For streaming responses, a watchdog tracks the time since the last byte forwarded to the client. If no byte arrives for `stream_idle_timeout`, the watchdog cancels the request context, closing both the upstream and the client connection. The trace finalizes with `disconnected: true` and `stream_done: false`. This catches hung SSE streams that the absent `WriteTimeout` would otherwise leave open forever, without truncating legitimately slow streams.
- **Request-body capture timeout** (`req_body_capture_timeout`, default 60 s). Bounds how long finalize will wait for `req.Body` EOF after the response side has already finalized — see §7.1 step 7.

---

## 11. Configuration

A minimal YAML, all fields optional (defaults shown):

```yaml
proxy:
  listen:   ":7861"
  upstream: "http://localhost:7860"

api:
  listen:   ":7862"
  # admin_token auto-generated at data/admin_token on first run.

storage:
  data_dir:          "./data"               # tmp/ subdir is wiped on startup
  max_body_bytes:    33554432               # 32 MB per direction
  capture_chan_size: 64                     # per-trace, per-direction
  writer_chan_size:  1024

timeouts:
  read_header_seconds:        10            # http.Server.ReadHeaderTimeout
  idle_seconds:               120           # http.Server.IdleTimeout
  stream_idle_seconds:        600           # cancel ctx if SSE chunk gap exceeds this
  req_body_capture_seconds:   60            # finalize waits at most this for req body EOF

shutdown:
  grace_seconds:     30

logging:
  level: "info"                             # debug | info | warn | error
```

Environment overrides use `APILOG_<SECTION>_<KEY>` for any path in the YAML, with nested keys joined by `_` and upper-cased. Examples:

```
APILOG_PROXY_UPSTREAM=http://gateway:7860
APILOG_PROXY_LISTEN=:7861
APILOG_API_LISTEN=:7862
APILOG_STORAGE_DATA_DIR=/var/lib/api-log/data
APILOG_STORAGE_MAX_BODY_BYTES=67108864
APILOG_SHUTDOWN_GRACE_SECONDS=60
APILOG_LOGGING_LEVEL=debug
```

Env vars always win over YAML.

---

## 12. Open questions for v0

These are deliberately left to the implementer:

- **Channel buffer sizes**: start with the defaults in §11; revisit under load testing.
- **Orphan tmp file GC cadence**: scan at startup, then once per hour. Tmp files older than 1 h with no associated in-flight trace are removed.
- **CLI subcommands** for reparse, rebuild-sqlite, and gzip-old-files. None are required for v0; `index.sqlite` deletion + restart already covers rebuild; reparse can be added when a real parser bug needs it.
- **Per-trace concurrency cap**: implicit upper bound on number of concurrent forwarding goroutines (and thus per-trace tmp files / channels in memory). v0 may set this via `http.Server.MaxConnsPerHost` or rely on OS file-descriptor limits; revisit if file-descriptor exhaustion becomes a real failure mode.

---

## 13. Known limitations

Documented limitations the project ships with, by deliberate operator
decision. Each entry names the protocol/path affected, the symptom,
and the reason for accepting it as-is.

### 13.1 OpenAI Responses `encrypted_content` is opaque

Some recorded /v1/responses traces contain reasoning blocks where the
upstream returns `encrypted_content` instead of plaintext reasoning.
There is no decryption key; the project cannot recover the underlying
text. This is accepted as a protocol limitation: api-log records the
opaque field but cannot recover plaintext reasoning.
The viewer's reasoning tombstone shows the summary/id without a
"plaintext not available" placeholder — the absence of a body
communicates the redacted state.

### 13.2 Streaming usage extraction — Gemini deferred

Gemini `:streamGenerateContent` SSE token extraction is NOT
implemented — token columns will be NULL for Gemini streaming traces.
The non-streaming Gemini path IS covered (see §6 / ROADMAP §8 for the
current `parser.ExtractUsage` coverage matrix; this entry is strictly
the deferral note, not a re-assertion of what does work).

Deferred until a Gemini-using operator surfaces with real sample event
shapes — synthesizing field paths from public docs without empirical
samples risks a §1 amendment that doesn't match real wire format.

### 13.3 Plugin error telemetry + panic recovery

The Plugin Phase A.1 wiring (commit `4500a7d`) does NOT yet:
- Increment a `drop_plugin_error` counter when a plugin's
  `IterateBeforeRecord` returns an error (errors only logged WARN).
- Wrap plugin calls in `defer recover()` — a buggy third-party
  plugin could leak tmp files (the upstream response IS already
  forwarded, but tmpDir.RemoveTraceFiles
  is unreachable past a panic).

Both are forward-looking; no third-party plugins exist in tree yet.
First plugin proposal that needs them will trigger the work.

### 13.4 AFTER-hook tool_call argument mutation — deferred to Phase D

Plugin Phase B + C v1 (contract frozen 2026-05-30 in
[docs/specs/plugin-b-c-spec.md](./docs/specs/plugin-b-c-spec.md)) ships AFTER-hook mutation for
`ParsedResponse.Content` and `ParsedResponse.Reasoning` only.
Mutation of streaming `tool_call` argument fragments on the AFTER hook
is deferred to Phase D per spec §10.6.

`ParsedResponse.ToolCalls` is exposed for AFTER plugins to READ; v1
plugins MUST NOT mutate it. The `text-replace` AFTER half explicitly
passes `tool_use` `input_json_delta` events through untouched even
when the match string appears in tool-call argument JSON. The
`text-append` AFTER half emits a synthesized final text delta and
leaves tool_call events alone. Operators wanting to control tool-call
behavior in v1 use one of: BEFORE-side `ParsedRequest.Tools` editing
(strip tools the model is not allowed to call), full-response
`ActionIntercept` (replace the whole stream with a refusal / safe
completion), or Observer-class JSONL scrub at record time.

The deferral is grounded in the spec §10.6 4-lens adversarial review:
the OSS-adopter and use-case-substitution lenses both found zero
realistic named adopters for AFTER-side tool_call argument mutation,
and the maintenance-burden lens flagged ~1400 LOC of bidirectional
per-protocol SSE re-emitter (Anthropic / Chat / Responses / Gemini all
differ) as a textbook vendor-wire-format trap if pre-emptively shipped.

Forward compatibility: when Phase D arrives, tool_call mutation lands
as a SEPARATE optional `ToolCallMutator` interface detected by type
assertion at dispatch time (Go stdlib `io.WriterTo` / `io.ReaderFrom`
evolution pattern), so existing v1 `BeforePlugin` / `AfterPlugin`
implementations remain UNTOUCHED. The dispatcher gains
buffer-then-expose machinery only when at least one registered plugin
opts in via the new interface. Re-open Phase D when a real adopter
files an issue with a concrete use case that BEFORE-tools-array,
`ActionIntercept`, or Observer-scrub genuinely cannot serve.

---

## 14. What this is not

The shape of the project is as much defined by what it refuses to
do as by what it does. The points below are architectural, not
roadmap items — they will not change without a §1-level
amendment.

### 14.1 Single-node only

One binary, one `<data_dir>`. There is no clustering layer, no
shared-storage replication, no leader election, no cross-node
trace ID coordination. Two binaries pointed at the same data
directory will collide at the ULID-generation and SQLite-writer
layers; see [docs/operations.md](./docs/operations.md)
"Multi-instance" for the failure modes.

Horizontal capture scale is solved by running independent
instances — one per LLM gateway upstream, each with its own data
directory — and aggregating downstream from the JSONL trees. The
project is shaped for the "tcpdump in front of one gateway"
deployment; a multi-tenant control plane is out of scope.

### 14.2 JSONL is truth; SQLite is rebuildable cache

§1 states the invariant; this section restates it as a posture.
Adopters can delete `index.sqlite` at any time and the binary will
rebuild it from the JSONL tree on next startup. Backup strategies
treat the JSONL tree as the only artifact that matters; the SQLite
index is a query-acceleration convenience, not a second source of
record.

The forward-referenced `api-log rebuild` subcommand (ROADMAP
"Day-2 operations") will surface this as an explicit operator
command rather than the implicit startup-on-empty-data-dir
behavior, but the underlying invariant is unchanged.

### 14.3 WAL means concurrent readers, single writer

The SQLite WAL mode documented in §4 buys us many concurrent
readers (the read API, plus any external `sqlite3` session a human
or a tool opens against the file) coexisting with the
writer goroutine. It does **not** buy us multiple writers. The
write path is one goroutine, full stop; the channel in front of it
absorbs bursts and drops with a counter when it saturates (§7.3).
Adopters writing custom tooling against `index.sqlite` should open
read-only connections.

### 14.4 No gateway behavior in the capture path

A consolidated restatement of the ROADMAP "What we will not do"
list as it intersects this document: the proxy does not
authenticate, route, retry, rate-limit, cache, rewrite, or
redact in the capture path. The `req.headers` and `resp.headers`
captured in the JSONL line are what the proxy actually forwarded
(modulo the §10.4 `X-Forwarded-*` suppression and the standard
hop-by-hop strip ReverseProxy already performs). Plugins
(§6.7 / ROADMAP §11) are the only sanctioned mechanism for
operator-side mutation, and they are off by default.

---

Operator-facing material — backup procedure, manual WAL checkpoint
one-liner, retention cross-reference, multi-instance behavior — is
not in this document. See [docs/operations.md](./docs/operations.md).
