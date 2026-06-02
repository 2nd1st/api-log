# Contributing

Thanks for the interest. This document covers the bar a patch needs to
clear before it lands. It applies to the `api-log` backend repo only;
the companion [`api-log-viewer`](https://github.com/2nd1st/api-log-viewer)
has its own conventions.

## Maintainer reality

This is a one-contributor project. PRs are reviewed best-effort with
**no SLA**. Small, well-scoped patches that follow the conventions below
land faster than large rewrites. A patch that has not been triaged in
two weeks is not lost; ping the PR thread once and wait.

I will close PRs that don't match the project scope (see
[ROADMAP §"What we will not do"](./ROADMAP.md#what-we-will-not-do)) or
that introduce features I'm not willing to maintain. That is not a
quality judgement on the patch; forking is a fine outcome and
explicitly OK — see [`.github/GOVERNANCE.md`](./.github/GOVERNANCE.md).

## Before opening a PR

Run these locally. CI runs the same set; failing CI blocks merge.

```bash
# 1. Tree builds
go build ./...

# 2. Tests + race detector clean
go test -race -count=1 ./...

# 3. Lint clean. CI pins golangci-lint v2.0 — match it locally to
#    avoid version skew. The .golangci.yml uses the v2 schema; v1.x
#    silently passes broken configs.
docker run --rm -v "$PWD":/work -w /work \
  golangci/golangci-lint:v2.0 golangci-lint run

# 4. Integration harness (dev-stack — no external upstream needed)
bash tests/integration/run.sh
```

If you touch the read API contract, the JSONL trace shape, or the
on-disk layout, also update [ARCHITECTURE.md](./ARCHITECTURE.md) in the
same PR.

## Code style

- `gofmt -s` clean. CI enforces this.
- `golangci-lint run` clean against the pinned v2.0 image. Disable a
  linter in `.golangci.yml` only with a justification comment; do not
  add per-line `//nolint` without a reason.
- No new `TODO` / `FIXME` / `XXX` markers unless they reference a
  tracked roadmap item or GitHub issue in the same comment, e.g.
  `// TODO(roadmap §day-2-ops): wire WAL checkpoint policy`.
- Keep imports goimports-ordered (stdlib / third-party / local groups).
- Tests live next to the code (`pkg_test.go`). Integration scenarios
  go under `tests/integration/`.
- Public-API doc comments on exported types and functions in `internal/`
  packages are encouraged but not required; doc comments on anything
  in `cmd/` and on JSONL / SQLite schema types are required.

## Restraint bar

api-log keeps a small core. The design discipline is:

> Small core + many plugins. Cutting from core is the rule; re-homing
> to a plugin is the resolution.

In practice that means:

- New behavior that does not fit the four supported plugin classes
  (`text-replace`, `text-append`, `path-filter`, future BEFORE/AFTER
  mutators per `docs/specs/plugin-b-c-spec.md`) needs an explicit
  argument for why it belongs in core.
- Anything in [ROADMAP §"What we will not do"](./ROADMAP.md#what-we-will-not-do)
  is closed on sight — no gateway features (auth, routing, retries,
  rate limiting, caching, request rewriting), no SDK instrumentation,
  no semantic interpretation of recorded content, no automatic
  redaction in the capture path, no bundled "smart" middlebox
  behavior, and no matching on credential-carrying header values.
- "Make it configurable" is not a refactor that lands without an
  adopter use case. See the gate-before-feature note in GOVERNANCE.

If a patch lives in a grey zone, open an issue first with the use case.
A 200-line PR that gets closed is more friction than a five-line issue
that gets a "yes, send it" reply.

## PR scope

- **One logical change per PR.** A refactor + a bug fix + a doc update
  is three PRs, not one.
- **Squash commits with descriptive messages.** Multi-commit PRs are
  fine in flight, but I will ask you to squash before merge if the
  intermediate commits are not independently meaningful. Match the
  existing commit-message voice (see `git log --oneline -50`): short
  imperative subject, optional body explaining the why.
- **Update CHANGELOG.md** under `## [Unreleased]` for any user-visible
  change. Follow the Keep-a-Changelog sections (Added / Changed /
  Fixed / Security).
- **Do not bump version constants in PRs.** Releases are cut by the
  maintainer per [RELEASING.md](./RELEASING.md).

## Reporting bugs

Open a GitHub issue. Include:

- api-log version (`api-log -version` or image tag)
- Go version + OS (if building from source)
- A minimal reproduction — the smallest config that reproduces the
  behavior, the request that triggered it, and the resulting JSONL
  line or SQLite row (redact bearer tokens before pasting)
- Expected vs. actual behavior

Security-sensitive reports go through the channel documented in
[SECURITY.md](./SECURITY.md), not the public issue tracker.

## Proposing a feature

Open an issue describing the use case before writing code. The
useful issue shape is:

1. The problem in one paragraph.
2. The deployment context — what gateway, what client mix, how many
   trace lines/day.
3. Why the existing surface (read API, plugin system, downstream
   tooling against JSONL) does not solve it.
4. A sketch of the proposed change.

A "yes, send a PR" reply is the gate; absent that, a feature PR is at
high risk of being closed unmerged.

## License

By contributing, you agree that your contribution will be licensed
under the same [MIT License](./LICENSE) as the rest of the project.
No CLA is required.
