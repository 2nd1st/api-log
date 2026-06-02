# Governance

This is a one-page note so adopters know what to expect from this
repository before they invest time on a PR.

## Maintainer

api-log has a single maintainer (`@2nd1st`). It is a hobby project
plus a working tool for my own LLM-gateway recording needs — not a
backed product, not a company deliverable. There is no SLA, no
guaranteed response time, and no roadmap-voting process.

## Decision-making

- The maintainer's call is final on scope, design, and what ships.
- Pull requests are welcome but acceptance is not guaranteed. Issues
  that include a concrete adopter use case have a much higher chance
  of becoming a feature; "wouldn't it be nice" requests do not.
- For anything beyond a small fix, **open an issue with the use case
  first**. A 200-line PR that gets closed is more friction than a
  five-line issue that gets a "yes, send it" reply. This is the
  gate-before-feature stance the project runs on.

## License + forking

- MIT license, no Contributor License Agreement.
- Forking is fine and explicitly OK — if a feature is out of scope
  here, your fork is the right home. The project's
  [§"What we will not do"][wnwd] section is honest about hard limits
  (no gateway features, no SDK instrumentation, no semantic
  interpretation of captured content, no credential-value matching);
  if you need any of those, a fork or a downstream tool is the
  expected path, not a PR against this repo.

## Code of conduct

Contributors are expected to follow the project's
[CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md) (Contributor Covenant
v2.1). Report issues by opening a GitHub issue and mentioning the
maintainer.

## Security

Security issues should follow the process in [SECURITY.md](../SECURITY.md).
Do not file public issues for vulnerabilities.

[wnwd]: ../ROADMAP.md#what-we-will-not-do
