# Plugin Phase B + C Contract Specification

**Status:** Ratified contract; BUILD-phase implementation.
**Date:** 2026-05-30 (initial ratification); subsequent amendments tracked in CHANGELOG.

---

## 1. Context

### 1.1 Why a two-hook design

Phase A landed an observe-class plugin scaffold (`internal/plugin/plugin.go`): `Plugin` base interface + `ObserveBeforeRecord` (post-finalize, pre-TrySend; can drop a recording) + `ObserveAfterRecord` (post-write-ack; side-effecting only). The scaffold lets two facts about the project sit on disk:

1. The interfere-class hooks an earlier design draft sketched (`mutate_req`, `before_forward`, etc.) needed an explicit operator-opt-in surface before shipping; they were intentionally left out of the observe-class scaffold.
2. Concrete first-mover use cases (watermark, prompt injection, redaction, per-IP gate) require interference, not just observation.

This spec writes the interfere-class contract. It freezes the API surface that BUILD-phase work packages implement, before any code goes into the hot path that can rewrite or short-circuit a request.

### 1.2 What this solves

Three concrete operator problems, in priority order:

1. **Watermark.** Append a small text marker to assistant responses leaving the proxy (provenance / "this answer came from this proxy"). Priority first plugin.
2. **Prompt injection.** Prepend system prompts or merge instructions into the request before forwarding (e.g. enforce a house style, inject persona, force tool preamble). BEFORE-hook flagship use case.
3. **Per-IP gate.** With XFF smart-resolution landing the real client IP into the recorded trace, the IP is reachable for a per-IP gate. Off-by-default `rate_limit_ip` plugin that 4xx's offenders before forwarding.

Other use cases (redaction, tool whitelist, skill injection, generic interception) ride the same contract.

### 1.3 Who consumes this

- **api-log backend**: implements the hook interfaces, the registry, the parsed-content builders, and the proxy-loop call sites.
- **api-log-viewer**: implements the Settings → Plugins panel that lets the operator add / edit / enable / disable plugin instances.
- **operator**: reads this spec to confirm the BUILD plan matches what was asked for, edits `config.yaml` + uses the viewer Settings UI to drive plugin behavior.
- **future plugin authors**: read this spec to know what the contract guarantees about call ordering, fail semantics, and parsed-content shape.

### 1.4 Supersession note

An earlier design draft proposed five hooks (`mutate_req`, `before_forward`, `after_forward`, `before_record`, `after_record`). That five-hook design was collapsed to **two** hooks on 2026-05-30. The five-hook design is OBSOLETE for greenfield work; only the Phase A observe-class subset survives, repurposed as a separate **Observer** class that coexists with the new BEFORE / AFTER hooks. See § 5 below for how `pathfilter` (the only existing Phase A plugin) lives in the new world.

---

## 2. Hook model

### 2.1 The two hooks

There are exactly two hot-path hook points. No more.

```
┌──────────────────────────┐
│ client request received  │
└────────────┬─────────────┘
             │
             ▼
   ┌─────────────────────┐
   │ parse request body  │  (parser package — chat / messages / responses / gemini)
   │ resolve client IP   │  (XFF smart-resolve)
   └────────────┬────────┘
                │
                ▼
   ╔══════════════════════════════════════════════╗
   ║   BEFORE hook chain (operator-ordered)       ║
   ║   - continue / mutate / intercept            ║
   ║   - intercept short-circuits forwarding      ║
   ╚════════════════════╤═════════════════════════╝
                        │
              ┌─────────┴──────────┐
              │                    │
              ▼                    ▼
     ┌──────────────────┐  ┌────────────────────┐
     │ forward upstream │  │ skip forward;      │
     │ collect response │  │ use intercept body │
     └────────┬─────────┘  └─────────┬──────────┘
              │                      │
              ▼                      ▼
   ╔══════════════════════════════════════════════╗
   ║   AFTER hook chain (operator-ordered)        ║
   ║   - continue / mutate                        ║
   ║   - intercept allowed too (rare; e.g. 5xx    ║
   ║     wrap or response replacement)            ║
   ╚════════════════════╤═════════════════════════╝
                        │
                        ▼
              ┌──────────────────────┐
              │ stream to client     │
              └──────────┬───────────┘
                         │
                         ▼
              ┌──────────────────────────┐
              │ finalize: build trace    │
              │ run Observer class       │  (pathfilter / future filters)
              │ TrySend → writer         │
              └──────────────────────────┘
```

Three things to keep straight:

- **BEFORE** sees the parsed *request* and can mutate it (request rewrite) OR intercept (skip upstream entirely and synthesize a response).
- **AFTER** sees the parsed *request and response* and can mutate the response (watermark) OR intercept (rare — replace the response after the fact).
- **Observer** (Phase A class) is NOT a hook in the new model's sense. It runs in the finalize block, post-response, and decides only whether the trace is recorded to disk. `pathfilter` is the existing example. Observers cannot mutate or intercept anything user-facing.

### 2.2 Action vocabulary

Each hook plugin call returns one of three actions:

| Action | Meaning | BEFORE behavior | AFTER behavior |
|---|---|---|---|
| `Continue` | Plugin had nothing to do. | Pass parsed request unchanged to next plugin / forwarder. | Pass parsed response unchanged to next plugin / client. |
| `Mutate` | Plugin produced a modified parsed object. | Replace parsed request; next plugin sees the new shape. | Replace parsed response; client receives the new shape. |
| `Intercept` | Plugin produced a final response. Skip everything downstream. | Skip forwarding to upstream; serve intercept response to client; AFTER chain still runs on the synthesized response so watermark etc. can decorate it. | Replace response; client receives the intercept response; remaining AFTER plugins do NOT run. |

Operator-confirmed: **the intercept response can carry any status code (200, 4xx, 5xx) and any body**. Plugins are not constrained to "rejection only."

### 2.3 The Go interfaces

This is the copy-pasteable contract. Names and types are frozen; BUILD W1 implements them verbatim.

