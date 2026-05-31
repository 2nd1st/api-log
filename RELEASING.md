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

## Identity migration (one-time, before the first public push)

The Go module path is currently `github.com/leoyun/api-log` to match
the operator's internal gitea. The public GitHub URL will be
`github.com/xiayangzhang/api-log`. Migration is a single commit that
must land before the first GitHub push:

```bash
# Rewrite the module path
go mod edit -module github.com/xiayangzhang/api-log

# Update every internal import path
grep -rln '"github.com/leoyun/api-log/' --include='*.go' \
  | xargs sed -i '' 's|"github.com/leoyun/api-log/|"github.com/xiayangzhang/api-log/|g'

# Verify
docker run --rm -e CGO_ENABLED=1 -v "$PWD":/work -w /work \
  golang:1.22 sh -c 'go build ./cmd/api-log && go test -race -count=1 -short ./...'

# Commit + push to gitea ONCE before flipping to GitHub
git add go.mod *.go internal/ cmd/
git commit -m "chore: rename module to github.com/xiayangzhang/api-log"
git push gitea main
```

Note: the operator's internal gitea remote stays at `leoyun/api-log`;
only the canonical module + GitHub URL move.

## Secret hygiene (filter-repo, one-time)

The git history contains the leaked sub2api user keys removed from
HEAD in commit `3f22001`. These must be scrubbed from history before
any push to a public remote.

```bash
# Prereq: install git-filter-repo (https://github.com/newren/git-filter-repo)
brew install git-filter-repo

# Create a replacements file matching the two leaked keys
cat > /tmp/key-scrub.txt <<EOF
literal:sk-REDACTED==>sk-REDACTED
literal:sk-REDACTED==>sk-REDACTED
EOF

# Take a backup before rewriting
git clone --mirror . /tmp/api-log-backup-$(date +%s).git

# Rewrite all history
git filter-repo --replace-text /tmp/key-scrub.txt --force

# Force-push to gitea (destroys history!) — this is a deliberate
# one-time event during the pre-public-push window
git push --force gitea main

# Verify the keys are gone
git log -p --all -S 'sk-14ab1372' | head    # expected: no output
git log -p --all -S 'sk-77c95b97' | head    # expected: no output

# DELETE the backup once you're satisfied the public push is clean
# (otherwise the leaked keys live on at /tmp/api-log-backup-*.git)
rm -rf /tmp/api-log-backup-*.git
rm /tmp/key-scrub.txt
```

The sub2api gateway whose keys these are is the operator's own; the
keys are not vendor credentials. Operator note: rotate via the
sub2api admin UI after public push, regardless.

## GitHub repo creation

```bash
# Authenticated as xiayangzhang
gh repo create xiayangzhang/api-log \
  --description "Transparent HTTP recording proxy for LLM gateway observability" \
  --public \
  --homepage "https://github.com/xiayangzhang/api-log"

gh repo create xiayangzhang/api-log-viewer \
  --description "Svelte 5 browser UI for api-log traces" \
  --public \
  --homepage "https://github.com/xiayangzhang/api-log-viewer"

# Topics aid SEO / GitHub search
gh repo edit xiayangzhang/api-log --add-topic llm-observability \
  --add-topic openai-proxy --add-topic anthropic --add-topic jsonl \
  --add-topic sqlite --add-topic golang --add-topic tcpdump

gh repo edit xiayangzhang/api-log-viewer --add-topic svelte5 \
  --add-topic typescript --add-topic llm-observability \
  --add-topic api-log
```

## First push

```bash
git remote add github git@github.com:xiayangzhang/api-log.git
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
`ghcr.io/xiayangzhang/api-log:0.1.0` + `:latest` via the workflow
in `.github/workflows/ci.yml`. Watch the Action run; the publish
typically completes in 3-5 minutes.

## Post-tag verification

```bash
# Pull the published image fresh
docker pull ghcr.io/xiayangzhang/api-log:0.1.0

# Run it and verify the version flag
docker run --rm ghcr.io/xiayangzhang/api-log:0.1.0 -version
# expected: v0.1.0 (or similar — see cmd/api-log/main.go ldflag)

# Smoke-test the image end-to-end
docker run -d -p 7861:7861 -p 7862:7862 ghcr.io/xiayangzhang/api-log:0.1.0
sleep 2
curl -s http://localhost:7862/ | jq '.viewer'
# expected: "https://github.com/xiayangzhang/api-log-viewer"
```

## Sub2gpt / sub2api migration to ghcr (optional, after tag)

Currently sub2gpt and sub2api build api-log from the local clone of
the operator's gitea. Once `:0.1.0` is on GHCR, switch the
`docker-compose.yml` `build:` clause to `image:` so subsequent rebuilds
pull the public image instead of building from source.

```yaml
# was
api-log:
  build:
    context: /opt/api-log
    dockerfile: Dockerfile

# becomes
api-log:
  image: ghcr.io/xiayangzhang/api-log:0.1.0
```

Restart the api-log service on each LXC. Verify with `docker logs`
that the version line reads `v0.1.0`, not `dev`.

## After the announcement

- Watch GitHub Issues for the first 48 hours; respond to anything
  flagged as a regression or a build break.
- Monitor `https://github.com/xiayangzhang/api-log/issues?q=is%3Aopen+label%3Abug`.
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
