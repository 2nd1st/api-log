# Third-party notices

This notice file is part of `api-log`'s MIT-licensed distribution
(see [LICENSE](./LICENSE)). The software listed below is bundled
into the `api-log` binary at build time and remains under its own
license. The list is generated from `go.mod` / `go.sum` for the
tagged release and is grouped by license type.

The `api-log-viewer` frontend is a separate project with its own
npm dependency tree. Adopters distributing the hosted viewer
bundle should consult
[`api-log-viewer/THIRD_PARTY_NOTICES.md`](https://github.com/2nd1st/api-log-viewer)
for that tree's notices; the backend distribution does not bundle
viewer assets.

---

## BSD-3-Clause

| Module | Version | Project URL |
|---|---|---|
| `modernc.org/sqlite` | v1.34.1 | https://gitlab.com/cznic/sqlite |
| `modernc.org/libc` | v1.55.3 | https://gitlab.com/cznic/libc |
| `modernc.org/memory` | v1.8.0 | https://gitlab.com/cznic/memory |
| `modernc.org/mathutil` | v1.6.0 | https://gitlab.com/cznic/mathutil |
| `modernc.org/strutil` | v1.2.0 | https://gitlab.com/cznic/strutil |
| `modernc.org/token` | v1.1.0 | https://gitlab.com/cznic/token |
| `modernc.org/gc/v3` | v3.0.0-20240107210532-573471604cb6 | https://gitlab.com/cznic/gc |
| `github.com/google/uuid` | v1.6.0 | https://github.com/google/uuid |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | https://github.com/remyoudompheng/bigfft |
| `golang.org/x/sys` | v0.22.0 | https://pkg.go.dev/golang.org/x/sys |

## MIT

| Module | Version | Project URL |
|---|---|---|
| `github.com/dustin/go-humanize` | v1.0.1 | https://github.com/dustin/go-humanize |
| `github.com/mattn/go-isatty` | v0.0.20 | https://github.com/mattn/go-isatty |
| `github.com/ncruces/go-strftime` | v0.1.9 | https://github.com/ncruces/go-strftime |

## MIT and Apache-2.0 (dual)

| Module | Version | Project URL |
|---|---|---|
| `gopkg.in/yaml.v3` | v3.0.1 | https://github.com/go-yaml/yaml |

The yaml.v3 source tree carries per-file MIT and Apache-2.0
headers; both licenses apply. See the upstream `LICENSE` and
`NOTICE` files for the per-file split.

## Apache-2.0

| Module | Version | Project URL |
|---|---|---|
| `github.com/oklog/ulid/v2` | v2.1.0 | https://github.com/oklog/ulid |

## MPL-2.0

| Module | Version | Project URL |
|---|---|---|
| `github.com/hashicorp/golang-lru/v2` | v2.0.7 | https://github.com/hashicorp/golang-lru |

`golang-lru` is used as an indirect dependency. MPL-2.0 file-level
copyleft applies to modifications of MPL-licensed files only;
linking the unmodified library into the `api-log` binary does not
extend MPL terms to the rest of the distribution.

---

## Provenance

The list above is sourced from `go.mod` (direct + indirect) and
`go.sum` for the tagged release. Modules listed in `go.sum` but
not pulled into the final build (test-only, tool-only) are
omitted. Adopters who need a machine-readable SBOM can run
`go mod download -json` or `cyclonedx-gomod` against the tagged
source tree.

If a license attribution is wrong or missing, file an issue at
https://github.com/2nd1st/api-log/issues and we will correct the
record.
