# Releasing

This document is the operator checklist for cutting a public api-log
release. It applies to both repos in lockstep — `api-log` and
`api-log-viewer` ship as paired tags.

## Pre-flight (every release)

Run these in the local clone of each repo. Anything red blocks the tag.

### api-log (backend)

```bash
# 1. Tree is clean
git status --porcelain   # must be empty

# 2. CHANGELOG has a section for the version about to ship
grep "^## \[$VERSION\] - " CHANGELOG.md

# 3. ROADMAP "v0.1.0 review — deferred items" carried items match
#    GitHub issues OR are explicitly accepted as un-issued
ls docs/reviews/                       # punch list lives here

# 4. Tests + race detector clean
docker run --rm -e CGO_ENABLED=1 -v "$PWD":/work -w /work \
  golang:1.22 sh -c 'go build ./cmd/api-log && go test -race -count=1 -short ./...'

# 5. Lint clean (the v2 config + v7 action that landed in Bucket A)
docker run --rm -v "$PWD":/work -w /work \
  golangci/golangci-lint:v2.0 golangci-lint run

# 6. CI workflow YAML schema valid + the release job exists
grep -A2 "release:" .github/workflows/ci.yml

# 7. Dockerfile builds standalone
docker build -t api-log:rc .
docker run --rm -p 7861:7861 -p 7862:7862 api-log:rc &
sleep 2
curl -s http://localhost:7862/ | head -c 200    # JSON pointer expected
kill %1
docker rmi api-log:rc
```

### api-log-viewer

```bash
git status --porcelain   # must be empty
grep "^## \[$VERSION\] - " CHANGELOG.md
pnpm install --frozen-lockfile
pnpm check                 # 0 errors
pnpm test                  # 18/18 pass (or current)
pnpm build                 # JS gz < 100 KB
```

## Hosted viewer — version + SHA bump

The backend pins both a viewer version and a SHA-256 of the viewer's
`dist.zip` release asset in source. Releases are coupled: the viewer
tag MUST land before the backend tag.

1. In api-log-viewer: commit any last edits, push, then
   `git tag -a vX.Y.Z -m "..."` and `git push github vX.Y.Z`.
2. Watch the viewer's `release` job (added to
   `.github/workflows/ci.yml` in the hosted-viewer feature). Wait
   until `dist.zip` and `dist.zip.sha256` appear as release assets
   on github.com/2nd1st/api-log-viewer/releases/tag/vX.Y.Z.
3. Read `dist.zip.sha256` from that release. Copy the 64-char hex.
4. In api-log: bump the two constants in
   `cmd/api-log/viewer_pins.go`:

   ```go
   viewerVersion = "vX.Y.Z"
   viewerSha256  = "<64-char hex>"
   ```

   Commit:

   ```
   chore: pin viewer vX.Y.Z (sha256 <first-8>)
   ```

5. Push to the release remote. Smoke-test an installed deployment
   and verify that `GET /healthz` reports the new `viewer.version`
   and `viewer.source=cache` after the first request to `/viewer/`.
6. THEN proceed with the rest of the release-prep flow below
   (GitHub push, tag).

Skipping this dance — bumping the backend tag against a viewer tag
whose `dist.zip` hasn't been published yet — leaves `/viewer/`
serving 503 until the viewer release job finishes.

## GitHub repo creation

```bash
# Authenticated as 2nd1st
gh repo create 2nd1st/api-log \
  --description "Transparent HTTP recording proxy for LLM gateway observability" \
  --public \
  --homepage "https://github.com/2nd1st/api-log"

gh repo create 2nd1st/api-log-viewer \
  --description "Svelte 5 browser UI for api-log traces" \
  --public \
  --homepage "https://github.com/2nd1st/api-log-viewer"

# Topics aid SEO / GitHub search
gh repo edit 2nd1st/api-log --add-topic llm-observability \
  --add-topic openai-proxy --add-topic anthropic --add-topic jsonl \
  --add-topic sqlite --add-topic golang --add-topic tcpdump

gh repo edit 2nd1st/api-log-viewer --add-topic svelte5 \
  --add-topic typescript --add-topic llm-observability \
  --add-topic api-log
```

