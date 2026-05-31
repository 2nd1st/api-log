# deploy/

Three reference Docker Compose stacks, scoped to different deployment shapes.
Pick the one that matches what you want to do today.

| dir | what it is | when to use |
|---|---|---|
| [`dev-stack/`](./dev-stack/) | api-log built from source + the `tools/mockup` Go mock LLM gateway, wired together on one network | the 5-minute "try it" path — no real upstream, no clone-a-sibling-repo, no real keys. The integration test in `tests/integration/run.sh` drives this stack. |
| [`demo/`](./demo/) | api-log in front of `sub2api` (a real gateway) | realistic real-upstream demo. `sub2api` itself is not vendored — clone it as a sibling directory (`../sub2api`) per `demo/docker-compose.yml`'s comments, or swap the build context for whatever upstream you already run. |
| [`bench/`](./bench/) | api-log alone, upstream URL supplied via `APILOG_PROXY_UPSTREAM` env | bench / load-test harness. Driven by `tests/bench/run.sh`; uses bind-mounted `./data/` so the orchestrator can scrape `/healthz`, JSONL, and `admin_token` from the host without exec'ing into the (distroless) container. |

All three publish the proxy on `:7861` and the read API on `:7862`, and write
data into a bind-mounted `./data/` directory inside the stack folder. The
auto-generated admin bearer for the read API lands at `./data/admin_token` on
first run.

If none of these match your topology, the canonical install is six lines of
YAML — see the "Deploy concept" section of the top-level [README](../README.md).
