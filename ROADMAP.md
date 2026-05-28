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

### 1. Export (highest priority)

- Batch export by filter (status / model / key_hash / session_root_id /
  time range — same surface as the viewer's filter sidebar).
- Output: a single `.zip` containing the matching JSONL line(s) at their
  original `data/<date>/<keyhash>.jsonl` paths.
- **The zip also bundles**:
  - `agent/CLAUDE.md` — operating instructions for a Claude agent who
    receives the zip ("here is what these JSONL lines mean, here is
    how to enumerate sessions, etc.").
  - Tiny helper scripts (jq snippets / Python one-liners) that index the
    bundled data for follow-up questions without needing to re-implement
    the read API.
- Implementation surface: new `internal/exporter` package + read API
  endpoint `GET /api/export?...` that streams the zip.
- Why it's #1: makes the recorded traffic actually useful for offline
  agentic analysis (skill-usage frequency, prompt clustering, regressions
  across model versions).

### 2. Operator config knobs

We have config plumbing already (`Config` struct + env + YAML). Need to
add a few surface-level toggles for production posture:

- `storage.archive_after_days` — gzip+move JSONLs older than N to a cold
  subdir; current behavior is "gzip on next-day rotation, then never
  touch", which is fine for now but won't be forever.
- `viewer.default_filters` — what the trace list shows when a fresh
  browser opens the viewer (e.g. exclude `/api/v1/*` admin UI noise on
  sub2 deployments).
- `api.public_query_enabled` — explicit toggle for "allow third-party
  clients to hit `/api/traces` with their own bearer". Default off.
  Today the admin token gates everything; this lets us hand out
  read-only tokens later.
- Other knobs go here as they're identified.

### 3. Detail-panel insight redesign

The current right panel shows raw views (overview / headers / body /
events / session / replay). They're faithful to the captured bytes but
**hard for humans to read** — they're optimized for "prove the recorder
worked", not for "help the operator understand what happened".

Discuss separately what the human-useful insights are. Candidates:

- "What did this request actually ask for?" → conversation summary
  with prompt + final answer + token cost + total duration on one line
- "Was this a retry / continuation?" → session position + parent diff
- "Why was it slow?" → first-byte vs total time vs upstream timings
- "What tools did the model use?" → tool_use blocks pulled to the
  surface
- "Did anything go weird?" → flag truncation, watchdog-fired, parse
  error, content-encoding mismatches inline

The current overview / headers / body tabs become a "raw" tab,
preserved for debugging; the new default tab is opinionated.

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

**Capture-time skip (medium, opt-in, still TODO)**

- New config: `capture.skip_paths: ["/api/v1/*"]` (default empty).
  Patterns matched against `req.URL.Path` before drainers are wired
  up; matched requests get forwarded normally but with a no-op sink,
  no JSONL line written, no SQLite row inserted.
- Tension with PHILOSOPHY principle 1 ("capture raw, derive
  structurally") — by skipping at capture time we're making a
  policy call. Mitigation: default empty + log a startup INFO line
  listing the active skip patterns so operators see exactly what
  isn't being recorded.
- Saves disk + SQLite churn on high-volume admin UIs; worth doing
  once a deployment has measurable noise (sub2api admin polls every
  60s per browser tab open).

### 5. Plugin / hook system (strategic, larger)

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

**Mutation-recording rule (load-bearing):**

If a plugin runs in `mutate_req` or `mutate_resp` and changes the
bytes, the JSONL line MUST carry BOTH the original and the mutated
form (e.g. a `mutations: [{plugin: "secret-redactor", before: <orig>,
after: <new>}]` block) — otherwise the recorder is lying about what
actually flowed, which breaks PHILOSOPHY § 6 ("filesystem is truth").
The default `req.body` / `resp.body` fields stay as **what reached
the upstream** / **what the upstream returned**; the original is
preserved in the mutations log.

This costs disk (we keep both versions for any mutated trace) but
it's the only honest implementation. Alternative — "just record the
mutated version" — would silently rewrite history and is rejected.

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

### 6. Unified dashboard

`Healthz` is one card grid today. It should be the home page of an
overall **Dashboard** tab that also surfaces:

- Volume over time (traces / minute, traces / day)
- Top models / paths / keys by volume
- Error rates (4xx, 5xx, dial errors) over time
- p95 latency trend
- Session creation rate
- Storage growth (data/ MB)
- Anomalies surfaced as cards (e.g. "drop_writer_full > 0 in last 5 min")

The existing `/healthz` JSON snapshot is still the source; the new
view aggregates time-series client-side from periodic polls OR (later)
a dedicated `/api/metrics?since=...` endpoint.

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
