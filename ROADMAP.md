# Roadmap to v0

Plan for shipping v0 in milestones. Each milestone is one to two commits
on `main`. Tests + smoke pass before moving on. M1 is done; remaining are
M2–M7 plus a demo / dev-stack infrastructure track.

---

## ✅ M1 — forwarding + body capture (DONE)

Commits: `649feed`, `faf5ca7`, `81678ec`. Forwarding works end-to-end;
both directions tee to per-trace tmp files; no JSONL or SQLite yet.

---

## M2 — finalize parser + JSONL writer

**Goal**: every completed trace lands in `data/<UTC-date>/<key_hash>.jsonl`
as a single line per ARCHITECTURE § 3, jq-queryable. Still no SQLite, no
session inference, no read API.

### Scope

- **`internal/sse`**: shape-agnostic line reader that emits `{event, data}`
  events. Three shape detections (data-only / event-named OpenAI Responses /
  event-named Anthropic). EOF semantics per ARCHITECTURE § 10.6 (flush
  pending accumulator). Returns `events []EventRecord` + `streamDone bool`.
- **`internal/parser`**: takes a tmp file + headers, returns the `req` /
  `resp` JSON shape (`body` | `events` + `stream_done` | `body_b64` +
  `parse_error`). Handles Content-Encoding decompression in memory; strips
  `Content-Encoding` from headers on success per ARCHITECTURE § 10.3.
- **`internal/jsonl`**: builds the JSONL line struct from request, response,
  and drain results; marshals to a single line.
- **`internal/writer`**: single-writer goroutine, channel-fed. Owns open
  JSONL file handles per `(date, key_hash[:8])` pair. Daily rotation logic
  (close + gzip-in-background + open new). `Append(record)` returns the
  pre-write byte offset for the caller (will be needed in M3 for the
  SQLite `jsonl_offset` column).
- **`cmd/api-log/main.go`**: replace M1's placeholder finalize with the
  real flow: parse request + response tmp files, build line, send to
  writer channel via non-blocking send, delete tmp files. Writer goroutine
  appends to disk. M2 does NOT include session inference; the writer
  goroutine's SQLite half is stubbed.

### Out of scope for M2

- SQLite (M3).
- Session inference (M3).
- `t_delta_ms` capture (M5).
- gzip random-access for old JSONLs (M4 / read API).

### Verification

- Unit tests: SSE parser table-driven over fixtures (real `experiments/`
  captures + hand-crafted edge cases). JSONL marshal golden tests.
- E2E smoke: same mock upstream as M1, but now assert a JSONL line exists
  at `data/<today>/<key_hash>.jsonl` with expected shape.
- Race: `go test -race -count=1 ./...` clean.

### Risk

- Daily rotation race (ARCHITECTURE § 7.4) needs careful single-writer
  contract. Test by setting `TZ=UTC` + `faketime` style (or pass a clock
  interface to the writer for tests).
- Compressed response body parsing: gzip easy, br/zstd less so. Default
  to identity-only in M2; fall back to `body_b64 + parse_error` for
  unknown encodings. Decompression library decisions noted in PR.

---

## M3 — SQLite mirror + session inference

**Goal**: SQLite `index.sqlite` exists, mirrors JSONL rows, populates
session inference fields. Cross-day session walking works.

### Scope

- **`internal/store/sqlite`**: schema migration (single `CREATE TABLE`
  + indexes from ARCHITECTURE § 4), WAL mode + pragmas, prepared
  `INSERT` / `UPDATE` statements.
- **`internal/session`**: session prefix construction per protocol
  (Chat → `messages`, Anthropic Messages → `[__apilog_system__] +
  messages`, OpenAI Responses → `[__apilog_system__] + normalize(input)`).
  Canonical-form hashing helpers. `FindParent(tx, key_hash, prefix)`
  returns `(parent_id, session_root_id)` via the IN-clause query of
  ARCHITECTURE § 5.2.
- **`internal/writer` (extended)**: the same single-writer goroutine
  now does, in order per trace: `JSONL append → SQLite mirror INSERT
  → session inference query → SQLite UPDATE with parent_id and
  session_root_id`. All in one SQLite write connection.
- **Startup rebuild**: if `index.sqlite` is missing or empty, scan
  `data/**/*.jsonl{,.gz}` in chronological order and replay all rows
  through the writer's INSERT+inference path.

### Out of scope for M3

- Read API (M4).
- `t_delta_ms` (M5) — derived columns leave `t_delta_ms` aside; the
  writer's `events[]` carry `null` until M5.

### Verification

- The `experiments/session-inference/algo_test.py` scenarios but now
  driven against the real Go session inference code, not the Python
  prototype. Same expected `parent_id` / `session_root_id` outcomes.
- Cross-day test: synthetic JSONL spanning two daily files, verify a
  follow-up trace on day N+1 attaches to a day N parent.
- Rebuild test: populate JSONL files, delete SQLite, restart, assert
  full index restored.

### Risk

- SQLite WAL contention with read-side handlers (will appear in M4).
  M3 is writer-only; M4 introduces concurrent readers — defer the test
  until then.
- Long sessions: benchmark to confirm the §5.2 IN-clause stays under
  ~1 ms p99 with 10⁵ traces. Already validated synthetically in
  `experiments/session-inference/benchmark.py` (in-memory); now needs
  real-SQLite confirmation.

---

## M4 — read API minimal

**Goal**: the documented `/api/traces` list, `/api/traces/:id` detail,
`/healthz` (with counters), and `GET /` from ARCHITECTURE § 6 all work.

### Scope

- **`internal/api`**: HTTP handlers for the four endpoints, all backed
  by SQLite + optional JSONL seek. Bearer-auth middleware with
  constant-time comparison. Error contract per ARCHITECTURE § 6.1.
- **`internal/counters`**: package-level atomic counters for the
  `/healthz` body; producers in `internal/writer` and `internal/capture`
  bump them.
- **Admin token lifecycle**: read or generate `data/admin_token` at
  startup; print to stdout once if newly generated.
- **JSONL seek on gzipped files**: streaming forward to `jsonl_offset`
  in the uncompressed stream (acceptable O(offset) for v0 per § 6.3).
- **cmd/api-log/main.go**: bind a second http.Server on the API listener
  with the read API routes. Two listeners total.

