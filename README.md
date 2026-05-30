# api-log

A transparent recording proxy for any OpenAI-compatible LLM gateway. One binary. Drop it in front of your gateway. Get a structured JSONL log of every request, response, tool call, and reasoning chunk — without touching client code.

---

## One-line pitch

**tcpdump for LLM gateways.** Sits in front of CPA / sub2api / new-api. Captures every Chat Completions / Responses / Anthropic Messages request, parses the JSON envelope and the SSE event stream into one JSONL line per trace, indexes it in SQLite for instant frontend queries, and gets out of the way. Tool calls, reasoning, and any other content the protocol carries live verbatim inside the captured `req` / `resp` objects — they are not promoted to top-level fields and we do not interpret them.

What you do with the recordings — token accounting, eval pipelines, skill extraction, AI-assisted ad-hoc analysis — is downstream of this project's scope.

---

## Status

**Pre-release.** v0 (capture path + session inference + read API) and v0.1 work (frontend viewer in a separate repo, plugin system, hot-reload) are shipped and running against live traffic. A v0.1.0 tag is being prepared; until then the API contract is stable but commit history may still rebase.

Read [PHILOSOPHY.md](./PHILOSOPHY.md) first, [ARCHITECTURE.md](./ARCHITECTURE.md) second. [ROADMAP.md](./ROADMAP.md) tracks what's done and what's queued.

---

## What this is

```
client(s)  →  api-log  →  CPA / sub2api / new-api / any OpenAI-compatible gateway  →  upstream
```

Two storage layers, in strict order of authority:

1. **`data/<date>/<key_hash>.jsonl`** — one line per completed trace. Append-only daily file per client key. Each line is the full HTTP transaction parsed into JSON: request headers + body, response headers + body (or streaming events array), timestamps, sizes, disconnect/truncation flags.
2. **`data/index.sqlite`** — derived cache. Mirrors the JSONL columns the frontend needs, adds derived fields (model, token counts, session `parent_id`, `session_root_id`, `key_hash`). Deletable; rebuilt from layer 1 in seconds.

The **forwarding goroutine never looks inside a body**. Body parsing (JSON unmarshal, SSE event splitting) happens in the per-trace finalize step, after forwarding completes and off the hot path. Semantic interpretation — cost calculation, model-family classification, "what's a high-frequency skill" — happens downstream of api-log entirely. (See PHILOSOPHY § principle 1.)

