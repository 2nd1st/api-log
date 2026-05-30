# Philosophy

> This document is the project's constitution. Every architectural decision, every accepted PR, every rejected feature should trace back to one of the principles below. If a future change cannot be justified against these principles, the principles win — not the change.

---

## Position

`api-log` is a **transparent recording proxy for any OpenAI-compatible LLM gateway**.

It sits in front of CPA / sub2api / new-api / any gateway speaking the OpenAI Chat Completions, OpenAI Responses, or Anthropic Messages protocol. It forwards every request to the gateway, captures the request and response on the way through, parses them into one JSONL line per trace, and maintains a small SQLite index for the frontend to query.

That is the whole pitch.

It does not authenticate. It does not retry. It does not score. It does not rate-limit. It does not enrich. It captures, writes, indexes, and gets out of the way.

---

## Why this exists (the architectural gap)

A growing class of LLM workloads runs through third-party CLI tools (Codex, Claude Code, Cursor, Aider), IDE plugins, and gateway proxies — code the observer cannot modify. For these workloads, SDK-style instrumentation (Langfuse, Phoenix, LangSmith) is structurally unreachable: there is nothing to `pip install` into. The HTTP boundary between client and upstream gateway is the only observation point left.

That network-layer slot has no incumbent open-source tool. Helicone occupied it in proxy mode but entered maintenance mode after Mintlify's acquisition; official guidance is now to migrate back to SDK-based Langfuse. mitmproxy is the closest spiritual ancestor architecturally, but it is protocol-agnostic — built for interactive HTTP debugging, not continuous structured recording of LLM traffic.

`api-log` occupies this gap. It does one thing: faithfully record HTTP traffic between a client and an OpenAI-compatible gateway, parse the body into structured JSON on the way, and make the result queryable by humans, frontends, and AI agents.

---

## Seven principles

> **The capture path is the smallest surface this project ships.** PRs that add code to the capture path bear the burden of proof, not the reverse. The default answer to every "we should also do this" is *no*; the principles below say what it takes to clear that bar.

### 1. The conversation is truth.

We record what the wire shows. The capture path extracts the protocol envelope — JSON body, SSE event boundaries, named fields that the protocol itself emits — into a structured JSONL line. That is the entire scope of write-path work.

The boundary is **provenance**, not difficulty:

> **We extract fields the protocol envelope already names. We synthesize nothing. If a value requires a price table, a tokenizer, a classifier, a heuristic, or a second-pass model — it lives downstream of api-log.**

`usage.input_tokens` is on the wire, named by the provider; we copy it. `cost_usd = input_tokens × $0.000003` is a synthesis from a price table; it never enters the binary. `model = "claude-sonnet-4-6"` is named by the provider; we copy it. `model_family = "claude"` is a heuristic classification; it never enters the binary. The line is drawn by what the provider itself called the field, not by how hard the math would be.

This excludes, by construction, the entire class of features future PRs will be tempted to add: cost calculation, model-family bucketing, prompt classification, content scoring, language detection, retry-rate analytics. Each requires a synthesizer that does not live in the gateway's response.

**Two narrow carve-outs are explicit, and neither extends the semantic boundary.**

