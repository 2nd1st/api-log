---
name: Bug report
about: Capture path, read API, viewer, or plugin behaving incorrectly.
title: ""
labels: bug
assignees: ""
---

<!--
Before filing: search existing issues. Best-effort maintenance, one
contributor, no SLA. A report with the fields below filled in moves
much faster than a report without them.

Redact bearer tokens / API keys before pasting JSONL or logs. The
capture path stores them unredacted on disk (this is documented; see
SECURITY.md) but they do not belong in a public issue.
-->

## Version

```
$ api-log -version
# paste output
```

If running from `main`, paste the commit SHA (`git rev-parse HEAD`).

## Deployment shape

- Install method: Docker Compose / native binary / `go install` / built from source
- OS + arch: e.g. `linux/amd64`, `darwin/arm64`
- Upstream gateway in front of which api-log is deployed (sub2api / CPA / new-api / other / none)
- Reverse proxy in front of api-log, if any (Caddy / nginx / none)

## Steps to reproduce

1. ...
2. ...
3. ...

**Expected:** what should have happened.

**Actual:** what did happen.

If the bug involves a specific HTTP request, paste the request shape
(method, path, relevant headers with credentials redacted, body
shape — not the literal body if it carries customer data).

## `/healthz` output

The counters and storage blocks are the maintainer's primary
diagnostic anchor. Capture them at the time of the incident if you
still can:

```bash
curl -s http://localhost:7862/healthz | jq '{counters, storage}'
```

```json
# paste output here
```

## Sample JSONL line (if capture-related)

If the bug is about what got recorded (missing fields, wrong shape,
truncation, parse failure), paste one JSONL line that reproduces it.
Redact `Authorization` / `x-api-key` / equivalent header values
yourself before pasting.

```json
# paste here
```

## Logs around the incident

stderr from the api-log process (or `docker logs` / `journalctl -u
api-log`) covering ~30 seconds before and after the incident.

```
# paste here
```

## Anything else

Plugin config in effect (`config.yaml` plugins block + any
`PUT /api/config/plugins` overrides), retention settings, anything
non-default that might be relevant.
