# Storage retention

`api-log` ships a built-in retention loop (v0.1.1). Off by default —
the monitor goroutine runs to maintain inventory + status, but
nothing gets deleted until you set a cap.

## Two knobs

| Knob | What it means | Default |
|---|---|---|
| `max_bytes` | Hard cap on the retention-managed footprint. Oldest `(date, keyhash)` buckets evicted first until under cap. `0` = no byte cap. | `0` (disabled) |
| `max_age_days` | Buckets whose mtime is older than `N` days are evicted. `0` = no age cap. | `0` (disabled) |
| `warn_at_percent` | `/healthz.storage.state` flips to `"warning"` at this %. Cosmetic — does not trigger eviction. | `80` |

Both `0` is the "engine running, no policy" mode — `/healthz.storage`
still reports usage; eviction never fires.

## What counts toward `data_dir_bytes`

Only files the writer (re)produces:

- `<DataDir>/<date>/<keyhash8>.jsonl[.gz]` — the source of truth
- `<DataDir>/<date>/<keyhash8>/media/<trace_id>/<idx>.<ext>` — extracted
  attachments

What's **excluded**:

- `index.sqlite` and its `-wal` / `-shm` siblings — rebuildable from
  JSONL, not in scope for retention
- `tmp/` — capture-time scratch, cleaned by the capture path itself
- `admin_token`, `runtime_overrides.json` — operator state, never
  evicted
- `viewer-cache/` — hosted-viewer extracted dist; lifecycle owned by
  `viewerhost`

Adopters who want raw disk usage run `du -sb <DataDir>/` and accept
the difference.

## Live-mutating retention

```bash
# Inspect current settings + state
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://localhost:8080/api/config/retention
# {"max_bytes":0,"max_age_days":0,"warn_at_percent":80,"source":"yaml"}

# Enable: cap at 10 GB and 30 days
curl -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"max_bytes":10000000000,"max_age_days":30,"warn_at_percent":75}' \
  http://localhost:8080/api/config/retention
# {"max_bytes":10000000000,"max_age_days":30,"warn_at_percent":75,"source":"override"}

# Inspect live state
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8080/healthz \
  | jq .storage
# {
#   "data_dir_bytes": 8500000000,
#   "max_bytes": 10000000000,
#   "max_age_days": 30,
#   "usage_pct": 85,
#   "state": "warning",
#   "engine_running": true,
#   "eviction_cap_hit": false
# }

# Disable retention (engine keeps reporting usage, never deletes)
curl -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"max_bytes":0,"max_age_days":0}' \
  http://localhost:8080/api/config/retention
```

Persistence: every PUT writes `<DataDir>/runtime_overrides.json`, so
the value survives restart without env / yaml plumbing.

## How eviction actually fires

- Monitor goroutine ticks once per hour by default.
- Each tick: walk the data dir → reconcile orphan SQLite rows → if
  retention is on, classify candidates (`mtime < now - max_age_days`
  OR cumulative bytes > `max_bytes`, oldest-first) → delete up to the
  per-tick cap (default 1000 files).
- A bucket whose writer-side file handle is currently open is
  **skipped** — its lease is held. Writer's idle-close (default 10
  min) releases the lease so retention can pick the bucket up on
  the next tick.
- A bucket mid-deletion blocks new appends to the same path: the
  writer surfaces `ErrFileBeingDeleted` from `AcquireLease`, the
  trace is counted as a JSONL-fail drop, and the next finalize routes
  to a fresh bucket. This is a race the design intentionally accepts
  rather than serializing eviction behind writer activity.

`/healthz.storage.eviction_cap_hit` flips true when a tick exhausted
the per-tick cap with more eligible files remaining — the signal
that another tick is needed (typical only during the first-enable
migration on a large data dir).

## What `/healthz.storage.state` means

| State | Meaning |
|---|---|
| `pending` | Monitor hasn't published its first tick yet. Healthz arrived early. |
| `disabled` | Both knobs are `0`. Engine runs (inventory + status); no eviction. |
| `ok` | Under thresholds. |
| `warning` | At or above `warn_at_percent` of `max_bytes`. |
| `critical` | At or above 100% of `max_bytes` — writer outpaced the hourly tick, or first-enable on an oversized dir. |

`usage_pct` is unclamped on purpose: a brief spike above 100% during
heavy writes is real signal, not an artifact to round down.

## Sizing rule of thumb

A trace JSONL line is dominated by request + response body bytes.
At the operator's traffic profile, the easiest first estimate is:

```
days_of_history ≈ max_bytes / (daily_traffic_bytes)
```

Set `max_bytes` to the disk budget you've allocated to the data dir,
not to a number you wish were possible — eviction at high usage is
correct behavior, not a failure mode.

Caveat: media-heavy traffic (image generation, vision models) skews
the per-trace size dramatically. If you're seeing `usage_pct` climb
unexpectedly fast, check `/healthz.counters.total_media_files` — at
finalize time the extracted attachments count alongside the JSONL.