### Out of scope for M4

- `/replay` endpoint (M5).
- Per-stream idle / req-body timeouts (M6).

### Verification

- Unit tests for each handler with a fixture SQLite + JSONL pair.
- Auth: missing token, wrong token, valid token → 401 / 401 / 200.
- Cursor pagination round-trip test.
- E2E: run api-log against mock upstream, send N requests, then GET
  /api/traces and verify N rows in correct order.

---

## M5 — `/replay` endpoint + `t_delta_ms` capture

**Goal**: per-event `t_delta_ms` is recorded on the live path; the
`/replay` endpoint re-emits with original pacing. This is the Langfuse-
incomparable differentiator (README headline).

### Scope

- **`internal/capture`**: drainer now does SSE frame-boundary detection
  (`\n\n`) and records timestamps from the right chunk per ARCHITECTURE
  § 7.1 step 2. Compressed responses skip timing capture (sentinel
  `null` per § 10.6).
- **`internal/sse` (extended)**: parser preserves the per-event
  `t_delta_ms` from drainer-recorded timings when materializing
  `events[]` at finalize.
- **`internal/api`**: `/api/traces/:id/replay` handler. Speed validation
  (`(0, 100]`), null-fallback to immediate emit, `?nodelay=1`,
  `Content-Type: text/event-stream`.

### Verification

- Unit test for the speed math (`sleep = delta / speed`).
- Null fallback test: synthetic trace with null `t_delta_ms` → replay
  emits all events with no sleeps.
- E2E: capture a real sub2api stream, replay it back at `speed=10`,
  verify event order + count match capture, total wall time scales
  inversely with speed.

---

## M6 — production hardening

**Goal**: timeouts, graceful shutdown, idle-stream watchdog, all
finalize trigger paths fire correctly under chaos.

### Scope

- **Stream-idle watchdog**: per-stream timer reset on each forwarded
  byte; on timeout, cancel request context → triggers finalize via
  ctx-cancel path.
- **`req_body_capture_timeout`** wired into finalize's `sync.Once`
  trigger set.
- **Graceful shutdown ordering**: ARCHITECTURE § 7.5 fully implemented
  (stop accept → drain forwarding → close writer chan → drain writer →
  wait gzip workers → exit).
- **Failure mode tests**: chaos harness that injects each failure
  mode from § 7.3 and asserts the documented behavior.
- **Counter wiring audit**: every drop / overflow path actually
  increments the documented counter.

---

## M7 — demo docker-compose + integration tests

**Goal**: `docker compose up` brings up api-log + a real LLM gateway,
client traffic gets recorded. Reproducible smoke for new contributors.

### Scope

- **`deploy/docker-compose.yml`**: api-log + sub2api (and/or cpa) +
  mock upstream chain. Persistent volume for `data/`.
- **Integration test suite**: scripted curl drives against the compose
  stack; asserts on JSONL output, SQLite rows, replay timing.
- **GitHub Action**: builds the image, runs the integration suite,
  publishes to ghcr.io on tag.

### Source repos for M7

- **sub2api**: <https://github.com/Wei-Shaw/sub2api> — build from this
  source (or pull a published image if available); add as a service in
  `deploy/docker-compose.yml`.
- **CPA**: image source TBD; revisit in M7.

---

## Infrastructure track (parallel, not blocking milestones)

These are dev-experience items that can land any time after M2:

- **`deploy/dev-stack/`** — a minimal docker-compose for local dev
  (mock upstream + api-log built from source). Used by integration
  tests in M2+.
- **Mock upstream**: a small Go program in `tools/mockup/` that mimics
  OpenAI Chat / Responses / Anthropic Messages SSE flows from canned
  fixtures, so tests don't depend on real-network LLM calls. Will
  consume the captured wire format we already verified in
  `experiments/`.
- **Real-API smoke**: a script that points api-log at the user's
  sub2api.homelab.lan and runs through one Chat, one Responses, one
  Anthropic Messages trace, asserting the JSONL output matches the
  expected structure. Cost: pennies per run.

---

## Estimated effort

Solo + AI assistant, part-time evenings (per the OSS contributor review):

| Milestone | Estimated LOC | Estimated time |
|---|---:|---:|
| M2 finalize + JSONL writer | ~1500 | 1–2 weeks |
| M3 SQLite + session inference | ~1200 | 1–2 weeks |
| M4 read API | ~1000 | 1 week |
| M5 replay + t_delta_ms | ~800 | 1 week |
| M6 hardening | ~500 + tests | 1–2 weeks |
| M7 compose + integration | ~300 + YAML | 1 week |
| **Total** | **~5300 + tests** | **6–10 weeks** |

This is in the ballpark the OSS contributor predicted (6–9 k LOC for v0).
Each milestone is shippable on its own: M2 → "we have a JSONL recorder",
M3 → "you can query sessions", M4 → "frontend can render", etc.

---

## ✅ v0 status

M1 – M7 all merged. Two live deployments on the homelab:

| Deployment | Stage | Notes |
|---|---|---|
| **sub2gpt** (`sub2gpt.homelab.lan`, viewer at `apilog-sub2gpt.homelab.lan/viewer/`) | iterate here | Every UI/feature change builds and rolls here first. Safe to break. |
| **sub2api** (`sub2api.homelab.lan`, viewer at `apilog-sub2.homelab.lan/viewer/`) | observe only | Live production traffic flows through api-log. **Do not auto-deploy here**; pull/rebuild only after the change has settled on sub2gpt. Roll back = one Caddyfile line back to `:8080`. |

Diagnostic signals added on top of the M4 counters: per-stage timing
histograms (drain / parse / sqlite ms with p50/p95/p99), status-bucketed
appends, upstream dial-error counter, slow-trace WARN log, periodic
counter snapshot at INFO every 60s.

---

## Post-v0 — known asks (no order, no commitment)

These came out of operator feedback after sub2gpt + sub2api went live.

### ✅ 1. Export (DONE — Phase I, 2026-05-29, commits `6294be2` backend + `db49249` viewer)

