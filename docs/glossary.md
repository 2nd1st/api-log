# Glossary

Terms used across `api-log` documentation and code, defined once so
the rest of the prose can be terse. Alphabetical. Each entry points
back to the ARCHITECTURE section where the term is defined deeper.

### bucket

A `(date, key_hash[:8])`-keyed JSONL file the writer goroutine
appends to — e.g. `<data_dir>/2026-05-27/a1b2c3d4.jsonl`. One bucket
per client per day. The retention loop evicts at bucket granularity;
the storage coordinator's lease (see *lease*) is held against a
bucket. ARCHITECTURE §2.

### capture_filter

Configuration knob that decides, before any tmp file is opened,
whether a request is recorded at all. Path-based and key-based
predicates only — never matched against credential-carrying header
values (per ROADMAP "What we will not do"). Distinct from
*path_filter*, which acts at read time. ARCHITECTURE §11.

### data_dir

The on-disk directory the binary owns: JSONL tree, `index.sqlite`,
`tmp/`, `admin_token`, `runtime_overrides.json`, and (when the
hosted viewer is enabled) `viewer-cache/`. Set via
`storage.data_dir:` in YAML or `APILOG_STORAGE_DATA_DIR` env;
defaults to `./data`. The binary has no `--data-dir` flag — the
only CLI flag is `-config <path>`. ARCHITECTURE §2.

### finalize

The writer's `appendOne` moment — the point at which a trace's
captured request, response, headers, and parsed bodies are stitched
into one JSONL line, written, and upserted into SQLite. Session
inference runs here. After finalize the per-trace tmp files are
removed. ARCHITECTURE §7.1 step 7-8.

### key_hash / key_hash8

First 16 hex characters of `sha256(canonical_auth_string)`.
`canonical_auth_string` is `Authorization` header value if present,
else `x-api-key`, else empty (yielding the all-zero hash for
unauthenticated traffic). `key_hash8` is the first 8 chars and
appears in bucket file names; SQLite stores the full 16-char form;
the read API accepts either as a prefix. ARCHITECTURE §2.

### lease

The storage coordinator's per-bucket lock the writer goroutine
acquires before opening a file handle, and the retention loop must
fail to acquire before it can evict the bucket. Released by the
writer's idle-close timer (default 10 min). See
docs/retention.md "How eviction actually fires" and `internal/
storage/lease.go`.

### path_filter

Read-time filter on the `path` column, applied by `/api/traces` and
the viewer. Defaults to `/v1/*` (prefix match via trailing `*`) so
the viewer's Landing hides the binary's own `/api/v1/*` admin
traffic. Distinct from *capture_filter*. ARCHITECTURE §6.2.

### prefix_canonical_hash

`sha256(canonicalize(session_prefix))[:16]` where the session prefix
is constructed per protocol (chat `messages` array, Anthropic
`system + messages`, Responses `instructions + input`).
`canonicalize` is sorted-key, no-whitespace JSON encoding. Stored
per trace; the IN-clause that finds a parent in §5.2 looks up this
column directly without ever opening a JSONL line. ARCHITECTURE §5.1.

### retention

The background loop that monitors `data_dir_bytes` and, if either
`max_bytes` or `max_age_days` is set, evicts oldest-first at bucket
granularity. Off by default (engine reports usage; never deletes).
Covered fully in docs/retention.md. ARCHITECTURE §4 (the
`idx_jsonl_path` index that backs it) and docs/retention.md.

### session

A conversation tree inferred from prefix chaining on the wire — not
"one task" or "one agent run." A session has a `session_root_id`
(equal to the root trace's `id`), and each non-root trace's
`parent_id` points to the trace whose full prefix matches the
longest strict prefix of the new trace. ARCHITECTURE §5.

### session_root_id

The trace ID at the root of a session's conversation tree. For a
trace with no inferred parent, `session_root_id = id`. All
`/api/traces?session_root_id=...` queries pivot on this column.
ARCHITECTURE §5.2 and §6.2.

### trace

One captured HTTP request-response pair: client → proxy →
upstream → proxy → client, plus the metadata
(timestamps, headers, parsed bodies, token counts, session linkage).
One JSONL line equals one trace. Streaming responses become an
`events` array within the line. ARCHITECTURE §3.