```go
// Package plugin (Phase B+C extension)
//
// The hot-path hook surface. Lives alongside the Phase A observe-class
// surface in internal/plugin. Phase A's Observer class is unchanged
// and continues to provide post-finalize, pre-record drop semantics
// (see pathfilter). The interfaces here are the NEW interfere-class
// surface ratified by operator on 2026-05-30.

type Hook int

const (
    HookBefore Hook = iota // post-receive, pre-forward
    HookAfter              // post-upstream-response, pre-client-send
)

type Action int

const (
    ActionContinue  Action = iota // pass through unchanged
    ActionMutate                  // replace parsed object, continue chain
    ActionIntercept               // short-circuit with InterceptResponse
)

// Protocol mirrors the parser package's protocol discriminator. Plugins
// can switch on this to decide whether they understand the shape.
type Protocol int

const (
    ProtocolUnknown   Protocol = iota
    ProtocolChat               // OpenAI /v1/chat/completions
    ProtocolMessages           // Anthropic /v1/messages
    ProtocolResponses          // OpenAI /v1/responses
    ProtocolGemini             // Google /v1beta/...
)

// Message is the normalized turn shape. Built by the request-parser pass
// that runs BEFORE the hook chain; identical across all four protocols.
//
// "Normalized" means the parser has:
//   - lifted system prompts into a separate field (SystemPrompt below),
//     so Messages contains only user / assistant / tool turns
//   - flattened multi-part content blocks into one logical "content"
//     per turn (multi-modal payloads are preserved in Parts)
//   - kept tool calls / tool results as separate fields on the turn
type Message struct {
    Role      string  // "user" | "assistant" | "tool"
    Content   string  // concatenated text content of all text parts
    Parts     []Part  // ordered original parts (text / image / audio / tool_use / tool_result)
    Name      string  // for tool turns, the tool name
    ToolCallID string // for tool turns, the matching call id
}

type Part struct {
    Type      string          // "text" | "image" | "audio" | "tool_use" | "tool_result"
    Text      string          // when Type == "text"
    MediaType string          // when Type == "image"|"audio" — e.g. "image/png"
    DataB64   string          // base64 for inline media
    URL       string          // when media is URL-only
    ToolUse   *ToolUse        // when Type == "tool_use"
    ToolResult *ToolResult    // when Type == "tool_result"
    Raw       json.RawMessage // original part JSON (escape hatch)
}

type Tool struct {
    Name        string
    Description string
    Schema      json.RawMessage // JSON Schema object
    Raw         json.RawMessage // original tool definition JSON
}

type ToolUse struct {
    ID    string
    Name  string
    Input json.RawMessage
}

type ToolResult struct {
    ToolUseID string
    Content   string
    IsError   bool
}

type ToolCall struct {
    ID        string
    Name      string
    Arguments string
}

// ParsedRequest is what BEFORE plugins (and AFTER plugins, as the
// request side) receive. Built from the raw request body by the parser
// once per request; reused across the entire chain.
type ParsedRequest struct {
    Protocol     Protocol
    Path         string         // "/v1/chat/completions" etc.
    Method       string         // "POST"
    Model        string         // copied from req body
    Messages     []Message      // normalized turn array
    Tools        []Tool         // tool definitions
    SystemPrompt string         // first system role text, computed
    Headers      http.Header    // a copy; safe to mutate
    ClientIP     string         // smart-XFF resolved
    KeyHash      string         // matches the trace's KeyHash
    RawBody      json.RawMessage // escape hatch; do NOT mutate in place
}

// ParsedResponse is what AFTER plugins receive. Built once after the
// upstream response is fully read (non-streaming) or fully assembled
// from SSE events (streaming).
type ParsedResponse struct {
    Protocol  Protocol
    Status    int            // upstream HTTP status (or 0 if intercepted before forward)
    Content   string         // concatenated assistant text content
    Reasoning string         // concatenated reasoning text (Anthropic thinking blocks etc.)
    ToolCalls []ToolCall     // structured tool calls (from response)
    Headers   http.Header
    RawBody   json.RawMessage // non-streaming case
    Events    []sse.Event     // streaming case
    Usage     UsageInfo       // tokens / model / finish_reason — from parser.ExtractUsage
}

type UsageInfo struct {
    Model            string
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
    CachedTokens     int
    ReasoningTokens  int
    FinishReason     string
}

// InterceptResponse is what a plugin returns when it wants to bypass
// the rest of the pipeline. The proxy serializes this directly to the
// client (after the AFTER chain runs, in the BEFORE-intercept case).
type InterceptResponse struct {
    Status  int         // 200 / 4xx / 5xx — plugin chooses
    Headers http.Header // merged into the response headers
    Body    []byte      // raw bytes, served as-is
}

type BeforeResult struct {
    Action    Action
    Mutated   *ParsedRequest      // when Action == ActionMutate; else nil
    Intercept *InterceptResponse  // when Action == ActionIntercept; else nil
}

type AfterResult struct {
    Action    Action
    Mutated   *ParsedResponse     // when Action == ActionMutate; else nil
    Intercept *InterceptResponse  // when Action == ActionIntercept; else nil
}

// BeforePlugin is the BEFORE-hook contract.
//
//  - Name() is the TYPE name (e.g. "watermark", "prompt_inject"). Distinct
//    INSTANCES of the same type share Name() and are disambiguated by
//    InstanceID() at registration time (see § 3).
//  - OnBefore receives the parsed request and the per-instance config
//    blob (decoded from runtime_overrides.json / config.yaml).
//  - Plugin MUST treat ParsedRequest as logically read-only and return
//    a mutated copy via Mutated. Mutating the input directly is undefined
//    behavior (the proxy may reuse the buffer for retry / logging).
//  - A returned error is treated as ActionContinue with a WARN log; the
//    request flows unchanged. Plugin panics are caught by defer recover()
//    in the dispatcher (§ 4) with the same fail-open semantics.
type BeforePlugin interface {
    Plugin // Name(), Init(cfg), Close() from Phase A
    OnBefore(ctx context.Context, req *ParsedRequest) BeforeResult
}

// AfterPlugin is the AFTER-hook contract.
//
// Same conventions as BeforePlugin. The request pointer is the parsed
// request post-BEFORE-chain (i.e. what was actually forwarded, or the
// original if no BEFORE plugin mutated it; in the BEFORE-intercept case
// it is whatever the intercepting plugin saw).
type AfterPlugin interface {
    Plugin
    OnAfter(ctx context.Context, req *ParsedRequest, resp *ParsedResponse) AfterResult
}
```

### 2.4 Hook ordering rules

- **Within a hook**: plugins fire in *registration order*. Registration order is set by the operator-declared list in `config.yaml` (declarative defaults) and overridden by `runtime_overrides.json` (runtime edits).
- **BEFORE vs AFTER**: BEFORE always runs to completion before forwarding (or before the short-circuit if intercepted). AFTER runs after the response is in hand.
- **Intercept short-circuit**:
  - BEFORE plugin returns `ActionIntercept` → remaining BEFORE plugins are SKIPPED. The proxy synthesizes the response from the intercept payload, then runs the FULL AFTER chain on the synthesized response (so watermark etc. still decorates it).
  - AFTER plugin returns `ActionIntercept` → remaining AFTER plugins are SKIPPED. The intercept response replaces what gets streamed to the client.
