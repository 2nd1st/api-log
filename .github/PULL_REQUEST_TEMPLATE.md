<!--
api-log is best-effort, one contributor, no SLA. PRs that fill in the
boxes below land faster than PRs that do not.

If this PR changes the JSONL line shape, the on-disk layout, or the
HTTP read API contract: read PHILOSOPHY.md § principle 6 first.
Wire-format contracts follow append-only / new-format-key migration
discipline; field renames and removals do not land.

If this PR adds capture-path behavior, plugin scope, or anything
that touches what gets recorded: read ROADMAP.md § "What we will
not do" first.
-->

## What changed + why

One paragraph. The "why" matters more than a file-by-file recap of
the "what" — the diff already shows the what.

## Trade-off chosen

Name the alternative you considered and rejected, and the reason.
"Did the obvious thing" is acceptable for small changes; for
anything touching the capture path, read API, plugin surface, or
on-disk format, name the alternative explicitly.

## Tests

- [ ] Added unit / integration tests covering the change.
- [ ] No tests added — reason: <!-- e.g. docs-only, mechanical refactor with existing coverage -->

## Local checks

- [ ] `go test -race ./...` clean.
- [ ] `golangci-lint run` clean (v2 config).
- [ ] Integration harness `./tests/integration/run.sh` clean (if the change touches the proxy / read API / plugin path).

## CHANGELOG

- [ ] Added an entry under `## [Unreleased]` in `CHANGELOG.md`.
- [ ] No CHANGELOG entry — reason: <!-- e.g. internal-only refactor, test-only change, docs typo -->

If yes, paste the entry you added:

```
### Added / Changed / Fixed / Removed
- ...
```

## Docs

- [ ] `README.md` / `README.zh.md` touched if user-visible surface changed.
- [ ] `ARCHITECTURE.md` touched if on-disk format, read API contract, or write path changed.
- [ ] `ROADMAP.md` touched if this lands a tracked item or moves the "deferred" list.
- [ ] `SECURITY.md` touched if the threat model or operator responsibilities changed.
- [ ] No docs change needed.

## Linked issue

Closes #<!-- number --> <!-- or "n/a" with a one-line reason -->