Backend `internal/exporter` package + `GET /api/export` endpoint stream a
zip of matching JSONL lines bundled with `agent/CLAUDE.md` + `agent/jq-cheatsheet.md`
+ `README.md`. Filter pipeline reuses `ListFilters` from
`internal/store/sqlite`. The 5000-row safety cap (Phase I) was removed in
Phase J (commit `35acd7c`) — `Store.AllMatching(filters, hardCap)` with
`hardCap=0` means unlimited. Files whose source JSONL contained both
matching + non-matching lines land as `<keyhash>.partial.jsonl` so the
recipient can tell the file isn't a complete day.

Phase K (2026-05-30, commit `67142f9`) extended the zip with
`media/<trace_id>/<idx>.<ext>` directories alongside each row when the
trace's extracted media files exist on disk — see §7 below.

Viewer Export page at `#/export` mirrors the FilterSidebar field shapes;
filter inputs include datalist autocomplete for path/model/key_hash
populated from a one-shot `/api/traces?limit=200` sample (Phase J,
commit `db49249`). Generate uses `authFetch → Blob → synthesized <a download>`
rather than `location.href` because navigation can't attach the Bearer
header `/api/export` requires.

Contract: `docs/specs/phase-i-export-contract.md`.

### 🟡 2. Operator config knobs (PARTIAL)

- ✅ **`media.save_attachments`** — Phase K (2026-05-30, commit `67142f9`).
  YAML default `true`; runtime override via `PUT /api/config/media` persisted
  to `<DataDir>/runtime_overrides.json` (loaded at startup AFTER yaml/env).
- ✅ **`viewer.default_filters`** — Partial: viewer reads
  `localStorage['apilog.default_path']` (default `/v1/*`) since Phase F.
  Viewer Settings page (Phase I, 2026-05-29) exposes the edit; backend
  doesn't push defaults to the viewer (operator preference, see
  api-log-viewer/PHILOSOPHY.md §6 — composable, not authoritative).
- ⏳ **`storage.archive_after_days`** — gzip+move JSONLs older than N to a cold
  subdir; current behavior is "gzip on next-day rotation, then never
  touch", which is fine for now but won't be forever. Not built.
- ⏳ **`api.public_query_enabled`** — explicit toggle for "allow third-party
  clients to hit `/api/traces` with their own bearer". Default off.
  Today the admin token gates everything; this lets us hand out
  read-only tokens later. Not built.
- Other knobs go here as they're identified.

### ✅ 3. Detail-panel insight redesign (DONE — Phase F → H, 2026-05-29/30)

Largely shipped on the viewer side (api-log-viewer repo) — the backend's
read-side contract is unchanged.

- ✅ Block-native conversation (text / reasoning / tool_call / tool_result /
  media / error / unknown) — Phase F, commit `f6cb518` (viewer).
- ✅ Per-block renderers and tool_call ↔ tool_result pairing by id —
  Phase F.
- ✅ Tab strip slimmed from 7 → 4 → 3 (Phase F → G → H): conversation /
  overview / raw; events / session / replay UI surfaces retired (backend
  `/api/traces/:id/replay` endpoint preserved for scripting). Phase H
  commit `4688137` (viewer).
- ✅ Overview enriched — CLIENT & SOURCE (UA classification +
  prompt-source detector), CONTENT SHAPE (block-type chips, tool
  inventory, response shape), MODEL BEHAVIOR (stop reason translation
  + reasoning count + tool count + first-reply latency) — Phase H.
  MODEL BEHAVIOR's cache + reasoning token fields and the SQLite-backed
  Dashboard TOP MODEL card became populatable with real data after
  T3 (2026-05-30, commit `49e55bb`) extracted `usage` from response
  bodies at finalize. Pre-T3 the columns were declared on the Row
  schema but never populated; Dashboard rendered `—` for model
  permanently. See §8 below for the protocol field paths.
- ✅ Reasoning tombstones (no "encrypted_content — plaintext not
  available" placeholder wording; the absence of body communicates the
  redacted-by-upstream state) — Phase F+H polish.
- ✅ XML-structured prompt rendering (codex `<personality>` etc;
  Anthropic system prompts) — Phase H.

The current overview / headers / body tabs became a single "raw" tab,
preserved for debugging; the new default tab is opinionated (overview).

### 4. Path noise filter (small, useful immediately)

Operators surfaced 2026-05-28: the trace list is polluted by sub2api's
own admin-UI calls (`/api/v1/auth/me?...`, `/api/v1/admin/...`,
`/api/v1/subscriptions/active`, etc.). These have nothing to do with
LLM traffic — they're the gateway's own dashboard polling itself.

Two layers (do the cheap one first):

**~~Display-side default filter~~ (DONE 2026-05-28, commit `5cbdda1`)**

- Backend: `/api/traces?path=...` supports exact match by default and
  prefix match when the value ends with `*` (e.g. `path=/v1/*`).
  Special-case: `path=*` alone disables the filter entirely.
- Viewer: path input pre-populated with `DEFAULT_PATH_FILTER` (read
  from `localStorage['apilog.default_path']`, defaulting to `/v1/*`).
  Clear button resets to the default, not empty.
- Future settings UI just edits that localStorage key — zero
  follow-up code change needed there.

Open follow-up (cosmetic): recorded path currently includes query
string (`/api/v1/auth/me?timezone=...`). For UI grouping this could
be split — `path` = bare URL path, separate `query` field. Defer to
the recorder schema bump pass.

**~~Capture-time skip~~ (✅ DONE — Plugin Phase A.1, 2026-05-30, commit `4500a7d`)**

Originally proposed as `capture.skip_paths` config knob. The Plugin Phase A
scaffold (§5) added `internal/plugin/builtin/pathfilter/` which implements
the same semantic via the `ObserveBeforeRecord` hook. Phase A.1 (this
commit) wires `Registry` into `cmd/api-log/main.go` finalize block, gated
on `config.Plugins.PathFilter.Patterns` non-empty — operators now get
path-skip via the plugin config block instead of a bespoke
`capture.skip_paths` knob:

```yaml
plugins:
  path_filter:
    patterns:
      - /api/v1/*
      - /health
```

Same mitigation story: default empty + startup INFO line listing active
patterns. PHILOSOPHY §1 tension acknowledged — the plugin form makes the
"capture never interferes" carve-out explicit + reviewable.

### 5. Plugin / hook system (strategic, larger)

