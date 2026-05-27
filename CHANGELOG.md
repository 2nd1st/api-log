# Changelog

All notable changes to this project will be documented here. Format:
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versioning:
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Wire-format
contracts (JSONL line schema, on-disk layout, HTTP read API) follow the
append-only / new-format-key migration discipline documented in
[PHILOSOPHY.md § principle 6](./PHILOSOPHY.md).

## [Unreleased]

### Added
- Initial design documents (PHILOSOPHY, ARCHITECTURE, README) — six review rounds.
- Verification experiments: X-Forwarded-For Go test, session-inference Python harness with synthetic + real-API scenarios.
- Project scaffold: Go module, package layout, CI workflow, hygiene files.
- **M1**: forwarding proxy + body capture pipeline. Bytes flow through to upstream; tmp files capture both directions; X-Forwarded-For suppressed via `Header[name] = nil`.
- **M2**: finalize parser + JSONL writer. Every completed trace lands at `data/<UTC-date>/<key_hash[:8]>.jsonl` matching the ARCHITECTURE § 3 schema. SSE parser handles all three shapes (data-only Chat / event-named Responses / event-named Anthropic) with `[DONE]` / `response.completed` / `message_stop` terminator recognition. JSON unmarshal for non-streaming bodies; `body_b64 + parse_error` fallback for non-JSON / parse failures. Content-Encoding decompression for `gzip` (stdlib); `br` / `zstd` graceful fallback. Daily rotation with background gzip workers. Writer goroutine + lossy `TrySend` channel.
- **M3**: SQLite mirror + session inference. New `internal/session` builds per-protocol session prefixes (Chat / Anthropic with `__apilog_system__` virtual turn / Responses with `input` normalization). New `internal/store/sqlite` uses modernc.org/sqlite (pure Go, no cgo) with WAL + NORMAL pragmas. Writer goroutine wraps JSONL append + SQLite INSERT + session inference IN-clause + UPDATE in a single transaction per trace (one fsync). `sync.Pool` for capture chunks reduces GC pressure under sustained load (sized for the 2000-user-team throughput target).
- **M4**: read API on a second listener. New `internal/api` mounts four endpoints: `GET /healthz` (status + uptime + atomic counters from `internal/counters`), `GET /api/traces` (SQLite-backed list with `since`/`until`/`status`/`model`/`key_hash`/`session_root_id` filters, opaque base64 `(ts_start_ms, id)` cursor pagination, default limit 100 cap 500), `GET /api/traces/:id` (SQLite row + JSONL seek by `jsonl_offset` — handles `.jsonl` and `.jsonl.gz` transparently since gzip preserves uncompressed offsets), `GET /` (JSON pointer to the separate viewer project). New `internal/admin` manages the `data/admin_token` file (256-bit hex, 0600 perms, regenerates if deleted) with constant-time bearer comparison. `/api/traces/:id/replay` returns 501 placeholder pending M5.

## [0.0.0] - TBD

- v0 implementation in progress; no release tagged yet.
