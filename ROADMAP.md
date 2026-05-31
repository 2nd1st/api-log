# Roadmap

`api-log` uses [CHANGELOG.md](./CHANGELOG.md) for shipped work. This
file is the **forward view**: what we plan to do, what we know we
are *not* going to do, and what we are watching for.

Most of v0 and v0.1 has landed (capture path + read API + viewer +
plugin system); see CHANGELOG. The open work for the next cycle
falls in two buckets: a small set of v0.2 candidates surfaced by
operator use, and the v0.1.0-review deferred-items list (post-tag
hygiene). Neither blocks the `v0.1.0` tag.

---

## v0.2 candidates

### Bridge adapters (separate repos)

Optional per-gateway adapters that join external data into api-log
traces by `key_hash`. The core proxy stays gateway-agnostic; adapters
live as separate repos with their own release cycles.

Use cases the operator has surfaced:

- **CPA bridge** — join `CPA`'s Redis usage queue (per-request
  upstream metering) onto api-log traces. Lets the JSONL line carry
  the upstream-reported `prompt_tokens` / `completion_tokens`
  in addition to whatever the gateway forwarded to the client.
- **new-api bridge** — same shape against `new-api`'s MySQL log table.
- **Generic bridge interface** — any external store keyed by
  `key_hash` + `ts_range` is a candidate; the adapter writes
  side-car JSONL or appends columns to the SQLite mirror via a
  documented extension point.

These are not in the core repo's scope. Tag them `api-log-bridge-cpa`,
etc.

### Operator config knobs not in v0.1.0

- **Default path-filter pattern set.** Today operators install the
  `path-filter` plugin and enumerate patterns themselves. A
  conservative default for the common LLM gateway shapes (admin
  polling, auth refresh, billing telemetry) would cut first-run
  noise. Needs operator-side validation before shipping defaults.
- **Per-`key_hash` capture budget.** Not a gateway rate limit. A
  cap of the form "stop recording after `N` MB / day for this
  `key_hash`" so a single noisy client cannot fill the disk. Off
  by default; opt-in via YAML.
- **`/healthz` field configurability.** Some operators want the
  `/healthz` JSON to expose a subset of counters; others want
  everything. A `healthz_expose: [counter_names...]` knob keeps the
  default-all-fields behavior but lets adopters narrow it.

### Day-2 operations

The largest gap surfaced by the v0.1.0 review. Captured in detail in
the "v0.1.0 review — deferred items" section below; this is a
pointer for navigation.

- `api-log rebuild` subcommand (rebuild SQLite from JSONL)
- `api-log verify` integrity check
- Retention / pruning command
- Documented WAL checkpoint policy
- Backup procedure in ARCHITECTURE

### Observability the binary itself emits

api-log is a logging tool with no observability surface of its own.

- `/metrics` Prometheus endpoint (the existing `internal/counters/`
  package is a near-perfect fit; the work is wiring an encoder
  behind an unauth `/metrics` route)
- OTEL trace span per proxied request
- Plugin-chain latency histogram per `plugin.name`

### Wire-format and schema versioning

The `"JSONL is truth, SQLite is rebuildable"` invariant has no
implementation hook. v0.2 should add:

- `"v": 1` field on every JSONL line.
- `PRAGMA user_version` on SQLite + a `schema_version` table so
  `migrate()` does not detect "already applied" via error-string
  matching.
- `JSONL_FORMAT_VERSION` constant downstream tools can pin against.
- Documented backward-compat policy: readers MUST tolerate absent
  fields; writers MUST NOT remove fields, only add.

### Cross-platform binary distribution

Today's install path is Docker-only. The binary is pure Go and
builds for darwin/arm64 + darwin/amd64 + windows/amd64 + linux/arm64
without changes. Shipping is the missing piece:

- `goreleaser`-driven multi-arch binary releases
- Homebrew tap, scoop manifest, Chocolatey
- README install paths beyond Docker

---

## v0.1.0 review — deferred items

Output of a pre-release adversarial review (81 findings). The
Critical + Important items that blocked / belonged-in the v0.1.0
tag landed in commits `b6a8cdf` through the v0.1.0 prep window —
see CHANGELOG.md for the per-commit walk. What follows are items
the operator consciously deferred to post-v0.1.0 because they're
(a) larger than fits the tag window or (b) not
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

The "JSONL is truth; SQLite rebuildable" invariant has no implementation
today.

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
  index.sqlite → verify counts. The tooling for the "rebuildable
  from JSONL" promise does not exist today.
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
- Contrast-ratio statement for the muted-palette theme.

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

- `CONTRIBUTING.md` describing PR rules (test commands, restraint
  bar, code-style references).
- `CODE_OF_CONDUCT.md` (Contributor Covenant boilerplate).
- `ISSUE_TEMPLATE/` + PR template.
- Governance + maintainer note: "this is one contributor's
  project; sustainability is best-effort." Honesty over
  pretending-to-be-bigger.

---

## What we will not do

These are restraints, not omissions. They keep the project small.

- **No gateway features.** No auth, no routing, no retries, no
  rate limiting, no caching, no request rewriting. Those belong in
  the upstream gateway.
- **No SDK instrumentation.** api-log captures HTTP traffic; it
  does not replace Langfuse / Phoenix / LangSmith for in-process
  tracing.
- **No semantic interpretation of recorded content.** Cost
  calculation, model-family classification, "what's a frequent
  skill", evaluation pipelines — all downstream of the captured
  JSONL.
- **No automatic redaction in the capture path.** The bytes the
  client sent are the bytes recorded. If you need redacted JSONL,
  run a sidecar that rewrites it. The operator-opt-in plugin
  surface can mutate response content, but the capture path itself
  is byte-faithful.
- **No bundled "smart" middlebox behavior.** No prompt collision
  detection, no fake-cache injection, no content classification on
  the wire.