- **Mutate cascading**: if plugin N returns `ActionMutate`, plugin N+1 sees the *mutated* parsed object. Operators should be aware of this and order their plugins accordingly (e.g. redact-then-prompt-inject is meaningfully different from prompt-inject-then-redact).

### 2.5 What plugins read and write

Plugins receive **parsed content** by default, not raw bytes. The parser package's existing shapes (`UsageInfo`, `trace.Body`, normalized `Messages` / `Tools`) feed the `ParsedRequest` / `ParsedResponse` builders. This is the explicit operator answer to "plugins shouldn't have to re-parse the body."

Plugins that need raw bytes (e.g. for cryptographic operations, body-hash comparisons) read `ParsedRequest.RawBody` / `ParsedResponse.RawBody` / `ParsedResponse.Events`. These are present always; mutation should still happen via the parsed shape so the next plugin / the trace recorder sees coherent state. If a plugin mutates only `RawBody`, the parsed fields drift from the bytes — undefined behavior, document as "do not do this."

**AFTER-hook `ToolCalls` carve-out (v1).** AFTER plugins MAY READ
`ParsedResponse.ToolCalls` (post-stream buffered view) for inspection,
but MUST NOT MUTATE it in v1. Tool-call argument mutation on the
response side is deferred to Phase D — see §10.6 for the full
rationale and §12.3 for the out-of-scope entry. Operators wanting to
control tool-call behavior in v1 use one of: BEFORE-side
`ParsedRequest.Tools` editing (strip tools the model isn't allowed
to call), full-response `ActionIntercept` (replace the whole stream
with a refusal / safe completion), or Observer-class JSONL scrub at
record time. Each of these resolves the underlying need cleanly
without committing v1 to a per-protocol streaming-fragment re-emitter.

---

## 3. Multi-instance + config

### 3.1 Type vs instance

The operator's model:

- A **plugin type** (e.g. `watermark`, `prompt_inject`, `rate_limit_ip`) is a Go type registered via an in-tree builtin factory at compile time. The builtin registry maps type-name → factory function.
- A **plugin instance** is a `(type, instance_id, config)` tuple. Multiple instances of the same type can coexist (e.g. two watermark instances applying different watermark text to different routes).
- Instance IDs are operator-chosen short strings (kebab-case recommended), unique across all instances regardless of type. Used in JSONL markers, viewer UI, and runtime config edits.

Example: an operator with two watermarks for two route groups:

```yaml
plugins:
  - type: watermark
    id: wm-public
    enabled: true
    config:
      pattern: "—via my.proxy.example/public"
      routes: ["/v1/chat/completions", "/v1/messages"]
  - type: watermark
    id: wm-internal
    enabled: false
    config:
      pattern: "[INTERNAL]"
      routes: ["/v1/responses"]
  - type: prompt_inject
    id: house-style
    enabled: true
    config:
      inject_as_system: "Respond in plain prose; no markdown headings."
      routes: ["/v1/*"]
```

### 3.2 Builtin factory registration

Each builtin plugin type registers a factory at `init()` time in `internal/plugin/builtin/<type>/`:

```go
// internal/plugin/builtin/watermark/watermark.go
func init() {
    builtin.Register("watermark", func(cfg map[string]any) (plugin.Plugin, error) {
        p := &Watermark{}
        if err := p.Init(cfg); err != nil {
            return nil, err
        }
        return p, nil
    })
}
```

The composite registry at startup iterates the effective `plugins` list (YAML merged with runtime overrides, ordered by the list itself), looks up each entry's type via the builtin factory map, constructs the instance with the per-instance config, and appends it to the active registry.

### 3.3 Config sources and merge

Two persistence layers, in increasing precedence:

1. **`config.yaml`** under the existing config root (alongside `media:`, `noise_filter:` etc.). Declarative defaults — the shape the operator ships in the deployed config.
2. **`<DataDir>/runtime_overrides.json`** under a new `plugins` block. Viewer-Settings-UI-editable. Mirrors the Phase K media pattern: `*pointer` fields distinguish "unset" from "explicit empty"; full-file atomic rename on save.

#### 3.3.1 YAML shape

```yaml
plugins:
  - type: watermark
    id: wm-public
    enabled: true
    config:
      pattern: "—via my.proxy"
      routes: ["/v1/*"]
  - type: prompt_inject
    id: house-style
    enabled: true
    config:
      inject_as_system: "..."
      routes: ["/v1/chat/completions"]
```

`plugins` is a YAML sequence (ordered). Each entry is mandatory keys `type`, `id`, `enabled`, `config`.

#### 3.3.2 runtime_overrides.json shape

```json
{
  "media": { "save_attachments": true },
  "plugins": {
    "instances": [
      {
        "type": "watermark",
        "id": "wm-public",
        "enabled": true,
        "config": { "pattern": "—via my.proxy", "routes": ["/v1/*"] }
      },
      {
        "type": "watermark",
        "id": "wm-internal",
        "enabled": false,
        "config": { "pattern": "[INTERNAL]", "routes": ["/v1/responses"] }
      }
    ]
  }
}
```

Semantics:

- `plugins.instances == nil` (missing) → fall through to YAML.
- `plugins.instances == []` (explicit empty array) → "operator turned off all plugins"; override wins, no plugins run.
- `plugins.instances == [...]` (non-empty) → **full replace** of the YAML list. There is no "merge by id" mode — operators who want to change one field write the full list back. This is what the viewer UI sends on PUT.

The operator's reasoning (paraphrased): partial overrides on a list keyed by `id` are surprising; full-replace is honest and matches how the viewer UI builds its payload.

#### 3.3.3 Go shape (Overrides extension)

Extending the existing `internal/runtime/overrides.go`:

```go
type Overrides struct {
    Media   MediaOverrides   `json:"media"`
    Plugins *PluginsOverrides `json:"plugins,omitempty"`
}

// PluginsOverrides is the on-disk shape for the plugins block of
// runtime_overrides.json.
//
// Pointer on the parent (Overrides.Plugins) distinguishes "no override"
// (nil → YAML wins) from "explicit empty" (non-nil with empty Instances
// → all plugins off).
type PluginsOverrides struct {
    Instances []PluginInstanceConfig `json:"instances"`
}

type PluginInstanceConfig struct {
    Type    string         `json:"type"`
    ID      string         `json:"id"`
    Enabled bool           `json:"enabled"`
    Config  map[string]any `json:"config"`
}
```

### 3.4 Atomic-load pattern

