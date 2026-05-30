# Changelog

All notable changes to this project will be documented here. Format:
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versioning:
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Wire-format
contracts (JSONL line schema, on-disk layout, HTTP read API) follow the
append-only / new-format-key migration discipline documented in
[PHILOSOPHY.md § principle 6](./PHILOSOPHY.md).

## [Unreleased]

### Added
- **W4.1 Phase 2 — client_project extraction** (2026-05-31, commit
  `3c6503d`): new `internal/parser/project.go` exports
  `ExtractProjectContext(systemPrompt string) *ProjectInfo` mirroring
  the viewer's `promptSource.extractProjectContext` (3 matchers in
  priority order: AGENTS.md injection ref + heading / CLAUDE.md
  injection ref + heading / first-heading-at-start). Writer calls it
  once at finalize on the extracted system prompt; populates
  `Row.ClientProject`. SQLite gains a nullable TEXT column
  `client_project` via idempotent ALTER TABLE; INSERT / select /
  exporter follow in lockstep (28 → 30 columns, the second column
  being W4.2's `client_version`). New `/api/traces?project=` filter
  joins the table for the viewer to scope a list to one project.
  rowJSON uses `omitempty` on ClientProject — the absence is the
  common case and the JSON stays tight when extraction yields nothing.
- **W4.2 — Plugin hot-reload** (2026-05-31, commit `e2212cf`):
  `PUT` / `PATCH` / `DELETE` on `/api/config/plugins` swap the live
  Registry atomically via `atomic.Pointer[instanceSnapshot]` —
  the operator no longer needs to restart `api-log` after editing
  plugin config. Reload is all-or-nothing (rollback on init error);
  startup tolerates a single bad instance (collect-and-continue) —
  shared `buildInstances` helper, different policies. Per-Iterate-call
  snapshot granularity is documented as accepted behavior: a Reload
  landing between a request's BEFORE and AFTER chain means BEFORE
  runs on the old config and AFTER on the new. Old instances are NOT
  Close()d on swap (a new instance might reference the same external
  resource as an old one; Close runs only on graceful shutdown).
  Rollback uses the pre-write LoadOverrides snapshot, not the
  handler's in-memory copy (SaveOverride does its own RMW). Tests:
  TestRegistry_AtomicReload exercises tight reader-loop + Reload
  under `-race`; PUT / PATCH / DELETE integration tests verify
  live-registry mutation + rollback path.
- **Plugin Phase B + C — contract + PHILOSOPHY amendments** (2026-05-30,
  commit TBD): `uiux-research/plugin-b-c-spec.md` frozen as the BUILD
  contract for the interfere-class hook surface (BEFORE / AFTER) layered
  on top of the Phase A.1 Observer scaffold. This commit is docs-only
  (no Go file touched) — it ships the PHILOSOPHY amendments the
  interfere-class surface requires before any code lands:
  PHILOSOPHY §2 gains a paragraph allowing operator-opt-in BEFORE/AFTER
  interference bounded by the two hook points (verbatim per spec §6.1);
  PHILOSOPHY §6 gains a paragraph documenting that plugin mutations are
  recorded post-mutation only — no pre/post diff retained (verbatim per
  spec §6.2); PHILOSOPHY "no" list `No configurable header / body
  redaction filters` rewritten to "in the capture path itself" with the
  plugin-path carve-out (verbatim per spec §6.4). ROADMAP gains §11
  Plugin Phase B + C and §5 cross-links. ARCHITECTURE §6.7 amended for
  the upcoming `/api/config/plugins` family (second write-side endpoint
  family in the read API, pattern-matching the §6.8
  `/api/config/media` precedent); ARCHITECTURE §13.4 added documenting
  the v1 carve-out: AFTER plugins may mutate `Content` / `Reasoning`
  only, and streaming `tool_call` argument mutation is deferred to
  Phase D per spec §10.6 (not cut — a future `ToolCallMutator` optional
  interface ships via Go's stdlib `io.WriterTo` / `io.ReaderFrom`
  evolution pattern when a real adopter use case surfaces). Go code
  lands in subsequent commits per spec §9 work packages W1–W6.
- **R5a — Client identification** (2026-05-30, commit `de44b28`): new
  `internal/parser/client.go` exports `ExtractClient(h trace.Headers) ClientInfo`
  with a 10-rule deterministic header-based classifier (claude-code-desktop,
  claude-cli, anthropic-sdk-py/ts, openai-sdk-py/ts, codex-cli, opencode-cli,
  browser, go-http-client). Writer calls it once at finalize; populates
  Row.ClientKind + Row.ClientVersion. SQLite gains two nullable TEXT
  columns via idempotent ALTER TABLE. No counters per-kind, no list-filter
  API expansion in this commit (restraint per `feedback_design_discipline`).
  Unblocks the T5 Landing "active clients" distribution panel.
- **Phase H** (2026-05-30, commit `492f1ad`): `total_bytes` cumulative counter on `/healthz` — bumped at JSONL append time so the read API can answer "how much have we recorded so far" without walking the data dir. (Token cumulative counters shipped subsequently in T3, commit `49e55bb`, below.)
- **Phase H** (2026-05-30, commit `96cf41a`): `clientAddrOf()` extracts the real client IP from `X-Forwarded-For` (leftmost = original client, RFC 7239) → `X-Real-IP` → `r.RemoteAddr` chain. Effective on traces where XFF actually reaches the backend; in the current homelab topology the placeholder evaluates to `127.0.0.1` because external HTTP is forwarded into the Caddy LXC via host loopback (incus port-forwarder / docker stack) — recovering the real IP needs PROXY protocol on the listener, separate infra task.
- **Phase I** (2026-05-29, commit `6294be2`): `internal/exporter` package + `GET /api/export` endpoint streaming a zip of matching JSONL lines bundled with `agent/CLAUDE.md` + `agent/jq-cheatsheet.md` + `README.md`. New `Store.AllMatching(filters, hardCap)` walks rows chronologically (ts_start ASC, id ASC) past the List() page limit — extracted `buildListConds` helper so the filter pipeline is single-sourced. `Deps.DataDir` added.
- **Phase J** (2026-05-30, commit `35acd7c`): Plugin Phase A scaffold — `internal/plugin/` package with the `Plugin` interface, `ObserveBeforeRecord` (shouldRecord gate) + `ObserveAfterRecord` (side-effect hook) discriminants, and `Registry`. First concrete plugin `internal/plugin/builtin/pathfilter/` matches `trace.Path` against glob patterns from `config.Plugins.PathFilter.Patterns` and drops matched traces. Scaffold-only — `cmd/api-log/main.go` does NOT construct the Registry yet; wiring is Phase A.1 (deliberate separation per `project_gate_position` memory + PHILOSOPHY §2 carve-out). `Config.Plugins.PathFilter` block + env binding added. Tests: table tests for the Registry's `IterateBeforeRecord` ordering invariant + pathfilter's glob behavior.
- **Phase J** (2026-05-30, commit `35acd7c`): export 5000-row cap removed end-to-end. `internal/exporter/exporter.go` dropped the `HardCap = 5000` const; `WriteZip` forwards the caller's `limit` straight to `AllMatching`. `Store.AllMatching` emits `LIMIT ?` conditionally — `hardCap=0` / negative means unlimited. `internal/api/export.go` `parseExportFilters` lifts the ceiling check on `limit` (only rejects `n < 1`).
- **T3.1 — Anthropic Messages SSE coverage** (2026-05-30, follow-up to
  T3): `extractMessages` now handles BOTH on-disk shapes. Body shape
  unchanged. New streaming shape walks `resp.events` in a single O(N)
  pass — first `message_start.data.message` yields model + input_tokens
  + cache_read_input_tokens + cache_creation_input_tokens; the most
  recent `message_delta.data` yields stop_reason +
  cumulative output_tokens; `message_stop` carries no usage and is
  skipped. Triggered by empirical observation on sub2api real traffic
  immediately after T3 shipped: 47 traces of `/v1/messages?beta=true`
  all showed NULL tokens because the dominant Anthropic traffic is
  streaming. PHILOSOPHY §1 compliant — every field path is a named
  Anthropic Messages SSE field; the extractor just dispatches on event
  name to find each named field. Tests: 4 new SSE test cases
  (happy-path with cache split + cumulative output_tokens, multi-delta
  last-wins, first-start-wins defensive, mid-stream-cut graceful
  degrade). Gemini `:streamGenerateContent` SSE remains out-of-scope
  until a Gemini operator surfaces with real samples.
- **T3 — usage extraction** (2026-05-30, commit `49e55bb`): new
  `internal/parser/usage.go` exports `ExtractUsage(trace.Trace) UsageInfo`
  with per-protocol field paths for Chat / Messages / Responses / Gemini;
  pointer fields throughout (nil != zero). Writer calls it once per
  `appendOne` at finalize; result populates Row.Model + Row.FinishReason
  + 5 token totals + Anthropic cache split, and bumps 5 new cumulative
  atomic counters (`total_prompt_tokens`, `total_completion_tokens`,
  `total_cached_tokens`, `total_cache_creation_tokens`,
  `total_reasoning_tokens`) surfaced on `/healthz`. SQLite gains
  3 nullable INTEGER columns via idempotent ALTER TABLE: `cached_tokens`,
  `cache_creation_tokens`, `reasoning_tokens`. INSERT extended from 25
  to 28 columns; `selectColumns` + `scanRow` extended in lockstep.
  Closes the gap where Row.Model / Row.PromptTokens / Row.CompletionTokens
  were declared but never populated — Dashboard TOP MODEL rendered `—`
  permanently and Overview MODEL BEHAVIOR carried no token data despite
  the upstream returning usage on every trace. Documented out-of-scope:
  Anthropic Messages SSE + Gemini `:streamGenerateContent` streaming bodies
  (contract names body paths only; event-path coverage would need a
  PHILOSOPHY §1 amendment).
- **T2 — Plugin Phase A.1 wiring** (2026-05-30, commit `4500a7d`):
  `cmd/api-log/main.go` constructs `plugin.NewRegistry()` alongside
  `mediaExt`; pathfilter registered + Init'd only when
  `cfg.Plugins.PathFilter.Patterns` is non-empty (startup INFO log
  prints active patterns — operator visibility per PHILOSOPHY §2);
  `IterateBeforeRecord` runs in finalize block immediately before
  `keyHash` compute + `writer.TrySend`; `shouldRecord=false` skips both
  TrySend and SQLite, cleans tmp files via
  `tmpDir.RemoveTraceFiles(traceID)`. Plugin errors log WARN but do not
  by themselves drop the trace (PHILOSOPHY §3 fail-open).
  `pluginReg.Close()` runs inline after `stopWriter()` in shutdown.
  Supersedes ROADMAP §4 "capture-time skip_paths" knob — the plugin
  carve-out makes the §2 boundary explicit. Known forward-looking
  follow-ups (non-blocking): no `drop_plugin_error` counter on /healthz;
  no `defer recover()` around plugin calls; no example yaml stanza in
  `deploy/dev-stack/`.
- **Phase K** (2026-05-30, commit `67142f9`): media extraction end-to-end. New `internal/media/` package walks parsed JSON bodies per protocol (chat / messages / responses / gemini); extracted images / audio / video / files write to `<DataDir>/<YYYY-MM-DD>/<keyhash[:8]>/media/<trace_id>/<idx>.<ext>`. `body_b64` explicitly NOT touched (operator clarification: unparseable JSON fallback is not an attachment). New `internal/runtime/` package persists operator-toggleable runtime overrides at `<DataDir>/runtime_overrides.json` (load order: hardcoded default → YAML → env → file → `PUT /api/config/media`). New `Config.Media.SaveAttachments` (default `true`). `internal/writer/writer.go` gains optional `mediaExt` + `mediaEnabled` params on `writer.New`; finalize calls `mediaExt.Extract` when the atomic flag reads `true` after the JSONL write succeeds. SQLite gains `media_count INTEGER DEFAULT 0` column (idempotent ALTER TABLE catching "duplicate column" specifically). `Counters.total_media_files` cumulative atomic surfaced on `/healthz`. New read API: `GET /api/media/{trace_id}/{idx}` streams the file with Content-Type from extension; `GET /api/config/media` returns `{save_attachments, source}`; `PUT /api/config/media` flips the atomic + atomic-renames the override file. `internal/exporter` bundles each row's media directory at `media/<trace_id>/<filename>` inside the zip. `agent/CLAUDE.md` + `agent/jq-cheatsheet.md` templates updated to mention the media/ layout.
- Initial design documents (PHILOSOPHY, ARCHITECTURE, README) — six review rounds.
- Verification experiments: X-Forwarded-For Go test, session-inference Python harness with synthetic + real-API scenarios.
- Project scaffold: Go module, package layout, CI workflow, hygiene files.
- **M1**: forwarding proxy + body capture pipeline. Bytes flow through to upstream; tmp files capture both directions; X-Forwarded-For suppressed via `Header[name] = nil`.
- **M2**: finalize parser + JSONL writer. Every completed trace lands at `data/<UTC-date>/<key_hash[:8]>.jsonl` matching the ARCHITECTURE § 3 schema. SSE parser handles all three shapes (data-only Chat / event-named Responses / event-named Anthropic) with `[DONE]` / `response.completed` / `message_stop` terminator recognition. JSON unmarshal for non-streaming bodies; `body_b64 + parse_error` fallback for non-JSON / parse failures. Content-Encoding decompression for `gzip` (stdlib); `br` / `zstd` graceful fallback. Daily rotation with background gzip workers. Writer goroutine + lossy `TrySend` channel.
- **M3**: SQLite mirror + session inference. New `internal/session` builds per-protocol session prefixes (Chat / Anthropic with `__apilog_system__` virtual turn / Responses with `input` normalization). New `internal/store/sqlite` uses modernc.org/sqlite (pure Go, no cgo) with WAL + NORMAL pragmas. Writer goroutine wraps JSONL append + SQLite INSERT + session inference IN-clause + UPDATE in a single transaction per trace (one fsync). `sync.Pool` for capture chunks reduces GC pressure under sustained load (sized for the 2000-user-team throughput target).
- **M4**: read API on a second listener. New `internal/api` mounts four endpoints: `GET /healthz` (status + uptime + atomic counters from `internal/counters`), `GET /api/traces` (SQLite-backed list with `since`/`until`/`status`/`model`/`key_hash`/`session_root_id` filters, opaque base64 `(ts_start_ms, id)` cursor pagination, default limit 100 cap 500), `GET /api/traces/:id` (SQLite row + JSONL seek by `jsonl_offset` — handles `.jsonl` and `.jsonl.gz` transparently since gzip preserves uncompressed offsets), `GET /` (JSON pointer to the separate viewer project). New `internal/admin` manages the `data/admin_token` file (256-bit hex, 0600 perms, regenerates if deleted) with constant-time bearer comparison. `/api/traces/:id/replay` returns 501 placeholder pending M5.
- **M5**: per-event `t_delta_ms` capture + `/replay` endpoint. Drainer records `(file_offset, wire_arrival_time)` per chunk; SSE parser tracks each event's first-byte offset; finalize zips the two via `capture.LookupChunkTime` (binary search). Encoded SSE (Content-Encoding != identity) skips timing capture — events get `t_delta_ms: null` per the §3 sentinel. `/replay` endpoint reconstructs SSE frames from the parsed `events[]` and emits at original pacing using `sleep = (t_delta_ms gap) / speed`; supports `?speed=N` (0..100], `?nodelay=1`. Null t_delta_ms in a gap means immediate emit. Returns 400 not_streaming for non-SSE traces.
- **M6**: production hardening. Stream-idle watchdog (`internal/proxy.StreamWatchdog`) cancels the request context if no response byte arrives for `stream_idle_seconds` (default 600s); fired traces are marked `disconnected: true`. `capture.Sink.OnByte` callback pulses the watchdog without coupling proxy code to the watchdog itself. Drainer-join timeout now marks the affected direction `truncated_*: true` (instead of just logging) so the JSONL line records the loss and `truncated_*_total` counters increment. Graceful shutdown ordering fixed: proxy.Shutdown → api.Shutdown → stopWriter (drains channel + waits gzip workers) → store.Close (explicit, no defer LIFO games) so the writer never sees a closed *sql.DB during its final flush. Chaos test verifies 500 concurrent producers against a wedged writer never block (max TrySend = 100ms on any goroutine).
- **M7**: shippable artifact + demo. Multi-stage `Dockerfile` produces a static distroless image; container runs as nonroot UID 65532 and pre-creates `/data` so the mount inherits ownership. New `tools/mockup` is a tiny LLM gateway that speaks all three protocols (non-streaming Chat, streaming Chat / Anthropic Messages / OpenAI Responses) — used to drive integration tests without an upstream key. `deploy/dev-stack/docker-compose.yml` brings up api-log + mockup against a bind-mounted data dir; `deploy/demo/docker-compose.yml` is the operator-facing layout (api-log fronting sub2api). `tests/integration/run.sh` exercises 16 assertions through the dev-stack: forwarding, JSONL persistence, SQLite list/cursor/detail, `/healthz` counters, cross-key isolation, session inference fires across two chat completions of the same key, replay pacing honors `speed`/`nodelay`, and the read API rejects unauth / wrong-token requests. GH Actions: a `release` job builds + pushes the image to `ghcr.io` on `v*` tags after the unit + integration suites pass.

## [0.0.0] - TBD

- v0 implementation in progress; no release tagged yet.