1. **Per-trace deterministic transforms of named values.** A hash, truncation, or canonical-form encoding of a field the protocol already names — for example, `key_hash = sha256(Authorization)[:16]` (a stable identifier built from a header) or `prefix_canonical_hash = sha256(canonicalize(session_prefix))[:16]` (a content-addressable hash of the request's `messages` array). These are encodings of a named input, not new information; they are no different in kind from the trace's `id` (a ULID is also a deterministic-time-based encoding).

2. **Structural cross-trace algorithms** that compute a property of the *relationships* between captured traces — specifically, the session inference of §"Why session inference" (matching prefixes across traces from the same `key_hash`). The output (`parent_id`, `session_root_id`) is a deterministic function of the captured JSONL lines and is fully rebuildable from them; no external table, model, or heuristic is consulted. Future cross-trace algorithms that meet the same bar (deterministic, rebuildable from JSONL alone, no external data) are eligible for the same carve-out.

**The carve-out is *structural*, not *semantic*. The semantic prohibition is absolute.** Cost, tokenizer-derived counts, classifier scores, model-family labels, heuristic categorizations, second-pass-model annotations — none of these are eligible. A future PR that argues "session inference is allowed, so my model-family bucketing should be too" is misreading the carve-out: session inference is rebuildable from JSONL by a script that consults no external data; model-family bucketing requires a lookup table that ships in someone's PR. They are not the same kind of thing, and they will never be on the same side of this line.

(For pragmatic absent-value sentinels — `status = -1`, empty-string `event` names for data-only SSE frames — see [ARCHITECTURE.md § 3](./ARCHITECTURE.md). They are encodings, not synthesis, and live with the schema, not the philosophy.)

### 2. Capture, never interfere.

The forwarding path is never throttled by a slow capture sink. If the disk is full, if the buffer overflows, if the JSONL writer falls behind — the client request keeps moving at upstream speed. Capture degrades to "we lost this body" before it ever degrades to "the client waited."

We do not retry. We do not cache. We do not rate-limit. We do not route. We do not rewrite. We do not enrich. The traffic belongs to the user; we are a tap, not a participant. PRs that add "smart" behavior to the request path will be rejected on sight.

Explicit operator-configured plugins registered at startup MAY interfere with requests and responses through the BEFORE (post-receive, pre-forward) and AFTER (post-upstream-response, pre-client-send) hooks. Such interference is OPT-IN at the config level — no plugin runs unless the operator has declared it in `config.yaml` or `runtime_overrides.json` — and is bounded by the two hook points described in `uiux-research/plugin-b-c-spec.md`. The original spirit of §2 holds: the capture path itself never independently rewrites, retries, rate-limits, or routes. The recorder is honest about what passed through it. What plugins do is recorded as the post-mutation state; the operator opted in and accepts that the recording reflects the new behavior, not a fictional pre-mutation baseline.

### 3. Fail open on capture; succeed visibly on forwarding.

If storage is full, if the index is locked, if the parser crashes, if a trace is silently dropped because a buffer overflowed — the client request **must still complete** to the gateway and back. Capture-side failures are by design **invisible to the client** (it gets its gateway response either way) but **visible to the operator** (via `/healthz` counters: `truncated_req_total` / `truncated_resp_total` / `drop_writer_full` / `drop_jsonl_fail` / `drop_sqlite_fail` and per-trace `truncated_req` / `truncated_resp` / `disconnected` flags). The operator's job is to monitor those signals; the client's job is to receive its response.

The forwarding side is the inverse: any failure on the forwarding path that *would* reach the client (process death, accept-loop hung, listener crashed) is fatal, surfaces immediately as a connection error or 5xx, and is something the operator must fix. We promise that no failure *short of process death* surfaces to the client. We also promise that capture failures, however silent to the client, are observable to the operator.

### 4. Compose, don't absorb.

Cost calculation lives in LiteLLM. Account attribution lives in CPA. Cache-rate dashboards live in new-api admin and `cpa-usage-keeper`. Eval suites live in Langfuse. Long-term aggregation lives in OTel collectors. Skill extraction lives in an analyst's `jq` pipeline or an AI agent reading the JSONL directly. Alerting / paging lives in whatever the operator already runs (Loki + alertmanager, Datadog, …). PII redaction lives in a downstream consumer or a separate sidecar. Diff-between-runs lives in a notebook. Any of these can consume our JSONL or our SQLite; none of them are reinvented here.

For every feature that sounds like "we should also do this," the first question is: **is there an existing tool that can consume our recordings to do it?** Our contribution is the recording itself — faithful, parsed, locally queryable. The job of the project is not to be feature-complete; it is to be the *source* that feature-complete consumers can speak to.

### 5. One process, one config.

Deploy is `docker run` or `./api-log`. No Kubernetes. No external Postgres. No required Redis. No sidecar. No worker queue. SQLite is the only embedded state, and it is derived (see principle 6).

The day this project requires more than one process to run is the day it has failed.

### 6. The filesystem is the truth; SQLite is a derived cache.

Source of truth is the JSONL files on disk. Each line is a complete parsed trace — request, response, headers, body, the works. SQLite is a **derived cache**: it mirrors JSONL fields for fast list-view queries and adds writer-computed columns (`model`, token counts, `session_root_id`) that are themselves provenance-extracted from the JSONL line (principle 1). It can be deleted at any moment and rebuilt from the JSONL files in seconds.

Two structural commitments follow:

- **Every value the SQLite layer exposes must be derivable from the JSONL files alone, by any tool that can read a line of JSON.** This pre-rejects any future column whose value depends on something outside the JSONL — embeddings, ML-derived classifications, vector indexes, joined external state. The "rebuild from JSONL" property is a contract, not a nice-to-have.
- **The JSONL schema is append-only for the lifetime of the project.** New optional fields can be added at any time; existing fields are never removed, renamed, or repurposed. Breaking changes happen by introducing a new top-level format key (e.g., `format: "apilog/v2"`) on new lines while older readers continue to handle older lines unchanged. This is the same longevity pattern that pcap, systemd-journal, and syslog have held for decades.

If the JSONL files exist, every byte of data exists, and any tool that can read a line of JSON can reconstruct everything we know.

Plugin mutations are NOT separately recorded. The JSONL line reflects the post-mutation state — what actually flowed after the BEFORE chain ran and what the client actually received after the AFTER chain ran. This is a deliberate trade-off: we forgo the ability to audit pre/post mutation diffs in exchange for simpler trace records and smaller files. Operators who need pre-mutation audit trails run their plugins under a dev profile that logs separately. Intercepted requests are marked on the JSONL line with a `plugin_intercepted` field (see plugin-b-c-spec § 5.2) so the operator can distinguish plugin-handled responses from genuine upstream responses.

### 7. The protocol surface is small, named, and slow to grow.

api-log records traffic at the **OpenAI / Anthropic-shaped gateway boundary** — request bodies that contain a `messages` array (plus optional sibling fields like Anthropic `system` or OpenAI Responses `instructions`), responses that are either a single JSON object or an SSE stream of `data:`-prefixed JSON events. That shape covers Chat Completions, Anthropic Messages, OpenAI Responses, and anything a future gateway translates into one of those.

Provider-native protocols that do **not** flow through such a gateway — Gemini's `contents` + `parts`, Bedrock's Invoke envelopes, Vertex native, raw OpenAI Realtime over WebSocket — are out of scope. A new protocol is added to the supported set only if it shares the `messages`-shaped envelope **and** has a real deployment that gateway operators are already proxying. Speculative protocol support is rejected on sight.

This principle is the firewall against parser-surface sprawl. Every new protocol is a new SSE event vocabulary, a new field-name shape, a new edge case in session inference. Frozen scope is what keeps the project small.

---

## The "no" list

The following are out of scope. Not "maybe later." Out of scope.

- No prompt management, no versioning, no template store.
- No eval / scoring / grading runners.
- No A/B testing infrastructure.
- No user / team / org / RBAC model.
- No SSO / OIDC.
- No "AI-powered" features in the core: no auto-classify, no auto-summarize, no recommendation, no on-write inference.
- No alerting / paging / notifications. Export to OTel / Loki / Vector if you want pages.
- No SaaS / hosted version / open-core tier.
- No phone-home telemetry.
- No auto-update.
- No plugin marketplace.
- **No synthesis on the capture path** (principle 1). We extract named protocol fields; we do not compute cost, infer model families, score quality, classify content, or run any second-pass interpretation.
- **No replay that re-contacts upstream.** `/api/traces/:id/replay` re-emits recorded SSE bytes to the API caller (a viewer) at original pacing — that is **inspection**. Re-sending a recorded request back to the gateway / LLM (with or without edits), simulating a different timing pattern by mutating recorded events, or chaos-engineering knobs that distort what was recorded — all of those are out of scope. The axis is **upstream contact**: anything that touches the upstream is forbidden; anything that re-emits recorded bytes back to a caller is fine.
- No upstream account management. The gateway already does this.
- No client authentication. The client's `Authorization` / `x-api-key` is captured as-is in the request headers — there is no separate redaction step. If you do not want bearer tokens on disk, that is a deployment-level decision.
- **No configurable header / body redaction filters in the capture path itself. Redaction MAY be implemented as an operator-opt-in plugin (see plugin-b-c-spec.md); the recorded JSONL line reflects the post-redaction state.**
- **No aggregate / analytics endpoints** like `/api/sessions/:root_id/tree`, `/api/stats/...`, `/api/usage/...`. The read API exposes raw rows; aggregate queries are SQL the frontend (or any consumer) runs against SQLite.
- **No per-key / per-model / per-path configuration overrides.** Body-size caps, retention windows, viewer flags are instance-wide and singular. Configurable-per-thing is the entry point for absorption.
- **No online re-parse on the read path.** Re-parse is an explicit offline CLI subcommand that walks JSONL through the current parser and updates SQLite. The read path never speculatively re-runs the parser.
- No WebSocket capture in v0 (and v0.1). Some gateways (e.g. sub2api's Responses v2) put their stateful streaming on WebSocket; that is a separate frame protocol. WebSocket support, if ever added, ships as a sibling project — not as an option in this binary.
- No content classification, prompt collision detection, fake-cache injection, or any other "smart" middlebox behavior.

If something on this list seems useful, the answer is **compose with another tool downstream**, not absorb.

---

## Why we said no to alternatives

### Why proxy, not SDK

SDK instrumentation requires that the observer own the application. When the application is a third-party CLI, an IDE plugin, or a gateway running someone else's code, there is nothing to instrument from inside. The network layer is the only observation surface that exists for these workloads.

### Why "in front of the gateway", not behind

Behind-the-gateway placement would expose upstream OAuth tokens and provider API keys, would require us to know each gateway's upstream topology, and would force us to become a router across multiple providers. All three break the project's scope. Front-of-gateway placement keeps us stateless, gateway-agnostic, and outside the credential blast radius.

### Why JSONL with parsed bodies, not raw HTTP wire bytes

We considered storing raw HTTP request/response bytes on disk and parsing only at read time. We rejected it for two compounding reasons. **First**, every consumer of this data (frontend list view, AI agent, jq pipeline) needs structured fields — so the parse has to happen somewhere, and doing it once at capture time amortizes across all readers. **Second**, the ecosystem standard for LLM conversation logs is JSONL with parsed message objects (CC, Codex CLI, every observability backend) — matching that format means our output is directly consumable by tools that already speak it.

The cost — write-path parsing — is bounded: we parse the protocol envelope (JSON body, SSE event boundaries), not its meaning.

### Why a single JSONL covers all protocols, not one file per protocol

OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages all share the same conceptual shape on the wire: an HTTP request with a structured body, an HTTP response (streaming or not) with a structured body. The `path` field in each JSONL line records which protocol it was. Splitting by protocol would force every consumer to know about three paths instead of one, for no analytical gain.

### Why session inference by messages prefix matching, not by chain pointer fields

We discovered through testing that in HTTP mode:
- OpenAI Chat Completions has no session field at all.
- OpenAI Responses API has a `previous_response_id` field, but sub2api's HTTP implementation silently ignores it (chains are WebSocket-only in their v2 protocol) and CPA accepts the field without actually threading context. In practice, the chain pointer is null on the wire.
- Anthropic Messages has no protocol-level chain field at all; sessions are implied by the messages array on the client side.

But every protocol has the same structural property: **stateless protocols force the client to resend the full conversation history on every turn**, which means each trace contains its full ancestry by design. Prefix matching over a normalized "session prefix" — the longest prior trace whose prefix equals the new trace's — recovers session structure from any of these protocols without needing protocol-specific code, supports forks (regenerated assistant replies become sibling branches under the same root), and degrades gracefully on the one edge case where it cannot succeed (two unrelated traces with byte-identical prefixes → treated as independent roots).

The "session prefix" is constructed per protocol so that all protocol-equivalent state participates:

- OpenAI Chat Completions: `messages[]` (system message is `messages[0]` already).
- Anthropic Messages: `[system_as_virtual_turn_0] + messages[]` — the `system` field lives outside `messages` on the wire, so we prepend it as a synthetic turn so that two conversations with the same first user message but different system prompts are correctly identified as separate roots.
- OpenAI Responses API: normalized from the `input` shape into a `messages`-like array, with `instructions` prepended like Anthropic's `system`.

This normalization is the only place in the codebase that has per-protocol knowledge about session boundaries. The matching algorithm itself is identical across protocols.

Validated against real traffic from sub2api on both Chat Completions and Anthropic Messages — see `experiments/session-inference/`.

### Why no Redis / external cache

Redis is correct when you need cross-process pub/sub, sub-millisecond RPC across hosts, or write rates SQLite cannot absorb. None apply here. The natural read accelerator is the OS page cache over a memory-mapped SQLite file. Adding Redis would add a process, a container, and another piece of state that can desync from truth.

---

## Scope boundaries (what "session" and "trace" actually mean)

The data model uses some terms in narrow ways that the user-facing English often does not match. To prevent slow scope drift, these definitions are explicit:

**A trace is one HTTP request/response pair.** Not "one LLM call from the user's perspective" (which might be one HTTP request, or might be a tool-call loop of dozens). Not "one agent run" (which might be hundreds). Not "one user turn in a chat" (which might be obscured by client-side caching or compaction). One HTTP transaction on the wire is one trace; nothing more is asserted.

**A session here means prefix-chained turns on the wire, nothing more.** It is not "one task," "one agent run," or "one user intent." When a Claude Code session runs for an hour and emits 47 HTTP requests, prefix-matching may group them into one session tree — or, if the client compacts its conversation mid-run (replacing 50 prior messages with a summary), it will appear as two separate session roots. Both outcomes are *correct under the wire-level definition*; mapping wire sessions back to user-task sessions is a downstream consumer's problem.

**Workloads that have no "session" at all are first-class.** Embedding requests, image-generation requests, transcription requests, file uploads — none have a `messages` array, none have meaningful prefix chains. Their `session_root_id` is always themselves; they are trees of size one. The schema accommodates this trivially; the *concept* of session simply does not apply, and we do not pretend it does.

**Client-side compaction / summarization breaks the chain on purpose.** When CC's auto-compaction or Codex's context-window pruning rewrites the messages array, the new trace is structurally a new conversation as far as the wire shows. Session inference will produce a new root, which is the honest answer to "what did this client send us." A downstream consumer that wants to glue the pre- and post-compaction halves together has the JSONL bodies available to do so.

**WebSocket-only flows are not seen.** Some gateways already route some workloads over WebSocket (sub2api's Responses v2, OpenAI Realtime, future provider-native channels). api-log sees nothing about those. This is not a hidden limitation — it is the consequence of being an HTTP/1.1 + HTTP/2 reverse proxy. A WebSocket recorder, if it ever exists, is a sibling project; principle 5 (one process, one config) forbids absorbing it.

**New providers with non-OpenAI-shaped protocols are out of scope** (principle 7). If Gemini ships a `contents` + `parts` native API that gateways forward verbatim, api-log will not parse it — the gateway is expected to translate to OpenAI/Anthropic shape, or that traffic flows past us untranscribed. Adding a new envelope shape is a v2-or-fork decision, not a feature PR.

---

## Engineering standards

This is an open source project. The code quality bar is **community standard**, not "it works on my machine." Reference points: Go stdlib, tcpdump, nginx, sqlite, Caddy.

**Style.** Idiomatic Go. `gofmt`, `goimports`, `golangci-lint` all clean. No dead code, no clever code, no unjustified abstractions. Exported identifiers documented with godoc comments. Package boundaries reflect responsibility, not convenience.

**Tests.** Unit tests per package. Table-driven where applicable. The forwarding hot path has integration tests against a mock upstream. `go test -race` enabled in CI. Coverage measured, not gated.

**Errors.** Always handled, never swallowed. Errors crossing package boundaries are wrapped with `fmt.Errorf("...: %w", err)`. No `panic` in library code; reserve panics for startup misconfiguration.

**Logging.** `log/slog`. `Debug` for development noise, `Info` for normal flow, `Warn` for recoverable issues, `Error` for failures the operator must act on. Structured fields, not interpolated strings.

**Context.** All I/O accepts a `context.Context`. Cancellation propagates. No `context.Background()` outside `main` and tests.

**Dependencies.** Minimal. Prefer stdlib. Each external import justifies its introduction. Pin versions; do not consume `latest`.

**Wire formats are stable contracts.** The JSONL line schema, the on-disk directory layout, and the HTTP read-API surface are versioned. Backwards-compatible additions only; breaking changes require a major version bump and a documented migration path.

**Reviews.** Every PR reviewed by someone other than the author. Reviews focus on principles first, then correctness, then style. A PR that violates a principle is closed regardless of how well it is written.

**Performance.** Benchmark before optimizing. The forwarding hot path must be measured under realistic load before each release. Idle-state per-request allocations are scrutinized.

---

## What this project looks like in five years

If we get the principles right, the following should still be true:

- The README's first sentence is unchanged.
- The deploy story is still a docker-compose snippet plus one binary.
- The "no" list is **longer**, not shorter.
- On-disk format is still `data/<date>/<key_hash>.jsonl` per day, plus `index.sqlite`. Schema may have gained optional fields; the structural shape has not changed.
- Frontends, AI-assisted analyzers, and pipeline integrations have grown into **separate projects** that consume our output. The core proxy still looks almost identical to v0.

That is what success looks like. Not a feature checklist competing with Langfuse. A small, sharp, durable tool that sits in the LLM-gateway ecosystem the way tcpdump sits in the network-tooling ecosystem — uncontroversial, ever-present, boring to look at, impossible to live without once you have used it.
