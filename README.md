English | [中文](README.zh.md)

# api-log: LLM proxy logging and API trace recorder

![api-log — tcpdump for LLM gateways](./docs/banner.png)

[![CI](https://github.com/2nd1st/api-log/actions/workflows/ci.yml/badge.svg)](https://github.com/2nd1st/api-log/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/2nd1st/api-log?include_prereleases&sort=semver)](https://github.com/2nd1st/api-log/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/2nd1st/api-log)](./go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

api-log is a transparent HTTP recording proxy for LLM gateway observability. It sits between clients and OpenAI-compatible or Anthropic Messages gateways, forwards traffic unchanged, records each completed request/response trace as append-only JSONL, and builds a SQLite index for local search, replay, and analysis.

The forwarding goroutine never inspects bodies. JSON unmarshalling, SSE event splitting, and session inference happen in a finalize step after the response has been delivered to the client. Token accounting, evaluation pipelines, and any semantic interpretation are downstream of this project's scope.

## Status

Preparing the `v0.1.0` tag. The capture path, read API, viewer (separate repo), and plugin system (Phase A observer + Phase B/C mutators) are shipped and running against live traffic. The HTTP read API contract is stable; pre-tag commits may still rebase.

See [ARCHITECTURE.md](./ARCHITECTURE.md) for the on-disk format and read API contract, and [ROADMAP.md](./ROADMAP.md) for what's queued.

## vs alternatives

| Tool | What it is | Why api-log is different |
|---|---|---|
| Helicone | Gateway / observability stack | api-log is only a transparent recorder; no routing, billing, auth, or hosted service. |
| Langfuse | App-level LLM tracing platform | api-log captures HTTP traffic at the gateway boundary without SDK instrumentation. |
| Phoenix | Evaluation / tracing / observability toolkit | api-log records raw gateway traces first; eval pipelines are downstream. |
| LangSmith | LangChain tracing/eval platform | api-log is framework-agnostic and stores JSONL + SQLite locally. |
| mitmproxy | General interactive proxy | api-log understands LLM JSON/SSE envelopes and writes structured traces. |

api-log is not a gateway. It does not authenticate, route, retry, rate-limit, cache, or rewrite. Those live in the upstream gateway.

## Quick start

### Docker Compose

```yaml
# docker-compose.yml — added alongside your existing gateway
services:
  gateway:                                  # CPA / sub2api / new-api / your stack
    # ... existing config ...
    expose: ["7860"]                        # move 7860 from "ports" to "expose"

  api-log:
    image: ghcr.io/2nd1st/api-log:latest
    ports:
      - "7861:7861"                         # proxy listener (clients connect here)
      - "7862:7862"                         # read API
    environment:
      APILOG_PROXY_UPSTREAM: http://gateway:7860
    volumes:
      - ./api-log-data:/data
```

```bash
docker compose up -d
```

Three reference stacks live under [`deploy/`](./deploy/README.md) — `dev-stack/` (api-log + a mock LLM gateway, no real upstream needed), `demo/` (api-log in front of `sub2api`), and `bench/` (api-log alone, upstream URL via env). For a 5-minute try-it, run `deploy/dev-stack/`; that's what [`tests/integration/run.sh`](./tests/integration/run.sh) drives.

### Point clients at api-log

Change the client `base_url` from the gateway port (`:7860`) to the api-log proxy listener (`:7861`). No other client changes.

### Verify a captured trace

```bash
# 1. Liveness — unauthenticated, k8s probe compatible
curl -s http://localhost:7862/healthz | jq .

# 2. Send one request through the proxy (replace with a real client call)
curl -s http://localhost:7861/v1/messages \
  -H "x-api-key: $UPSTREAM_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}'

# 3. Read the auto-generated admin bearer
TOKEN=$(cat ./api-log-data/admin_token)

# 4. List recent traces from the read API
curl -s -H "Authorization: Bearer $TOKEN" \
  'http://localhost:7862/api/traces?limit=5' | jq '.traces[] | {id, path, status, model}'
```

The admin bearer is generated on first run and written to `data/admin_token`. Delete the file and restart to rotate.

## How recording works

### Traffic path

```
client(s)  →  api-log  →  CPA / sub2api / new-api / any OpenAI-compatible gateway  →  upstream
                ↓
          data/<date>/<key_hash>.jsonl    (append-only, source of truth)
          data/index.sqlite               (derived index, rebuildable)
```

The proxy listener accepts plain HTTP. Forwarding uses `httputil.ReverseProxy` with a custom `Transport` that tees request and response bodies into per-trace temp files. When the response finishes (success, client disconnect, or upstream error), the finalize step parses the bodies into the JSONL line shape, writes one line to today's JSONL file, and inserts the indexed columns into SQLite. See [ARCHITECTURE § 7](./ARCHITECTURE.md) for the full write path.

### Storage model

Two layers, in strict order of authority:

1. **`data/<date>/<key_hash>.jsonl`** — one line per completed trace. Append-only daily file per client key. Each line carries the full HTTP transaction (request headers + body, response headers + body or `events[]` for streams, timestamps, sizes, truncation flags).
2. **`data/index.sqlite`** — derived columns the read API needs (status, model, token counts, session linkage, `jsonl_path` + `jsonl_offset`). Deletable; rebuilt from layer 1 in seconds. WAL-mode, conn pool of 8.

When the JSONL file and SQLite disagree, JSONL wins.

### JSONL trace shape

```json
{
  "id": "01HX7K8MS...",
  "ts_start": "2026-05-27T10:23:45.123Z",
  "ts_end":   "2026-05-27T10:23:46.357Z",
  "client":   "172.17.0.5:54321",
  "method":   "POST",
  "path":     "/v1/messages",
  "upstream": "http://gateway:7860",
  "status":   200,
  "req": {
    "headers": {"x-api-key": "sk-***", "anthropic-version": "2023-06-01"},
    "body":    {"model": "claude-sonnet-4-6", "messages": [...], "stream": true}
  },
  "resp": {
    "headers": {"content-type": "text/event-stream", "x-request-id": "..."},
    "events":  [
      {"event": "message_start",       "data": {"message": {"id": "msg_...", ...}}, "t_delta_ms":  12},
      {"event": "content_block_delta", "data": {"delta": {"text": "Hello"}, ...},   "t_delta_ms": 234},
      {"event": "message_delta",       "data": {"usage": {"output_tokens": 8}},     "t_delta_ms": 511},
      {"event": "message_stop",        "data": {},                                  "t_delta_ms": 514}
    ],
    "stream_done": true
  },
  "disconnected":   false,
  "truncated_req":  false,
  "truncated_resp": false
}
```

Header values shown above are redacted for documentation; the on-disk JSONL contains the raw bytes the client sent. See [Security](#security) below.

The full field reference lives in [ARCHITECTURE § 3](./ARCHITECTURE.md).

## Protocol coverage

api-log forwards every HTTP request byte-faithfully. The finalize step parses bodies for a known set of LLM API shapes; unrecognized paths still record headers + raw body but skip the structured fields.

- **OpenAI Chat Completions** (`/v1/chat/completions`) — request `messages[]`, response `choices[]`, both streamed (`data: {...}\n\n` chunks) and non-streamed.
- **OpenAI Responses** (`/v1/responses`) — request `input[]`, response `output[]`, including SSE events such as `response.output_item.added` for tool-call extraction.
- **Anthropic Messages** (`/v1/messages`) — request `messages[]` + `system`, response `content[]`, SSE events `message_start` / `content_block_delta` / `message_delta` / `message_stop`.
- **SSE streams** — each `event:` / `data:` pair is split into one entry in `resp.events[]` with `t_delta_ms` recorded against the response start. Non-SSE responses land as parsed JSON in `resp.body`.
- **After-mutation capture for streams** — when plugins mutate a streaming response (Phase B/C), api-log records the bytes the client actually received, not the upstream pre-mutation stream.

Tool calls, reasoning blocks, and any other content these protocols carry live verbatim inside `req.body` and `resp.body` / `resp.events[]`. They are not promoted to top-level fields and api-log does not interpret them.

## Query examples

The examples below are reference snippets, not benchmarked workloads. Run them against your own data.

### jq over JSONL

Tool-call frequency across captured Responses-API traces:

```bash
zcat data/2026-*/*.jsonl{,.gz} 2>/dev/null \
  | jq -r '.resp.events[]? | select(.event=="response.output_item.added")
           | .data.item | select(.type=="function_call") | .name' \
  | sort | uniq -c | sort -rn
```

Streams that were cut short:

```bash
jq 'select(.disconnected==true or .resp.stream_done==false)
    | {id, path, status, ts_start}' \
  data/2026-05-27/*.jsonl
```

### sqlite3 over the index

Recent failing traces by model and path:

```bash
sqlite3 data/index.sqlite \
  "SELECT ts_start, model, path, status FROM traces
   WHERE status >= 400
   ORDER BY ts_start DESC LIMIT 20"
```

All traces in one session, in order:

```bash
sqlite3 data/index.sqlite \
  "SELECT id, jsonl_path, jsonl_offset FROM traces
   WHERE session_root_id='01HX7K...' ORDER BY ts_start"
```

The `jsonl_path` + `jsonl_offset` pair lets a consumer seek directly to the line in the JSONL file. AI agents doing batch analysis can bypass the read API and read JSONL files off disk.

### Replay a recorded SSE stream

```bash
curl -N -H "Authorization: Bearer $TOKEN" \
  "http://localhost:7862/api/traces/01HX7K8MS.../replay?speed=2"
```

`/api/traces/:id/replay` re-emits the recorded SSE frames at original per-chunk pacing (or `speed` × faster). `speed=2` halves the delays; `nodelay=1` dumps every event back-to-back. The replay is to the API caller; it never re-contacts the upstream LLM. See [ARCHITECTURE § 6.4](./ARCHITECTURE.md) for the full semantics, including how reparsed traces handle missing `t_delta_ms`.

## Read API

The read API listens on a separate port (`:7862` by default) from the proxy. Thirteen routes total; the full surface lives in [ARCHITECTURE § 6](./ARCHITECTURE.md). Highlights:

- `GET /healthz` — unauthenticated; exposes in-memory drop / overflow counters so operators can detect capture degradation without grepping logs.
- `GET /api/traces` — list, SQLite-backed, supports `since` / `until` / `status` (exact or `2xx` / `4xx` / `5xx` bucket) / `model` / `path` (trailing `*` for prefix) / `key_hash` / `session_root_id` / `project` / `limit` / cursor pagination.
- `GET /api/traces/:id` — detail; returns `{row, trace}` so callers get both the SQLite row and the parsed JSONL line in one round trip.
- `GET /api/traces/:id/replay` — pacing-preserving SSE replay.
- `GET /api/sessions` — session summaries grouped by `session_root_id`. Row-level aggregation only; not a tree walk.
- `GET /api/export` — streams a zip of matching JSONL lines + bundled `agent/CLAUDE.md` for offline / AI-assisted analysis.
- `GET/PUT /api/config/plugins` (+ `PUT /api/config/plugins/:id`, `DELETE /api/config/plugins`, `GET /api/plugins/types`) — hot-reload of `text-replace` / `text-append` / `path-filter` plugins. YAML remains the declarative truth; runtime overrides persist to `data/runtime_overrides.json`. Off by default.

All read endpoints except `/healthz` require `Authorization: Bearer <data/admin_token>` and respond with `Cache-Control: no-store`.

api-log ships **no embedded HTML viewer**. `GET /` returns a JSON pointer to the separate [api-log-viewer](https://github.com/2nd1st/api-log-viewer) project; the binary contains zero HTML.

## Bundled viewer

api-log ships hosted viewer at `/viewer/` by default. On first request, the backend fetches `dist.zip` from a pinned release of [`api-log-viewer`](https://github.com/2nd1st/api-log-viewer), verifies the asset's SHA-256 against a constant baked into the backend binary, and extracts the bundle to a cache under `data/viewer-cache/`. Subsequent requests serve from the cache.

A SHA-256 mismatch is fatal to the route — `/viewer/` returns 503 and the binary logs the mismatch. The backend never serves an unverified asset.

Override knobs (env or YAML):

| Knob | Default | Effect |
|---|---|---|
| `APILOG_VIEWER_ENABLED` | true | Set false to skip hosting; `/viewer/` returns 503 `disabled`. |
| `APILOG_VIEWER_REPO` | `2nd1st/api-log-viewer` | Point at any GitHub repo. |
| `APILOG_VIEWER_VERSION` | pinned to the backend release | Tag to fetch. |
| `APILOG_VIEWER_SHA256` | pinned to the backend release | Must be supplied when overriding REPO or VERSION; mismatch returns 503. |
| `APILOG_VIEWER_LOCAL_PATH` | (unset) | Absolute path to a `dist/` directory. Skips fetch + verify entirely; useful for offline / air-gapped deployments and for local viewer development. |
| `APILOG_VIEWER_CACHE_DIR` | `<data_dir>/viewer-cache` | Cache root. |
| `APILOG_VIEWER_PUBLIC_PATH` | `/viewer` | URL prefix. |

The backend never auto-updates the cached viewer — fetch happens once per (repo, version, sha) tuple. To roll forward, bump the backend release (which bumps the pinned constants) or override `APILOG_VIEWER_VERSION` + `APILOG_VIEWER_SHA256` explicitly.

## Security

**Bearer tokens land on disk unredacted.** The JSONL files contain the raw `Authorization` / `x-api-key` headers exactly as the client sent them. api-log does not redact anything from the capture path. Treat the `data/` directory the way you would treat `~/.ssh/` or a file holding production API keys:

- Mount `data/` to a path with restrictive filesystem permissions (`chmod 700` or tighter).
- Do not expose the proxy listener (`:7861`) or the read API listener (`:7862`) to untrusted networks without a reverse proxy enforcing transport security and access control.
- The auto-generated admin bearer at `data/admin_token` is the only credential gating the read API. Rotate it by deleting the file and restarting.
- Plain HTTP between containers on the same host is the primary supported topology. If you need TLS or cross-host routing, terminate it with whatever reverse proxy you already run; api-log itself listens HTTP only.

Redaction is a deliberate non-goal of the capture path. If you need redacted traces, run a sidecar over the JSONL files; the on-disk format is documented and stable.

See [SECURITY.md](./SECURITY.md) for the threat model and disclosure process.

## Development

```bash
git clone https://github.com/2nd1st/api-log.git
cd api-log

# unit + integration tests (race detector on)
go test ./...

# lint (golangci-lint v2)
golangci-lint run

# build
go build -o bin/api-log ./cmd/api-log

# the dev-stack integration harness
./tests/integration/run.sh
```

The project ships 23 Go packages, race-clean tests, and CI lint via `golangci-lint v2`. CI runs on every push and PR; see [`.github/workflows/ci.yml`](./.github/workflows/ci.yml).

## Roadmap

- [x] **v0** — capture path (parse + JSONL write + SQLite mirror), session inference, minimal read API.
- [x] **v0.1 viewer** — [api-log-viewer](https://github.com/2nd1st/api-log-viewer) — multi-instance aggregation, session-tree visualization, tool-call rendering, SSE replay.
- [x] **v0.1 plugins** — `text-replace` / `text-append` / `path-filter` with hot-reload via `PUT /api/config/plugins`. Off by default.
- [ ] **v0.2** — optional per-gateway **bridge adapters** (separate projects) — join external data (CPA's Redis usage queue, new-api's MySQL log table) into api-log traces by `key_hash`. The core proxy stays gateway-agnostic.

See [ROADMAP.md](./ROADMAP.md) for the full list including the `v0.1.0-deferred` section.

## Acknowledgements

### Design influence

- **tcpdump / pcap** — append-only capture with deferred interpretation
- **CLIProxyAPI (CPA)** — single-binary, single-config aesthetic
- **Claude Code / Codex CLI** — local JSONL session files as a usable format
- **Langfuse** — the LLM-observability surface, against which we deliberately differ on the capture-vs-instrument axis

### Live-traffic iteration partner

- **sub2api** — primary upstream gateway used to validate the capture path against live traffic; the path-filter pattern set and the client-identification taxonomy were tuned against its real-traffic shapes.

### Development assistance

This codebase and its documentation were developed with **Claude Opus 4.8** (Anthropic) as the primary pair-programmer for both code and prose, and **GPT-5.5** via Codex CLI as a research and review assistant — adversarial pre-release review, README structural analysis against reference OSS projects, and fact-check cross-checks. The choices on what to keep, cut, or amend are the human author's; AI assistance is named here for transparency, not as authorship.

## License

[MIT](./LICENSE) — © 2026 Leo Yun.