## First push

```bash
git remote add github git@github.com:2nd1st/api-log.git
git push -u github main
```

Verify the GitHub render: README.md displays the screenshots (for the
viewer repo), badges resolve, CI workflow runs and goes green.

## Cutting v0.1.0

```bash
# Backend
git tag -a v0.1.0 -m "v0.1.0 — first public release"
git push github v0.1.0

# Viewer
cd ../api-log-viewer
git tag -a v0.1.0 -m "v0.1.0 — first public release"
git push github v0.1.0
```

The backend CI release job watches `v*` tags and publishes
`ghcr.io/2nd1st/api-log:0.1.0` + `:latest` via the workflow
in `.github/workflows/ci.yml`. Watch the Action run; the publish
typically completes in 3-5 minutes.

### Release notes are auto-extracted from CHANGELOG.md

Both repos' release CI now extracts the `## [VERSION]` section
of `CHANGELOG.md` and uses it verbatim as the GitHub release body
(backend via `goreleaser --release-notes`; viewer via softprops
`body_path`). The fallback when the matching section is absent is
the tag annotation (`git tag -m "..."`), so the release body is
never empty.

What this means for the operator:

- **Update CHANGELOG.md BEFORE pushing the tag.** The version
  section that lives at the top under `## [Unreleased]` should be
  promoted to `## [X.Y.Z] - YYYY-MM-DD` in the same commit as the
  pin bump or release-prep work. After the tag fires, CI reads the
  CHANGELOG state at that commit.
- **You can still post-edit the release body** with `gh release
  edit X --notes "..."` if the CHANGELOG entry needs polish.
  GoReleaser is configured with `mode: keep-existing`, so a re-run
  of the CI release job does not clobber a manually-edited body.
- **No more "release page is blank" surprise** — that was the
  v0.1.1 / v0.1.2 retro-fix trigger.

## Post-tag verification

```bash
# Pull the published image fresh
docker pull ghcr.io/2nd1st/api-log:0.1.0

# Run it and verify the version flag
docker run --rm ghcr.io/2nd1st/api-log:0.1.0 -version
# expected: v0.1.0 (or similar — see cmd/api-log/main.go ldflag)

# Smoke-test the image end-to-end
docker run -d -p 7861:7861 -p 7862:7862 ghcr.io/2nd1st/api-log:0.1.0
sleep 2
curl -s http://localhost:7862/ | jq '.viewer'
# expected: "https://github.com/2nd1st/api-log-viewer"
```

## Deployment migration to GHCR (optional, after tag)

If an existing deployment builds api-log from a local source
checkout, switch the `docker-compose.yml` `build:` clause to
`image:` so subsequent rebuilds pull the public image instead of
building from source.

```yaml
# was
api-log:
  build:
    context: /opt/api-log
    dockerfile: Dockerfile

# becomes
api-log:
  image: ghcr.io/2nd1st/api-log:0.1.0
```

Restart the api-log service. Verify with `docker logs` that the
version line reads `v0.1.0`, not `dev`.

## After the announcement

- Watch GitHub Issues for the first 48 hours; respond to anything
  flagged as a regression or a build break.
- Monitor `https://github.com/2nd1st/api-log/issues?q=is%3Aopen+label%3Abug`.
- The first PRs from outside contributors land here; expect to spend
  time on the CONTRIBUTING.md gap surfaced in the v0.1.0 deferred
  items list.

## What this document is not

- A marketing plan. HN / Hacker News / Reddit / Twitter strategy is
  the operator's call; the announcement post is not a release-engineering
  artifact.
- A versioning policy. SemVer rules are in CHANGELOG.md per the
  Keep-a-Changelog format header.
- A back-compatibility contract. The wire-format + read-API
  compatibility story lives in ARCHITECTURE.md.