### A JSONL line looks like this

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
    "headers": {"x-api-key": "sk-...", "anthropic-version": "2023-06-01"},
    "body":    {"model": "claude-sonnet-4-6", "messages": [...], "stream": true}
  },
  "resp": {
    "headers": {"Content-Type": "text/event-stream", "X-Request-Id": "..."},
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

---

## What this is not

- **Not a gateway.** It does not authenticate, route, retry, rate-limit, cache, or rewrite. Your gateway already does that.
- **Not Langfuse, Phoenix, or LangSmith.** Those instrument application code from inside via SDK. `api-log` is a network-layer recorder for traffic you cannot modify the source of.
- **Not Helicone.** Helicone shipped a full gateway stack (auth, routing, caching). `api-log` ships none of that.
- **Not mitmproxy.** mitmproxy is protocol-agnostic and interactive. `api-log` is OpenAI / Anthropic-aware on the *read* side (parses the bodies into structured fields) while staying byte-faithful on the *capture* side. Continuous recording, not interactive debugging.
- **Not a smart middlebox.** No prompt collision detection, no fake-cache injection, no content classification, no traffic shaping, no on-write inference.

For the long version, see [PHILOSOPHY.md](./PHILOSOPHY.md).

---

## Demo

**The JSONL is generic.** What follows are examples of what a consumer (a person at a terminal, a frontend, an AI agent) can do with the data we record — *not* features of api-log itself. api-log records the bytes; everything below is downstream.

**Skill / tool-call frequency across 30 days:**
```bash
zcat data/2026-*/*.jsonl{,.gz} 2>/dev/null \
  | jq -r '.resp.events[]? | select(.event=="response.output_item.added")
           | .data.item | select(.type=="function_call") | .name' \
  | sort | uniq -c | sort -rn
```

**Traces where the response stream was cut short:**
```bash
zcat data/2026-05-27/*.jsonl 2>/dev/null \
  | jq 'select(.disconnected==true or .resp.stream_done==false)
        | {id, path, status, ts_start}'
```

**All traces in one session, ordered:**
```bash
sqlite3 data/index.sqlite \
  "SELECT jsonl_path, jsonl_offset FROM traces
   WHERE session_root_id='01HX7K...' ORDER BY ts_start"
```

A frontend exists to make these point-and-click — but the data shape never requires one.

---

## Deploy concept

```yaml
# docker-compose.yml — ~6 new lines added to your existing gateway setup
services:
  gateway:                                  # CPA / sub2api / new-api / etc.
    # ... your existing config ...
    expose: ["7860"]                        # move 7860 from "ports" to "expose"

  api-log:                                  # ← new
    image: ghcr.io/xiayangzhang/api-log:latest
    ports:
      - "7861:7861"                         # proxy listener (clients connect here)
      - "7862:7862"                         # read API
    environment:
      APILOG_PROXY_UPSTREAM: http://gateway:7860
    volumes:
      - ./api-log-data:/data
```

Then update clients' `base_url` from `:7860` to `:7861`. That's the install.

A ready-to-edit version lives at [`deploy/demo/docker-compose.yml`](./deploy/demo/docker-compose.yml). Its skeleton expects `sub2api` cloned as a sibling directory (`../sub2api`); replace that build context with whatever upstream you front. The local dev stack at [`deploy/dev-stack/docker-compose.yml`](./deploy/dev-stack/docker-compose.yml) wires api-log to a mock LLM gateway (`tools/mockup`) and is what the integration test in [`tests/integration/run.sh`](./tests/integration/run.sh) drives.

For bare-metal: change one port number in your gateway config, run a single binary. Same outcome.

**Plain HTTP between containers on the same host is the primary supported topology.** If you need TLS or cross-host routing, terminate it with whatever reverse proxy you already run; api-log itself listens HTTP only.

### Operator note: bearer tokens land on disk

The JSONL files contain the **unredacted** `Authorization` / `x-api-key` headers exactly as the client sent them. api-log does not redact anything from the capture path — that is a deliberate design choice ([PHILOSOPHY § the "no" list](./PHILOSOPHY.md)). File-system permissions on `data/` are the operator's responsibility; treat that directory the way you would treat a `~/.ssh/` directory or a file holding production API keys. If you need redaction, run a downstream sidecar over the JSONL files; do not ask api-log to add a configurable filter.

---

## Read API

See [ARCHITECTURE.md § 6](./ARCHITECTURE.md) for the full read API surface (current count: 13+ endpoints; `/api/export` is the primary download surface).

`/api/traces/:id/replay` is the differentiator vs. SDK-based observability tools — it re-emits the recorded SSE stream at original per-chunk pacing using captured `t_delta_ms`. Replay is **to the API caller** (a viewer), never back to the upstream LLM.

`/api/traces` hits SQLite for millisecond response times. `/api/traces/:id` joins SQLite with one seeked read into the JSONL file. AI agents doing batch analysis can bypass the API entirely and read JSONL files directly off disk.

api-log ships **no embedded HTML viewer**. `GET /` returns a JSON pointer to the separate `api-log-viewer` project; the binary contains zero HTML. This is deliberate (PHILOSOPHY § principles 4 and 5).

---

## Design documents

- **[PHILOSOPHY.md](./PHILOSOPHY.md)** — Position, seven principles, the "no" list, scope boundaries, why we rejected each alternative, engineering standards. **Read this first.**
- **[ARCHITECTURE.md](./ARCHITECTURE.md)** — Data model, on-disk layout, JSONL line shape, SQLite schema, session inference, write path, read API, concurrency, failure modes, implementation notes.

---

## Roadmap

- [x] **v0** — capture path (parse + JSONL write + SQLite mirror), session inference, minimal read API.
- [x] **v0.1** — `api-log-viewer` (separate project) — multi-instance aggregation, session-tree visualization, tool-call rendering, SSE replay rendering, AI-assisted ad-hoc analysis.
- [x] **v0.1 plugins** — `text-replace` / `text-append` / `path-filter` with hot-reload via `PUT /api/config/plugins`. Off by default; opt-in via YAML or runtime API. See [PHILOSOPHY § 2 amendment](./PHILOSOPHY.md) for the operator-mutation boundary.
- [ ] **v0.2** — optional per-gateway **bridge adapters** (separate projects) — join external data (CPA's Redis usage queue, new-api's MySQL log table) into api-log traces by `key_hash`. The core proxy stays gateway-agnostic.

---

## Acknowledgements

The architecture borrows ideas from:

- **tcpdump / pcap** — append-only capture with deferred interpretation
- **CLIProxyAPI (CPA)** — single-binary, single-config aesthetic
- **Claude Code / Codex CLI** — local JSONL session files as a usable format
- **Langfuse** — the LLM-observability surface, against which we deliberately differ on the capture-vs-instrument axis

---

## License

[MIT](./LICENSE) — © 2026 Leo Yun.