Backend holds an `atomic.Pointer[Registry]`. At startup, build registry from (yaml merged with overrides) and Store. On runtime config edit:

1. `SaveOverride(dataDir, func(o *Overrides) { o.Plugins = &newBlock })` writes the new JSON.
2. Rebuild a fresh `Registry` from the new effective config (yaml + new overrides).
3. `Init()` each new instance.
4. Atomically swap `atomic.Pointer[Registry]`.
5. `Close()` the old registry's instances asynchronously (after a grace period so in-flight requests using the old pointer drain).

This mirrors the Phase K media subsystem's pattern. The viewer UI's PUT handlers call into a single backend function that performs steps 1–5.

### 3.5 Per-instance enable / disable

Disabled instances are still loaded and Init'd at startup (so config drift is caught early), but the dispatcher skips them on each call. This is a design choice over "don't construct disabled instances at all" — it makes the viewer's enable-toggle round-trip cheap (no need to fail config validation when the user is mid-edit).

---

## 4. Fail / panic semantics

The proxy invariant is unchanged from Phase A: **capture still happens; client requests don't degrade because a plugin misbehaves**. Specifics:

### 4.1 Plugin returns error

Each hook method's contract is "no error return value." If a plugin needs to surface a failure, it logs via `slog` and returns `ActionContinue`. The framework treats this as best-effort observation: the request flows unchanged.

(Rejected design: an `error` return type. Operator and Phase-A precedent both agreed plugins should not be able to fail the request by returning an error.)

### 4.2 Plugin panics

The dispatcher wraps each plugin call in `defer recover()`:

```go
func (r *Registry) runBefore(ctx context.Context, req *ParsedRequest) (*ParsedRequest, *InterceptResponse) {
    cur := req
    for _, inst := range r.beforePlugins() {
        res := safeCallBefore(ctx, inst, cur) // defer recover() inside
        switch res.Action {
        case ActionMutate:
            if res.Mutated != nil {
                cur = res.Mutated
            }
        case ActionIntercept:
            return cur, res.Intercept
        }
    }
    return cur, nil
}

func safeCallBefore(ctx context.Context, inst *Instance, req *ParsedRequest) (res BeforeResult) {
    defer func() {
        if rec := recover(); rec != nil {
            slog.Warn("plugin panic", "type", inst.Type, "id", inst.ID,
                      "hook", "before", "panic", rec)
            res = BeforeResult{Action: ActionContinue}
        }
    }()
    return inst.Plugin.(BeforePlugin).OnBefore(ctx, req)
}
```

Same shape for AFTER. The plugin-panic isolation requirement is captured here in this contract.

### 4.3 No trace is ever dropped due to plugin failure

A panicking plugin produces no mutation, no interception, and a WARN log. The proxy continues. The trace recorder still sees the request and response. The recorded trace's `plugin_errors` field (a small array, see § 5) carries the panic message so operators can debug from disk.

### 4.4 Init failure is fatal

A plugin whose `Init(cfg)` returns an error fails process start (Phase A semantics, unchanged). For runtime config edits via the viewer UI, an Init failure of a new instance does NOT swap the atomic pointer — the old registry stays live and the PUT response returns the Init error to the UI for display.

---

## 5. Trace recording semantics

### 5.1 Post-mutation only

**No mutation log.** The recorded JSONL line carries the request and response state as it actually flowed (post-BEFORE-chain request, post-AFTER-chain response). Plugins do not write a separate pre/post diff entry.

The trade-off: we forgo the ability to audit what was changed, in exchange for simpler trace shape and smaller files. The rule is documented in § 6 below.

### 5.2 Intercepted traces ARE recorded

If a plugin intercepts a request (BEFORE → ActionIntercept) or replaces a response (AFTER → ActionIntercept), the trace IS still recorded, with a top-level marker on the JSONL line:

```json
{
  "ts": "2026-05-30T18:14:00Z",
  "path": "/v1/chat/completions",
  "status": 403,
  ...,
  "plugin_intercepted": {
    "type": "rate_limit_ip",
    "id": "ddos-gate",
    "hook": "before"
  }
}
```

The marker is one of:

- absent → trace flowed through with no interception
- present → trace was intercepted; `type` + `id` + `hook` identify the responsible plugin

This is the minimum information needed for the operator to see "this 403 didn't come from upstream, it came from my plugin." Without this, intercepted traces would be indistinguishable from genuine upstream rejections.

### 5.3 Plugin error breadcrumbs

A small array of plugin error events on the trace, populated by the panic / fail path:

```json
{
  ...,
  "plugin_errors": [
    { "type": "watermark", "id": "wm-public", "hook": "after", "msg": "runtime error: index out of range" }
  ]
}
```

This is the only audit channel for plugin failures at the disk level. Optional field, absent on the common case (no plugin failed). Operators reading traces can grep for `plugin_errors` to find affected traces.

### 5.4 No retroactive recording

Once the writer has finalized the JSONL line, no plugin can amend it. AFTER plugins that want to mutate the response must do so before the response is streamed to the client; the trace's `resp` block captures the post-mutation state at finalize time. There is no "re-record" path.

---

## 6. Recording behavior under plugins

Four ratified rules govern how plugin presence affects the JSONL record.

### 6.1 Plugins are explicit operator opt-in

Plugins MAY interfere with requests and responses through the BEFORE (post-receive, pre-forward) and AFTER (post-upstream-response, pre-client-send) hooks. Interference is opt-in at the config level — no plugin runs unless the operator has declared it in `config.yaml` or `runtime_overrides.json` — and is bounded by the two hook points described in this document. The capture path itself never independently rewrites, retries, rate-limits, or routes. What plugins do is recorded as the post-mutation state; the operator opted in and accepts that the recording reflects the new behavior, not a fictional pre-mutation baseline.

### 6.2 Mutations are recorded post-mutation only

The JSONL line reflects the post-mutation state — what actually flowed after the BEFORE chain ran and what the client actually received after the AFTER chain ran. Plugin mutations are NOT separately recorded; there is no pre/post diff. This is a deliberate trade-off: simpler trace records and smaller files in exchange for foregoing pre-mutation audit. Operators who need pre-mutation audit trails run their plugins under a dev profile that logs separately.

### 6.3 Named-field discipline unchanged

Plugin mutations modify named fields the parser already understands; they do not introduce synthesized values. A watermark plugin appending to `messages[-1].content` is editing a named field; it is not synthesizing one. The "named-fields-only, no body synthesis" discipline that constrains the parser also constrains plugins.

### 6.4 Intercepted requests are marked