**Phase B + C status (in flight 2026-05-30): contract frozen in
`docs/specs/plugin-b-c-spec.md`; build underway. Adds the
interfere-class BEFORE / AFTER hooks alongside the Phase A Observer
class. See §11 below for the in-flight entry and the supersession
note: the original five-hook sketch in `docs/specs/plugin.md` is
OBSOLETE for greenfield work.**

**Phase A.1 status (2026-05-30, commit `4500a7d`): scaffold + wiring shipped.**

Design lives in `docs/specs/plugin.md` (the contract). Phase A
(commit `35acd7c`) landed the scaffold — `internal/plugin/` (Plugin +
ObserveBeforeRecord + ObserveAfterRecord + Registry) plus the first
concrete plugin `internal/plugin/builtin/pathfilter/` plus the
`plugins:` config block. Phase A.1 (commit `4500a7d`) wired the
Registry into `cmd/api-log/main.go` — pathfilter patterns from
`config.Plugins.PathFilter.Patterns` now actually drop matched traces
from JSONL + SQLite at finalize, via `IterateBeforeRecord` between
`buildTrace` and `writer.TrySend`. Startup logs the active patterns
(operator visibility per PHILOSOPHY §2). Plugin errors fail-open
(slog.Warn, capture continues); the registry closes inline after
`stopWriter()` in the shutdown sequence.

Phase A is **observe-class only**: no `mutate_req`, no `before_forward`,
no gate behavior. Those are the gate-position phases (B and C in
plugin.md § 8) and remain designed-but-not-built per
`project_gate_position`. Known Phase A.1 follow-ups (forward-looking,
non-blocking): no `drop_plugin_error` counter on /healthz; no
`defer recover()` around plugin calls (a panicking third-party plugin
would leak tmp files but the upstream response IS already forwarded so
PHILOSOPHY §2 is honored); no example yaml stanza in `deploy/dev-stack/`.

Phase A.1 closes the planned ROADMAP § 4 "capture-time skip" TODO
through `path-filter` instead of a bespoke `capture.skip_paths` knob.

The original sketch below remains the long-form record of what hooks
are designed (interfere-class included) and how mutation-recording
would work — none of it ships in Phase A. See `docs/specs/plugin.md`
§§ 7.2, 7.3 for the Phase B (`key-redactor`) and Phase C (`rate-limit`)
designs, and §§ 8 Phase B / Phase C for the PHILOSOPHY amendments each
phase requires.

---

api-log sits at the natural gate position in front of LLM gateways.
The operator pointed out 2026-05-28 that this position is useful
beyond just recording — rate-limit, DDoS-resistance, audit, secret
redaction, alert routing, etc.

But: today's capture path is intentionally non-interfering. Adding
gate-style behavior risks violating PHILOSOPHY § 2 ("capture never
interferes"). The right shape is a **plugin system** with explicit
hook points and OFF-by-default behavior.

Sketch of hook points (in order of trace lifecycle):

| Hook | Can do | Cannot do (philosophy) |
|---|---|---|
| `before_forward(req)` | inspect; reject with 4xx; rate-limit; rewrite headers (rare); short-circuit response | block capture itself — if it fires, what would have been the trace still gets a partial line |
| `mutate_req(req)` ⚠️ | inject system prompt; redact secrets; substitute model names; A/B prompt variants | silently lie about what was sent — see "mutation-recording rule" below |
| `before_record(trace)` | skip recording (path filter use case); add operator tags | block forwarding |
| `mutate_resp(resp)` ⚠️ | strip PII from output; rewrite refusals; transform formats | silently lie about what came back — see "mutation-recording rule" below |
| `after_record(trace)` | push to external sinks; emit alerts; update operator dashboards | retroactively edit recorded JSONL |

**Mutation-recording rule — SUPERSEDED:**

The original sketch above (keep BOTH original and mutated form in a
`mutations: [...]` block, reject "just record the mutated version")
has been overturned. See the PHILOSOPHY §6 amendment + `docs/specs/plugin-b-c-spec.md`
§5: plugin mutations are recorded **post-mutation only**, with no
separate pre/post diff retained. The JSONL line carries what actually
flowed; the operator opted in to the mutation and accepts the
post-mutation recording as the trace of record. Intercepted traces
carry a `plugin_intercepted` marker; plugin errors carry an optional
`plugin_errors` breadcrumb. The pre/post `mutations[]` block is not
shipped and is not on the roadmap.

Likely-useful plugin categories (operator notes 2026-05-28):

- **gate**: rate-limit, IP / key blocklists, request-shape validators
- **filter**: skip recording for high-noise paths (sub2api admin UI),
  exclude probe/healthcheck traffic
- **inject**: system prompt prefix, tool whitelist enforcement,
  default-parameter overrides (e.g. cap `max_tokens`)
- **redact**: strip PII / secret patterns from request OR response
  before they hit the recorder OR before they leave api-log
- **route**: A/B model selection, fallback chains, canary deploy gating
- **alert**: trigger external notification on patterns
  (`status >= 500`, "user said X", token-spend threshold)
- **transform**: format conversion (e.g. inbound Chat → outbound
  Responses for an upstream that only speaks one shape)

Most of these are user-territory; few will ship in the in-tree plugin
bundle. The first one we'd actually need is probably **filter**
(§ 4 path-noise) since the live deployment is already noisy.

Implementation surface (sketch):

- `internal/plugin` package with `Plugin` interface and a registry.
- Built-in plugins shipped in-tree but disabled by default:
  - `pathskip`: maps to (4) above.
  - `ratelimit`: per-key_hash token bucket with operator-tunable
    rates; rejects with `429 + Retry-After`.
  - `defaultfilter`: pushes a default viewer filter; pure config,
    no runtime hook.
- Third-party plugins out of scope for now — Go's plugin story is
  painful (CGO, build-coupling). If demand emerges, evaluate
  in-process Lua / wasm / sidecar-RPC then.

Why bundle these in-tree instead of "scripting": because the gate
position is sensitive (a buggy plugin breaks the LLM path). We want
review + tests for each plugin we ship. Operators get behavior via
config, not by uploading Go files.

### ✅ 6. Unified dashboard (DONE — Phase H, 2026-05-30, viewer commit `4688137`)

Shipped on the viewer side as the **Landing** page (replaces the old
"Dashboard" name per operator request; route `#/landing`). Reads only the
existing `/healthz` + `/api/traces?since=...` endpoints — no new
backend API.

Surfaces (in render order):
- **STATUS strip** — backend live/stalled, last poll, **data dir total bytes**
  (from `counters.total_bytes`, Phase H, commit `492f1ad`), uptime,
  this-week count via paginated `/api/traces?since=monday`.
- **CAPABILITY strip** — protocols recognized by `adapt()` lit when at
  least one trace in the last hour exercised them.
- **NEEDS ATTENTION** — last 30 min of 5xx / slow / truncated / upstream-
  dial-error rows, clickable to drill in.
- **VOLUME** — 60-min traces/min SVG sparkline derived client-side from
  loaded rows (no chart library — restraint memory).
- **INTERNAL · healthz** — collapsible by default; the old card grid
  preserved for diagnostic spelunking; auto-expands when any warn/err
  counter is non-zero.

Backend kept simple per ARCHITECTURE — no `/api/metrics?since=...`
aggregation endpoint added; the viewer aggregates client-side.

`total_media_files` (Phase K) joins `total_bytes` as a healthz cumulative
counter; both surface in the Landing STATUS strip.

T3 (2026-05-30, commit `49e55bb`) added five further cumulative counters —
`total_prompt_tokens`, `total_completion_tokens`, `total_cached_tokens`,
`total_cache_creation_tokens`, `total_reasoning_tokens` — sourced from
the per-trace usage extraction at finalize. These power the Landing
"recent token activity" KPIs whose values were previously hardcoded to
0 because writer didn't extract usage. (Subject to the §6 healthz
endpoint policy decision — counters live in memory regardless of
whether `/healthz` surfaces them; the Landing data source may shift to
SQLite aggregates over the new columns.)

