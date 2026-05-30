# Security

## Reporting a vulnerability

Open a private security advisory via the repository's GitHub Security tab.
If you cannot use GitHub, email the maintainer with subject `[api-log security]`.
Please do not file a public issue for a security report.

## Threat model

api-log is a transparent recording proxy. The threats most relevant to its
design — and the ones a security review should focus on — are:

- **Process crash / OOM affecting the client request.** Principle 3
  ("succeed visibly on forwarding") makes this the only failure surface
  that reaches the client. Any code path that can panic during forwarding
  is a security concern in the availability sense.
- **Capture-side resource exhaustion.** Unbounded tmp file growth,
  writer-channel saturation feeding back to forwarding latency, etc. The
  per-direction `max_body_bytes` cap and lossy channel design (§7.2)
  bound this; bugs that violate those bounds are vulnerabilities.
- **Command / SQL / path injection** in operator-controlled YAML / env
  configuration, in the read API filter parameters, or in the JSONL
  rebuild path scanning `data/`.
- **Replay endpoint misuse.** `/api/traces/:id/replay` re-emits
  reconstructed SSE bytes to the API caller. A bug that lets `/replay`
  re-contact the upstream gateway is in scope.
- **Read-API auth bypass.** The admin bearer token mediates the read
  API surface. Constant-time comparison is required; timing leaks are
  in scope.

## Not in scope — documented operator responsibilities

These are not vulnerabilities; they are deliberate design choices the
operator must understand and manage. Reports about these will be closed.

- **Bearer tokens and API keys land on disk in plaintext.** The JSONL
  files contain `Authorization` / `x-api-key` headers exactly as the
  client sent them. The capture path itself performs no redaction:
  the forwarding goroutine is byte-faithful, and the JSONL line
  records the upstream request bytes and the client-received response
  bytes exactly as they flowed across the wire. Treat the `data/`
  directory the way you would treat a file holding production API
  keys: file-system permissions, disk encryption, and log-pipeline
  policy are the operator's responsibility. If you need redaction,
  run a downstream sidecar over the JSONL files.

  Operator-opt-in plugins MAY mutate the response stream. AFTER-class
  plugins registered in `config.yaml` or `runtime_overrides.json` can
  rewrite `ParsedResponse.Content` and `ParsedResponse.Reasoning` text
  deltas; when they do, the JSONL line records the post-mutation
  bytes — the bytes the client actually received — per the
  mutation-recording rule in `docs/specs/plugin-b-c-spec.md` § 5.
  This is not redaction-at-capture: nothing runs unless the operator
  declared the instance in config, and the recorded line is the
  honest record of what the client received.

  Currently shipped plugins do not touch headers. `text-replace` and
  `text-append` operate on response body content (and the BEFORE-side
  request body) only. No shipped or planned plugin strips
  `Authorization` / `x-api-key` from captured JSONL — header
  protection on disk remains the operator's filesystem-permissions
  responsibility.

  If a future plugin adds header mutation, it must follow the same
  post-mutation recording discipline: the JSONL line records what
  actually flowed after the plugin chain ran. Silent header rewrites
  — mutating headers on the wire while recording the pre-mutation
  bytes, or vice versa — are not accepted into this repo.

  A plugin that bypasses these rules is in scope for security
  reports. Specifically: mutating without recording the post-mutation
  state, accessing storage paths outside its declared scope, or
  leaking per-request state across requests.
- **No client authentication on the proxy listener.** Any client that
  can reach the listener can submit traffic, exactly as if they could
  reach the gateway directly. Restrict network access at the operator
  layer.
- **Plain HTTP between containers.** api-log listens HTTP only.
  TLS termination is the operator's reverse proxy.
- **Replay correctness is best-effort, not adversarial.** Reconstructed
  SSE frames in `/replay` are not byte-identical to the wire bytes the
  upstream emitted (see ARCHITECTURE.md § 6.4). Trusting replay output
  as a substitute for the original is the consumer's call.

## Supported versions

v0 is in development; no version is currently supported in the
production-security sense. Will be filled in once v0 ships.
