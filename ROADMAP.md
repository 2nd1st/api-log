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

### Open questions for M7

- Is there an official sub2api docker image we can pull, or do we need
  to bake one? The user already runs sub2api at `http://sub2api.homelab.lan`
  — clarify image source.
- CPA image source.

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