### ✅ 7. Media extraction (DONE — Phase K, 2026-05-30, backend `67142f9` + viewer `77fd9ca`)

A late post-v0 addition discovered while building the Export page.

Backend extracts images / audio / video / files from PARSED JSON bodies
(`req.body` + `resp.body`) and writes them to disk alongside the JSONL
line. `body_b64` is explicitly NOT an attachment (operator clarification
2026-05-30 — unparseable JSON fallback container, not a user file).
Default `media.save_attachments: true`.

- `internal/media/` package: per-protocol walkers (chat / messages /
  responses / gemini) → MediaFile metadata + file written to
  `<DataDir>/<YYYY-MM-DD>/<keyhash[:8]>/media/<trace_id>/<idx>.<ext>`.
- `internal/runtime/` package: `Overrides` JSON persisted at
  `<DataDir>/runtime_overrides.json`. Load order: hardcoded default →
  YAML → env → runtime_overrides.json → `PUT /api/config/media`.
- SQLite gains `media_count INTEGER DEFAULT 0` column (idempotent ALTER
  TABLE).
- `Counters.total_media_files` cumulative atomic, surfaced on `/healthz`.
- Read API gains `GET /api/media/{trace_id}/{idx}` (streams the file),
  `GET /api/config/media` (returns current value + source), `PUT
  /api/config/media` (flips the atomic + persists).
- Exporter bundles each row's `media/<trace_id>/` directory at
  `media/<trace_id>/<filename>` inside the zip.

PHILOSOPHY adherence: §1 carve-out 1 — extracting named protocol fields
is a deterministic transform, no heuristic synthesis. §2 — extraction
runs AFTER JSONL is on disk; failure logs WARN, never blocks forwarding.
§6 — JSONL still carries the b64; extracted files are a derived
filesystem cache.

Contract: `docs/specs/phase-k-media-contract.md`.

### ✅ 8. Usage extraction (DONE — T3, 2026-05-30, commit `49e55bb`)

Closes a gap noticed by the operator 2026-05-30: Row.Model /
Row.PromptTokens / Row.CompletionTokens were declared on the SQLite
schema but never populated, so Dashboard TOP MODEL rendered `—`
permanently and Overview MODEL BEHAVIOR carried no token data despite
the upstream returning a usage block on every trace.

New `parser.ExtractUsage(t trace.Trace) UsageInfo` extracts named
protocol fields (PHILOSOPHY §1 carve-out 1: deterministic copy, no
synthesis):

- **Chat** (`/v1/chat/completions`):
  `usage.{prompt_tokens, completion_tokens, total_tokens,
  prompt_tokens_details.cached_tokens,
  completion_tokens_details.reasoning_tokens}`; model from resp body
  (fallback req body); finish_reason from `choices[0]`.
- **Messages** (`/v1/messages`, Anthropic):
  `usage.{input_tokens, output_tokens, cache_read_input_tokens,
  cache_creation_input_tokens}`; model from resp body; `stop_reason` as
  finish_reason. The Anthropic cache split is preserved as two distinct
  SQLite columns (`cache_read` = hit, `cache_creation` = miss sent for
  caching) — collapsing them would synthesize information PHILOSOPHY
  forbids.
- **Responses** (`/v1/responses`, OpenAI): both streaming
  (`events[-1].data.response.usage`) and non-streaming
  (`resp.body.usage`) shapes;
  `input_tokens_details.cached_tokens` +
  `output_tokens_details.reasoning_tokens`; model from req body
  (resp echoes resolved upstream names like `gpt-4o-2024-11-20` which
  would silently diverge from the contract).