When an AFTER plugin returns an `intercept` action (responding directly to the client without contacting upstream), the JSONL line carries a top-level `plugin_intercepted: {type, id, hook}` marker (see § 5.2). Without this, intercepted traces would be indistinguishable from genuine upstream rejections.

### 6.5 Redaction policy

Body / header redaction is not in the capture path itself. Redaction MAY be implemented as an operator-opt-in plugin under the rules above; when it is, the recorded JSONL line reflects the post-redaction state.

---

## 7. Builtin plugins to ship (MVP)

Three plugins ship in the first BUILD commit. Two of them are operator-priority; the third is the surviving Phase A observer.

### 7.1 `text-replace` (BOTH hooks)

**Priority:** Operator-defined MVP #1 — concrete dual-direction test of the hook framework. Two example replacements:
- upstream: replace `"please"` with `"kindly"` in user-supplied content (trivial word-swap demo)
- downstream: replace any occurrence of an internal codename with a redacted placeholder before the response leaves the proxy

**Behavior:** A single plugin instance implements BOTH `BeforePlugin` and `AfterPlugin`. The `before` half walks `ParsedRequest.Messages[*].Content` (text parts only) and runs `strings.ReplaceAll` per match rule. The `after` half does the same on `ParsedResponse.Content` (and on each streaming `delta` event's text fragment for SSE). Each rule is an `{match, replace}` pair; literal substring match (not regex) for MVP — regex deferred to a follow-up rule shape.

**Config schema:**

```yaml
config:
  routes: ["/v1/*"]                                # path glob list; empty = all paths
  up:                                              # rules applied to inbound request content
    - {match: "please", replace: "kindly"}
  down:                                            # rules applied to outbound response content
    - {match: "internal-codename", replace: "[redacted]"}
```

Either `up` or `down` (or both) can be empty/omitted — instance only registers the corresponding hook(s). Multiple rules apply in declared order.

**Mutation surface:**
- BEFORE: mutates `ParsedRequest.Messages[*].Content` for text-typed parts only. Tool-call args, image blocks, reasoning blocks left alone.
- AFTER (text deltas only): mutates `ParsedResponse.Content` (final concatenated assistant text) for non-stream; for SSE, mutates each `content`-class delta event's text payload as it flows. **`tool_use` `input_json_delta` events are passed through UNTOUCHED even if the match string appears inside tool-call argument JSON** — this is the v1 carve-out per §10.6. Operators who want to redact secrets from tool-call arguments should run a BEFORE plugin that strips them from the request, an `ActionIntercept` on response, or an Observer-class JSONL post-write scrub. The streaming case keeps the framework's "mutate-as-it-streams" pattern from W1 for text content only.

**Out of scope for MVP:** regex matching, case-insensitive matching, named-capture replacements, per-route different rules (operator can declare two instances with different `routes` lists if needed — multi-instance shines here), AND tool-call argument mutation on the AFTER hook (see §10.6 / §12.3).

### 7.2 `text-append` (BOTH hooks)

**Priority:** Operator-defined MVP #2 — companion to `text-replace`, exercises the "append at known position" mutation pattern.

**Behavior:** Append fixed text to the end of a content payload. Two example uses:
- upstream: append `"\n\nBe concise."` to the system prompt (policy footer / persona enforcement)
- downstream: append `"\n\n— routed via api-log proxy"` to the final assistant content (provenance watermark)

A single plugin instance implements both hooks; either `up.suffix` or `down.suffix` (or both) controls behavior.

**Config schema:**

```yaml
config:
  routes: ["/v1/*"]
  up:                                              # append on inbound request
    suffix: "\n\nBe concise."
    target: "system_prompt"                        # "last_user_message" | "system_prompt"; default last_user_message
  down:                                            # append on outbound response
    suffix: "\n\n— routed via api-log proxy"
    target: "content"                              # "content" | "reasoning"; default content
```

`target` lets the BEFORE half land the suffix either at the last user message (the "ask politely" use case) or merged into the system prompt (the "policy footer" use case — subsumes the original `prompt_inject` idea without a separate plugin).

**Mutation surface:**
- BEFORE: appends to the `Content` of either the last `role: user` `Message` in `ParsedRequest.Messages` or `ParsedRequest.SystemPrompt` per `target`.
- AFTER: appends to `ParsedResponse.Content` or `ParsedResponse.Reasoning` per `target`. For SSE: emits one final framework-synthesized `delta` event carrying the suffix before the upstream's terminal `done` is forwarded.

**Out of scope for MVP:** prepend mode (use `text-replace` with `match=""` if ever needed — actually no, prepend is small if asked; just deferred), conditional append based on response status, per-model targeting.

**Why these two plugins as MVP (not watermark + prompt_inject):**

Watermark is essentially `text-append` with `target=content` AFTER-only. Prompt injection at the system-prompt position is essentially `text-append` with `target=system_prompt` BEFORE-only. The operator's `text-replace` + `text-append` pair is a strictly more general framework with shared multi-direction config — same instance handles both directions, fewer plugin types, lower cognitive overhead for new contributors. Watermark and prompt_inject remain valid named ideas; if a future contributor wants them as named plugins, they're 30-line wrappers over `text-append` with a frozen `target`. Same approach for any "named convenience plugin" — keep the small core (`text-replace`, `text-append`), let the brainstorm phase (post-MVP) propose named conveniences only when there's a real distinguishing config or behavior to lock in.

### 7.3 `pathfilter` (Observer class — Phase A, REVISITED)

`pathfilter` already exists in `internal/plugin/builtin/pathfilter/` as an Observer (`ObserveBeforeRecord`). It does NOT mutate or intercept the request/response; it only decides whether the recorded trace is written to disk.

**The reconciliation question:** does the new model fold pathfilter into the unified BEFORE/AFTER scheme, or keep it as a third class?

**Decision: keep it as a third Observer class.** Reasoning:

- Pathfilter's job is "should this trace be recorded?" — a question about the writer pipeline, not the forwarder. The BEFORE/AFTER hooks are about modifying client-facing behavior; pathfilter does not.
- Forcing pathfilter into AFTER as a "continue + drop-record flag" would require adding a new "drop_record" action to the hook vocabulary just to absorb one existing plugin. The unified model loses its simplicity.
- Observers compose cleanly with hooks. The pipeline becomes: BEFORE chain → forward → AFTER chain → respond → finalize → Observer chain → writer. Observers see the final trace; hooks shape what flows.

**The taxonomy:**

| Class | Hook points | Can mutate request? | Can mutate response? | Can intercept? | Can skip recording? |
|---|---|---|---|---|---|
| BEFORE | post-receive, pre-forward | yes | n/a | yes | no |
| AFTER | post-upstream-response, pre-client-send | n/a | yes | yes | no |
| Observer | post-finalize, pre-writer | no | no | no | yes |

Pathfilter stays in the Observer column. The Phase A `ObserveBeforeRecord` and `ObserveAfterRecord` interfaces are renamed for clarity:

- `ObserveBeforeRecord` → `ObserveOnFinalize(ctx, tr trace.Trace) (record bool, err error)` (drop-recording semantic)
- `ObserveAfterRecord` → `ObserveAfterWrite(ctx, tr trace.Trace)` (side-effecting only)

This rename is a Phase-A migration, low blast radius; the only existing implementer is `pathfilter`. BUILD W5 handles it.

### 7.4 `rate_limit_ip` (BEFORE hook) — deferred to post-MVP

Not in the first BUILD ship. Operator has the use case but watermark + prompt_inject prove the contract first. Once those are live, `rate_limit_ip` is the third plugin to add. Config sketch (for forward planning, not for BUILD W1–W4):

```yaml
config:
  limit_per_minute: 60
  burst: 10
  reject_status: 429
  reject_body_json: { "error": { "message": "rate limit exceeded" } }
  exempt_key_hashes: []
  scope: "ip"   # "ip" | "ip+route"
```

---

## 8. Viewer Settings UI

### 8.1 Section placement

A new top-level "Plugins" section in the Settings page (alongside the existing Media / Noise Filter / etc. sections).

### 8.2 List view

The section shows a table of registered instances:

| Type | Instance ID | Enabled | Brief summary | Actions |
|---|---|---|---|---|
| watermark | wm-public | ✓ | pattern: "—via my.proxy" • routes: /v1/* | Edit • Disable • Remove |
| prompt_inject | house-style | ✓ | inject_as_system: "Respond in…" • routes: /v1/* | Edit • Disable • Remove |
| watermark | wm-internal |   | pattern: "[INTERNAL]" • routes: /v1/responses | Edit • Enable • Remove |

Toolbar above the table: "Add plugin" button.

### 8.3 Add-instance flow

"Add plugin" opens a modal:

1. Pick **type** from a dropdown populated by GET `/api/plugins/types` (see § 8.5).
2. Enter **instance ID** (unique short string; validation: kebab-case, 1-64 chars, must be unique).
3. The modal renders an **inline form** for the type's config schema (each builtin type exposes a JSON-schema-like descriptor; the form generator handles primitive fields, string arrays, and nested objects).
4. Submit → PUT `/api/config/plugins` with the full instance list + new entry appended.

### 8.4 Edit / disable / remove

- **Edit**: inline form pre-populated with the instance's current config; submit → PUT `/api/config/plugins/{id}` with single-instance update.
- **Disable / Enable**: toggle button; PUT `/api/config/plugins/{id}` with `{enabled: false}` patch.
- **Remove**: confirmation dialog; PUT `/api/config/plugins` with the full list minus this instance.

### 8.5 Backend API surface

Five endpoints, all under `/api/`. Lives in `internal/api/config.go` alongside the existing media endpoints.

| Method | Path | Body | Returns | Description |
|---|---|---|---|---|
| GET | `/api/plugins/types` | — | `[{type, description, config_schema}, ...]` | List all builtin plugin types and their config descriptors. Used by the Add modal to populate the dropdown and render the form. |
| GET | `/api/config/plugins` | — | `{instances: [...], source: "yaml" or "override"}` | Get the effective plugin list. `source` indicates whether the active list comes from YAML defaults or runtime overrides. |
| PUT | `/api/config/plugins` | `{instances: [...]}` | `{ok: true, instances: [...], errors: [...]}` | Full replace of the runtime-override plugin list. Triggers atomic registry rebuild. If any new instance fails Init, returns error and does NOT swap. |
| PUT | `/api/config/plugins/{id}` | `{type?, enabled?, config?}` | `{ok: true, instance: {...}}` | Single-instance update. Backend loads current list, finds by id, applies patch, rebuilds. |
| DELETE | `/api/config/plugins` | — | `{ok: true, source: "yaml"}` | Clear runtime overrides; revert to YAML. |

### 8.6 Config schema descriptor

Each builtin plugin type provides a config schema at registration time:

```go
type ConfigSchema struct {
    Fields []ConfigField `json:"fields"`
}

type ConfigField struct {
    Name        string         `json:"name"`
    Label       string         `json:"label"`
    Type        string         `json:"type"`        // "string" | "int" | "bool" | "string_array" | "enum"
    Default     any            `json:"default,omitempty"`
    Enum        []string       `json:"enum,omitempty"`
    Description string         `json:"description,omitempty"`
    Required    bool           `json:"required,omitempty"`
}
```

The viewer Add/Edit form generator reads this and renders the appropriate input control per field. No free-form JSON editing in MVP (operator can always edit `config.yaml` directly if they need something exotic).

### 8.7 Persistence

All edits go to `<DataDir>/runtime_overrides.json` via the extended `runtime.SaveOverride()` (see § 3.3.3). The viewer never writes `config.yaml` directly; YAML is the operator-controlled declarative source, runtime_overrides.json is the dynamic state.

---

## 9. Scope estimate

This is the BUILD-phase work-package plan, not part of the frozen contract. ~ 2–3 days of work, likely 4–6 commits across two repos (api-log + api-log-viewer).

| WP | Repo | Scope | Approx. effort |
|---|---|---|---|
| W1 | api-log | Hook framework: `ParsedRequest` / `ParsedResponse` builders (reusing parser shapes), `BeforePlugin` / `AfterPlugin` interfaces, registry with multi-instance support, `defer recover()` dispatcher, atomic.Pointer for live swap. | 0.75 day |
| W2 | api-log | Builtin plugins: `watermark` (after) + `prompt_inject` (before). Each ships with config schema, unit tests, and a small fixture-based integration test that runs the plugin against a recorded trace fixture. | 0.5 day |
| W3 | api-log | Runtime overrides extension: extend `Overrides` struct, GET/PUT `/api/config/plugins` + `/api/plugins/types` + per-instance PUT. Wire to atomic-rebuild. | 0.5 day |
| W4 | api-log-viewer | Settings → Plugins section UI: list view, add/edit modal, config-schema form generator, API integration. | 0.75 day |
| W5 | api-log | Phase A migration: rename `ObserveBeforeRecord` → `ObserveOnFinalize`, `ObserveAfterRecord` → `ObserveAfterWrite`. Update `pathfilter` to the new names. Add Observer-class call site to the finalize block. | 0.25 day |
| W6 | api-log | Docs: recording-behavior rules from § 6 above, ARCHITECTURE update, README plugin section. | 0.25 day |

W1 and W3 unblock W4; W2 depends on W1; W5 is independent and can run in parallel.

Ratification gate: this spec (file you are reading) is the gate. BUILD-W1 does not start until operator confirms the checklist in § 11.

---

## 10. Open questions

The remaining ambiguities the inventory surfaced. Each has a *recommended answer* — operator can ratify or override.

### 10.1 Per-instance enable/disable vs config-only

**Q:** Does the viewer Settings UI for plugin instance config expect a separate enable/disable flag, or just config overrides?

**Recommended:** Separate `enabled` boolean per instance (§ 3.3). Easier UX, easier to disable without losing config, matches the convention of the existing media `save_attachments` toggle.

### 10.2 Instance identification

**Q:** Are multi-instances identified by `(type, name)` tuple or by a single string ID?

**Recommended:** Single string `id` field, unique across all instances. Cleaner for URLs (`PUT /api/config/plugins/{id}` is one path segment), cleaner for trace markers (`plugin_intercepted.id`), and lets the operator change the *type* of an instance via the API without changing its identity. See § 3.1.

### 10.3 AFTER hook timing

**Q:** Does AFTER map to current Phase A `AfterRecord` (post-write-ack) or to a new hook before finalize?

**Recommended:** A new hook running *before finalize, after upstream response is in hand, before the response is streamed to the client*. This is what enables AFTER to mutate the response visibly. The existing `AfterRecord` (now `ObserveAfterWrite`) keeps its post-write-ack semantics for side-effecting observation only. Two distinct call sites, two distinct classes.

### 10.4 Intercept marker shape

**Q:** Should intercepted traces use `Trace.Mutations[]{action: "intercepted"}` or a top-level field?

**Recommended:** Top-level field `plugin_intercepted: {type, id, hook}` (§ 5.2). Since the operator decided to NOT record mutations at all, the only thing we DO record is the fact of interception — a top-level field is simpler than reviving a mutations array for one use case.

### 10.5 Plugin-types discoverability

**Q:** Does the viewer need an API to list available plugin types, or does the operator manually maintain that knowledge?

**Recommended:** Yes — `GET /api/plugins/types` (§ 8.5). The viewer Add modal needs this to populate the type dropdown and to render the config form per type's schema. Built-in registry exposes the list at process start; viewer just queries.

### 10.6 Tool_call AFTER mutation — deferred to Phase D

**Q:** Should v1 ship AFTER-hook mutation of streaming `tool_call` argument fragments?

**Recommended (operator-ratified 2026-05-30 via 4-lens adversarial workflow):** **No — defer to Phase D**, not permanent cut. Rationale grounded in the 4-perspective analysis:

| Lens | Verdict | Key finding |
|---|---|---|
| OSS adopter | cut | 7 enumerated realistic adopter personas; **zero** named a real AFTER tool_call arg mutation use case. Every "I want to control tools" maps to BEFORE-tools-array (whitelist), `ActionIntercept` (block whole response), or Observer-class JSONL scrub. |
| Maintenance burden | phase-d | ~1400 LOC of bidirectional per-protocol SSE re-emitter (Anthropic / OpenAI Chat / OpenAI Responses / Gemini all differ) with zero named adopter = textbook vendor-wire-format trap. Defer until demand. |
| Architectural elegance | phase-d | Frozen `ParsedResponse` already carries BOTH `Events` (raw streaming view) AND `ToolCalls` (post-stream buffered view). A future Tier-2 `OnToolCall(complete_args)` callback is purely **additive** — zero v1 plugin rework. |
| Use-case substitution | cut | Every imagined use case has a strictly better home: tool whitelist → LiteLLM / Portkey YAML; skill injection → BEFORE-side JSON edit; arg sanitization → executor-side hooks (MCP perms, Claude Code allowlist) where it actually stops execution. |

Synthesizer's call: ship-phase-d. The split was 2-2, but **defer and cut produce IDENTICAL v1 binaries** (since `ParsedResponse.Events` + `ToolCalls` are already in the frozen contract); defer preserves no-cost optionality without committing schema. Re-open Phase D only on a real adopter filing an issue with a concrete use case that BEFORE-tools-array / `ActionIntercept` / Observer-scrub genuinely can't serve.

**v1 contract for AFTER hooks (carve-out enforced):**
- `text-replace` AFTER half mutates ONLY text-content delta events (`content_block_delta` with `text_delta` type for Anthropic; `delta.content` for Chat; `response.output_text.delta` for Responses; `text` part for Gemini). `tool_use` `input_json_delta` events pass through untouched.
- `text-append` AFTER half emits a synthesized final text delta. Tool_call events untouched.
- `AfterResult.Mutated` MAY modify `ParsedResponse.Content` / `Reasoning`. Mutating `ParsedResponse.ToolCalls` in v1 is undefined behavior — the dispatcher won't re-emit changed tool_call args, the recorded JSONL line carries the pre-plugin args, and the client receives the upstream's original tool_calls. Document this clearly so plugin authors don't accidentally rely on it.

**Phase D design when it ships:**

Add a separate optional interface (Go stdlib `io.WriterTo` / `io.ReaderFrom` evolution pattern):

\`\`\`go
type ToolCallMutator interface {
    OnToolCall(ctx context.Context, req *ParsedRequest, call *ToolCall) ToolCallResult
}

type ToolCallResult struct {
    Action  Action          // ActionContinue | ActionMutate | ActionIntercept
    Mutated *ToolCall       // when Action == ActionMutate (complete args replaced)
    // ActionIntercept on a single tool_call drops it; full-response intercept stays AfterResult.
}
\`\`\`

The dispatcher detects opt-in via type assertion: `if tc, ok := plugin.(ToolCallMutator); ok { ... }`. Plugins that don't implement it pay zero cost. The buffer-then-expose machinery only spins up when at least one registered plugin implements `ToolCallMutator`. Existing v1 `BeforePlugin` / `AfterPlugin` implementations are 100% UNTOUCHED.

This pattern (additive optional interfaces detected by type-assertion at dispatch time) is exactly how Go's stdlib evolves `io.Reader` / `io.Writer` with `io.WriterTo` / `io.ReaderFrom` — no signature breakage, opt-in capability, no preemptive schema commitment.

---

## 11. Operator ratification checklist

Operator confirms each item before BUILD W1 launches. Yes/no per line.

- [ ] Two hooks only (BEFORE / AFTER); both can `Continue` / `Mutate` / `Intercept`. — yes / no
- [ ] Intercept response can be any HTTP status (200, 4xx, 5xx) and any body, plugin chooses. — yes / no
- [ ] MVP plugins to ship: `text-replace` (both hooks, multi-direction) + `text-append` (both hooks, multi-direction) per §7.1 / §7.2 — these subsume the earlier `watermark` / `prompt_inject` placeholder names. `rate_limit_ip` is post-MVP. — yes / no
- [ ] Multi-instance per type: yes (operator declares `(type, id, config)` tuples in a list). — yes / no
- [ ] Pathfilter / Observer stays as a separate third class (not folded into before/after); rename Phase A's `ObserveBeforeRecord` to `ObserveOnFinalize`. — yes / no
- [ ] Mutation recording: NO (record post-mutation only; trade audit for simplicity). — yes / no
- [ ] Intercept marker on JSONL line as `plugin_intercepted: {type, id, hook}`. — yes / no
- [ ] Plugin failure (error or panic) is fail-open: log WARN, continue with original; never block forwarding. — yes / no
- [ ] Viewer Settings → Plugins section: list + add/edit/disable/remove via config-schema-driven form. — yes / no
- [ ] Recording-behavior rules per § 6 above (opt-in plugins; post-mutation only; named-field discipline; plugin_intercepted marker; redaction-via-plugin only). — yes / no
- [ ] Persistence: extend `runtime_overrides.json` with a `plugins` block; full-list replace on PUT (no merge-by-id). — yes / no
- [ ] BUILD scope estimate (~ 2–3 days, 6 WPs as in § 9) is roughly right. — yes / no
- [ ] **AFTER tool_call carve-out:** AFTER plugins may only mutate `ParsedResponse.Content` / `Reasoning` text in v1; tool_call argument mutation on the AFTER hook is **deferred to Phase D** per §10.6 and requires a named adopter use case to unlock. — yes / no
- [ ] **text-replace AFTER pass-through:** the AFTER half of `text-replace` explicitly passes `tool_use` `input_json_delta` events through untouched (even if the match string appears in tool-call argument JSON); operators wanting that effect should use BEFORE-side stripping, full-response `ActionIntercept`, or Observer-class JSONL scrub. — yes / no
- [ ] **Forward compatibility:** when Phase D arrives, tool_call mutation lands as a SEPARATE optional interface (`ToolCallMutator`) detected by type assertion (Go stdlib `io.WriterTo` / `io.ReaderFrom` pattern), so existing v1 `BeforePlugin` / `AfterPlugin` implementations are UNTOUCHED. The dispatcher gains buffer-then-expose machinery only when at least one registered plugin opts in via the new interface. — yes / no

---

## 12. Appendices

### 12.1 Glossary

- **Hook**: a call site in the request/response path where plugins run (BEFORE or AFTER).
- **Hook plugin**: a plugin implementing `BeforePlugin` or `AfterPlugin`. Can mutate or intercept.
- **Observer**: a plugin implementing `ObserveOnFinalize` or `ObserveAfterWrite`. Cannot mutate user-facing behavior; can drop recording.
- **Instance**: a single configured running plugin, identified by `id`.
- **Type**: the Go type behind a plugin, identified by `type` name (e.g. "watermark").
- **Builtin**: a plugin type shipped in-tree under `internal/plugin/builtin/`.
- **Effective config**: YAML defaults merged with `runtime_overrides.json` (overrides win when non-nil).
- **Intercept**: short-circuit the pipeline; plugin produces the final response.
- **Mutate**: replace the parsed object the next plugin (or the forwarder) sees.

### 12.2 Cross-references

- Phase A plugin scaffold: `api-log/internal/plugin/plugin.go`
- Existing pathfilter: `api-log/internal/plugin/builtin/pathfilter/`
- Runtime overrides pattern: `api-log/internal/runtime/overrides.go`
- Phase K media contract (overrides shape precedent): [`docs/specs/phase-k-media-contract.md`](./phase-k-media-contract.md)
- Original 5-hook design draft (obsolete; superseded by this document)
- Parser shapes: `api-log/internal/parser/parse.go`, `usage.go`
- Trace shape: `api-log/internal/trace/`
- Smart XFF resolution (provides `ClientIP`): commit on 2026-05-30 covering `internal/proxy/xff.go`

### 12.3 Out-of-scope reminder list

For each "out of scope" claim in the spec, the explicit reason:

| Out of scope | Reason | Belongs to |
|---|---|---|
| Model routing (rewrite model field) | the upstream gateway already owns this. | upstream layer |
| Per-key rate limit | the upstream gateway owns this. | upstream layer |
| Per-key budget | the upstream gateway owns this. | upstream layer |
| Semantic cache short-circuit | Not a recorder concern; complex. | external service if needed |
| Encryption / decryption | Operator: community DIY; not shipping crypto. | third-party plugins |
| Mutation diff audit | Out of scope per § 6.2 above. | dev profile / future plugin |
| Cryptographic watermark | Out of MVP scope; later plugin. | future plugin |
| Hot-reload of builtin types | Restart the process. (Plugins are in-tree Go code.) | n/a |
| Streaming `tool_call` argument mutation on AFTER hook | No named OSS adopter; better-solved at BEFORE (strip tools from `ParsedRequest.Tools`), `ActionIntercept` (replace whole response), or Observer (scrub recording). See §10.6 for the full 4-lens analysis. | Phase D if demand emerges via a real GitHub issue |

### 12.4 What this spec does NOT freeze

- The exact wire shape of the `ConfigSchema` descriptor (§ 8.6). Two implementers can disagree on field names; W3 picks a final shape and updates this spec.
- The viewer UI's visual design. § 8 freezes the API surface and the information architecture; the actual layout / colors / etc. follow Phase L's design language.
- The plugin-error breadcrumb max length / retention policy. § 5.3 says "small array"; W1 picks a numeric cap (recommendation: keep last 4 entries; trim oldest).
- The exact set of routes the watermark plugin's default config matches. § 7.1 shows `/v1/*` as illustration; W2 picks a sensible default and documents it.

### 12.5 Migration / deprecation

When this contract lands:

- The earlier 5-hook design draft is removed from the tree (superseded; pointer to this doc only).
- The two interfaces renamed in W5 (`ObserveBeforeRecord` → `ObserveOnFinalize`, `ObserveAfterRecord` → `ObserveAfterWrite`) are a breaking change to the Phase A package, but the package has only one in-tree implementer (`pathfilter`) and zero external consumers, so the migration is a single commit.
- The `ROADMAP.md` plugin-section entry should be updated to point at this spec.

— end of plugin-b-c-spec.md —