- **Gemini** (`/v1beta/models/<NAME>:generateContent`):
  `usageMetadata.{promptTokenCount, candidatesTokenCount,
  cachedContentTokenCount}`; model from path regex; finishReason from
  `candidates[0]`. No reasoning tokens (protocol doesn't expose them).

SQLite gains three nullable INTEGER columns via idempotent ALTER TABLE
(same pattern as Phase K media_count): `cached_tokens`,
`cache_creation_tokens`, `reasoning_tokens`. Existing `model`,
`prompt_tokens`, `completion_tokens`, `total_tokens`, `finish_reason`
columns finally get populated.

`Counters` gains five cumulative atomics — `total_prompt_tokens`,
`total_completion_tokens`, `total_cached_tokens`,
`total_cache_creation_tokens`, `total_reasoning_tokens` — bumped at the
same finalize callsite where Row fields are filled (one ExtractUsage
call serves both). Surfaced on `/healthz` today; subject to §6's
healthz endpoint policy decision.

**Anthropic Messages streaming SSE** (`message_start` carries model +
input-side usage including cache_read / cache_creation; final
`message_delta` carries stop_reason + cumulative output_tokens) was
added in a follow-up commit immediately after T3 shipped, once real
sub2api traffic showed it was the dominant protocol and the original
"body-only" scope left NULL counters on the operator's actual traffic.
This stays §1-compliant: every field path is named by the Anthropic
SSE spec; the contract just dispatches on which event kind to read each
named field from.

**Still out-of-scope (PHILOSOPHY §1):** Gemini `:streamGenerateContent`
SSE — no real samples in tree yet, deferred until a Gemini-using
operator surfaces.

Tests: 21 parser table cases (incl. zero-vs-absent, body_b64 fallback,
Responses dual-shape, model-comes-from-request-not-response for
Responses); SQLite round-trip (populated + absent branches); writer
integration smoke. go test -race ./... green across 19 packages.

PHILOSOPHY adherence: §1 named-field-only extraction; §2 finalize-time
work never blocks forwarding (parser failures log WARN, traces still
land); §6 columns are rebuildable from JSONL by replaying the same
ExtractUsage function.

### ✅ 9. Client identification (DONE — R5a, 2026-05-30, commit `de44b28`)

Headers-only classifier surfaces "which SDK / CLI / browser sent the
request" as new columns `client_kind` and `client_version` on the
SQLite row. Backend-only; viewer aggregations land in T5.

Taxonomy (first match wins, inspected at finalize off `req.headers`):

| Kind | Discriminator | Version source |
|---|---|---|
| `claude-code-desktop` | `Anthropic-Client-Platform: desktop_app` AND `Anthropic-Beta` contains a `claude-code-*` token | `Anthropic-Client-Version` |
| `claude-cli` | `User-Agent` starts with `claude-cli/` | UA version suffix |
| `anthropic-sdk-python` | `x-stainless-package-version: anthropic@*` AND `x-stainless-runtime: python` | package version |
| `anthropic-sdk-ts` | same package, `x-stainless-runtime: node` | package version |
| `openai-sdk-python` | `x-stainless-package-version: openai@*` AND `x-stainless-runtime: python` | package version |
| `openai-sdk-ts` | same package, `x-stainless-runtime: node` | package version |
| `codex-tui` | `User-Agent` starts with `codex-tui/` (OpenAI terminal UI) | UA version suffix |
| `codex-cli` | `User-Agent` starts with `codex/` | UA version suffix |
| `opencode-tui` | `User-Agent` starts with `opencode-tui/` | UA version suffix |
| `opencode-cli` | `User-Agent` starts with `opencode/` | UA version suffix |
| `browser` | `User-Agent` starts with `Mozilla/` | nil |
| `go-http-client` | `User-Agent` starts with `Go-http-client/` | UA version suffix |
| (none) | no rule matches | both nil |

PHILOSOPHY §1 carve-out 1: named headers only, no body content sniff, no
heuristic UA library imported. New client kinds add a row to the table;
the dispatch is a straight switch, no ambiguity-tiebreaker logic. No
per-kind counters, no `?client_kind=` list-filter knob in this commit
([[feedback_design_discipline]] — restraint).

### 10. Cross-day session continuity (CONFIRMED 2026-05-30)

✅ confirmed 2026-05-30 — operator OK'd day-split scheme; see
ARCHITECTURE §5.5 Status callout. No code change.

### 11. Plugin Phase B + C (IN FLIGHT 2026-05-30)

Adds the interfere-class hook surface on top of the Phase A.1 observer
scaffold. Contract is frozen in
`docs/specs/plugin-b-c-spec.md` — read that first.

Two hot-path hook points only: **BEFORE** (post-receive, pre-forward)
and **AFTER** (post-upstream-response, pre-client-send). Each hook
plugin returns `Continue` / `Mutate` / `Intercept`. Intercept can carry
any HTTP status (200, 4xx, 5xx) and any body. Phase A's Observer class
(`pathfilter`) stays as a separate third class — Observers run
post-finalize, pre-writer, and can only drop recording.

MVP plugins shipping in the first BUILD commit (spec §7):
- `text-replace` (BOTH hooks, multi-direction): literal substring
  match/replace on request/response content. Subsumes the operator's
  upstream `"你" → "世界上最好最好的ai"` + downstream
  `"<needle>" → "世界上最好的好哥哥"` examples.
- `text-append` (BOTH hooks, multi-direction): append fixed suffix to
  last user message / system prompt / assistant content / reasoning.
  Subsumes the original `watermark` + `prompt_inject` placeholder
  names without locking in single-direction plugin types.
- `rate_limit_ip` is deferred post-MVP — the operator wants it, but
  the two text plugins prove the contract first.

Trace recording semantics (spec §5, ratified):
- **Post-mutation only** — no separate mutation log. The JSONL line
  carries what actually flowed. Trade audit detail for simpler trace
  shape; PHILOSOPHY §6 amended in this commit.
- **Intercepted traces ARE recorded** with a top-level
  `plugin_intercepted: {type, id, hook}` marker so operators can tell
  a plugin-served 403 apart from a genuine upstream 403.
- **Plugin error breadcrumbs** as a small optional `plugin_errors`
  array — the only audit channel for plugin failures at disk level.

Fail / panic semantics (spec §4): hooks have no `error` return value;
plugins log via `slog` and return `ActionContinue` on trouble. The
dispatcher wraps each call in `defer recover()` so a panicking plugin
becomes a WARN log + ActionContinue — capture and forwarding continue.
Closes ARCHITECTURE §13.3 partially (the v1 contract resolves the
"no `defer recover()` around plugin calls" item for hot-path hooks; the
Observer-class call site picks up the same wrapper as part of W5).

Multi-instance + persistence (spec §3): operator declares plugin
instances in `config.yaml` as `(type, id, enabled, config)` tuples;
runtime overrides extend `runtime_overrides.json` with a `plugins`
block that **full-replaces** the YAML list (no merge-by-id). New
endpoints `GET/PUT /api/config/plugins`, `PUT /api/config/plugins/{id}`,
`DELETE /api/config/plugins`, `GET /api/plugins/types` — the second
write-side endpoint family in the read API, pattern-matching the §6.8
`/api/config/media` amendment. Atomic registry rebuild via
`atomic.Pointer[Registry]`; failed Init on a new instance does NOT
swap and surfaces the error to the viewer UI.

v1 carve-out (spec §10.6, 4-lens adversarial ratification): AFTER
plugins MAY READ `ParsedResponse.ToolCalls` but MUST NOT MUTATE
streaming `tool_call` argument fragments. Tool_call argument mutation
on the AFTER hook is **deferred to Phase D**, not permanently cut —
when (if) a real adopter files an issue, Phase D ships as a separate
optional `ToolCallMutator` interface detected by type assertion (Go
stdlib `io.WriterTo` / `io.ReaderFrom` evolution pattern), so v1
`BeforePlugin` / `AfterPlugin` implementations stay untouched. See
ARCHITECTURE §13.4 for the deferred-tool_call note.

PHILOSOPHY amendments landed in this pass (verbatim per
plugin-b-c-spec.md §6):
- §2 "Capture, never interfere" — new paragraph allowing
  operator-opt-in BEFORE/AFTER interference, bounded by the two hook
  points, recorded post-mutation.
- §6 "The filesystem is the truth" — new paragraph documenting that
  plugin mutations are NOT separately recorded; trade-off explicit.
- "No" list — `No configurable header / body redaction filters` rewritten
  to "in the capture path itself", carving out the plugin path with
  faithful post-mutation recording.

Work package plan (spec §9): W1 framework + dispatcher, W2 MVP
plugins, W3 overrides + API, W4 viewer Settings UI (separate repo,
follow-up workflow), W5 Phase A migration
(`ObserveBeforeRecord` → `ObserveOnFinalize`,
`ObserveAfterRecord` → `ObserveAfterWrite`; `pathfilter` is the only
in-tree consumer — single-commit migration per spec §12.5), W6 docs.
This commit is W5 — the docs amendments themselves. Go code lands in
subsequent commits.

Supersedes the original `docs/specs/plugin.md` five-hook sketch for
greenfield work. Cross-link from §5 above.

---

## v0.1.0 review — deferred items

Output of the 10-lens cross-repo adversarial review (workflow
`wv354526x`, 81 surviving findings — full punch list at
[docs/reviews/v0.1.0-pre-release.md](./docs/reviews/v0.1.0-pre-release.md)).
Critical + Important items that block / belong-in the v0.1.0 tag
landed in Buckets A–C (commits `b6a8cdf` through Bucket C). What
follows are items the operator consciously deferred to post-v0.1.0
because they're (a) larger than fits the tag window or (b) not
contract-breaking. None block the tag; each is a real follow-up.

### Release engineering

- **Multi-arch image.** Current `docker/build-push-action@v6` ships
  linux/amd64 only — Apple Silicon, RPi, Graviton get
  `exec format error`. Add a `platforms:` key + buildx QEMU step,
  publish at least `linux/amd64` + `linux/arm64`.
- **Supply chain.** No `provenance: true`, no SBOM step (e.g.
  `anchore/sbom-action`), no cosign signature. Distroless base
  pinned by tag (`gcr.io/distroless/static-debian12:nonroot`) not
  by sha256 digest — pin by digest, refresh deliberately.
- **THIRD_PARTY_NOTICES.** `modernc.org/sqlite` (BSD-3) and
  `gopkg.in/yaml.v3` (Apache-2.0) impose notice-preservation. Add
  a NOTICE / THIRD_PARTY_NOTICES.md generated from go.sum + the
  npm tree on the viewer side.
- **Dependency hygiene.** Neither repo ships `.github/dependabot.yml`
  or Renovate config. Add minimal dependabot for `gomod` + `npm` +
  `github-actions`, weekly cadence.
- **Reproducibility.** Dockerfile has `-trimpath` (good) but no
  `SOURCE_DATE_EPOCH`; image digest varies across rebuilds of the
  same git tag.
- **`govulncheck` + `pnpm audit` CI steps.** Both absent today; with
  the floating distroless tag this means silent base churn.
- **`RELEASING.md`.** README announces "v0.1.0 tag is being prepared"
  with no published checklist. ~10 lines describing: version bump
  surfaces (CHANGELOG, viewer README Status, dockerfile arg),
  tag conventions, ghcr publish path, smoke-test before announce.

### Wire-format + schema versioning

PHILOSOPHY § 6's "JSONL is truth; SQLite rebuildable" invariant has
no implementation today.

- No `"v": 1` on the JSONL line.
- No `PRAGMA user_version` or `schema_version` table on SQLite.
- `migrate()` detects "already applied" via string-matching error
  messages (sqlite.go:138, 156, 175, 192) — fragile across driver
  upgrades.
- No `JSONL_FORMAT_VERSION` constant downstream tools can pin
  against.
- Downgrade is impossible because columns can't be dropped.

Plan a v0.2 schema-versioning Work Package: set `PRAGMA user_version`
on `migrate()`, stamp every JSONL line with `"v": <int>`, document a
backward-compat policy ("readers MUST handle absence of any field;
writers MUST NOT remove fields, only add").

### Day-2 operations

- `api-log rebuild` subcommand: walk JSONL → drop + recreate
  index.sqlite → verify counts. PHILOSOPHY § 6 promises this is
  rebuildable; the tooling doesn't exist.
- `api-log verify`: integrity check (every SQLite row's jsonl_path
  + jsonl_offset resolves to a non-empty line; every JSONL line
  has at least one SQLite row OR is documented partial-line).
- Retention / pruning: no `apilog retain --before 30d` or similar.
  A 2000-user-fleet adopter at month 6 has an unbounded data dir.
- Disk-full circuit breaker beyond `DropJSONLFail`: a back-pressure
  signal that switches capture to header-only or to a tmpfs spool.
- WAL checkpoint policy: no manual `PRAGMA wal_checkpoint(TRUNCATE)`
  anywhere; low-traffic instances may grow `index.sqlite-wal`
  unboundedly.
- Backup procedure documented in ARCHITECTURE / README:
  `sqlite3 .backup` + JSONL tar.

### Observability the binary emits

api-log is a logging-and-observability tool with no observability
surface of its own.

- `/metrics` Prometheus endpoint (the counters in
  `internal/counters/` are already a near-perfect fit; add a
  Prometheus encoding behind a `/metrics` route that's
  unauth-gated like `/healthz`).
- OTEL exporter for proxy-side trace spans (one span per request
  with attrs: path, status, model, client_kind, duration, slow).
- Plugin-chain latency histogram per `plugin.name`.

### Configuration UX

- `api-log validate <path>`: parse + reject with line-pointing
  error message before the binary touches the network.
- `api-log.example.yaml` at repo root (currently buried in
  deploy/dev-stack/).
- JSON Schema for editor autocomplete (generated from the
  `Config` struct's yaml tags).
- `--config-help` flag listing every key + env override + default.
- README "Configuration" section enumerating env vars + YAML
  keys (today the only authoritative reference is Go struct tags).

### HTTP server hardening completion

Bucket B added `recoverMW` + `http.MaxBytesReader` on PUT plugins.
The read API still ships with:

- No `MaxHeaderBytes` on the read API listener (apiSrv).
- No `ReadTimeout` on the read API. (The proxy's `WriteTimeout=0`
  is correct for SSE; the read API doesn't need that exception.)
- The proxy listener is intentionally permissive (operator-restricted
  at the network layer); the read API is admin-bearer-gated so its
  hardening profile should be tighter.

Single-commit follow-up: set apiSrv ReadTimeout=15s, WriteTimeout=
30s, MaxHeaderBytes=64KB; document the proxy carve-out.

### Cross-platform binary distribution

- No Homebrew tap, no scoop manifest, no Chocolatey, no goreleaser
  multi-arch binary. The pure-Go binary builds for darwin/arm64,
  darwin/amd64, windows/amd64, linux/arm64 with no source changes;
  shipping is the missing piece.
- README install path is Docker-only despite the "one binary"
  pitch.

### Multi-instance correctness

- No `instance_id` mixed into the ULID generation. Two binaries
  pointed at the same `data/` produce collision-prone IDs.
- No advisory lock on `data/index.sqlite` (modernc.org/sqlite
  shares the file mutex but two writers across processes is
  undefined).
- README / ARCHITECTURE don't state the single-node constraint
  explicitly. Add to ARCHITECTURE Limitations section.

### Adopter "first 5 minutes" UX

- 60-second `docker compose up; curl proxy; curl api` story.
  Today `deploy/demo/` requires cloning sub2api as a sibling.
  `deploy/dev-stack/` is hidden behind the "dev" label.
  `deploy/README.md` (added in Bucket C) helps; an asciicast or
  GIF would complete the story.
- Glossary explaining trace / session / key_hash /
  prefix_canonical_hash / session_root_id.
- Annotated `api-log.example.yaml` at the repo root.

### Viewer — accessibility (a11y)

- Semantic landmarks audit (header / main / nav).
- Focus trap on modals (PluginEditModal, CommandPalette,
  AuthModal).
- Documented Escape handler on CommandPalette.
- Skip-to-content link.
- axe-core CI step.
- Contrast-ratio statement for the muted-palette theme (the
  operator's Linear/Raycast-leaning aesthetic explicitly chose low
  contrast — needs a measurement, not a defense).

### Viewer — Important findings not in Bucket C

- ConversationTab session-view N sequential awaits → backend batch
  endpoint (`?session_root_id=…&include=full`) eliminates per-trace
  JSONL open overhead. The L9 perf finding that motivated Bucket
  B's SQLite conn pool fix; finishing the chain needs the batch
  endpoint.
- ConversationTab `authFetch` is imported directly instead of
  threaded via prop (breaks the DI pattern every other tab uses).
- Trace ID interpolated unencoded into URLs in ConversationTab +
  OverviewTab (theoretical today because ULIDs are alphanumeric;
  inconsistent with PluginList which does encode).
- Export.svelte 8-input filter subscription via manual
  `void [f_status, f_path, …]` — brittle on add; replace with
  `$derived qsKey`.
- RawTab re-runs `jsonHL` over the full body on every i18n label
  change; language toggle locks UI on large traces.

### Minor backend nice-to-have

- Generated admin token printed to **stdout** today; survives in
  journald + docker logs. Switch to stderr with a pointer at
  `data/admin_token`; document journaling risk in SECURITY.md.
- No CORS / standard security headers (X-Content-Type-Options:
  nosniff, Referrer-Policy: no-referrer, Permissions-Policy: empty,
  explicit Access-Control-Allow-Origin). Single middleware.
- `Watchdog.Pulse` calls `time.Timer.Reset` on every byte chunk →
  runtime timer-lock contention on dense SSE. Coalesce: only Reset
  if `lastPulse >= timeout/4` ago.
- `ListSessions` correlated subqueries scan the table 3× per
  request. Add `CREATE INDEX idx_session_ts ON traces(session_root_id,
  ts_start DESC)`, or refactor to a single self-join.
- Filter columns (`path`, `model`, `client_kind`, `status`,
  `client_project`) have no per-column index; SQLite scans
  `idx_ts` for every filtered list. Add `idx_path_ts` and a
  partial `idx_project_ts` `WHERE client_project IS NOT NULL`.
- `parser.finalize` does `io.ReadAll` of the full body twice on
  the failure path — GC pressure under burst load.

### Process / governance

- CONTRIBUTING.md describing PR rules (test commands, restraint
  bar, code-style references).
- CODE_OF_CONDUCT.md (Contributor Covenant boilerplate).
- ISSUE_TEMPLATE/ + PR template.
- Governance + maintainer note: "this is one contributor's
  project; sustainability is best-effort." Honesty over
  pretending-to-be-bigger.

---

## Iteration model

- **Local repo (`/Volumes/leoyun/personal-projects/api-log-project`)**:
  source of truth. All changes start here.
- **Gitea (`leoyun/api-log`)**: pushed on each non-trivial commit. The
  `/opt/api-log` clones on sub2gpt + sub2api pull from here.
- **sub2gpt deployment**: `cd /opt/api-log && git pull && cd
  /opt/sub2gpt && docker compose build --quiet api-log && docker
  compose up -d api-log`. Iterate freely.
- **sub2api deployment**: pull + rebuild **only after** the change has
  been validated on sub2gpt or by integration tests. This is live
  traffic; don't break it casually.
