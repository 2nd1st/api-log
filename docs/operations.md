# Operations

Day-2 topics for `api-log` operators: WAL checkpoints, backups,
retention cross-reference, and what multi-instance does and does not
do. Keep this short — most of the behavior is in ARCHITECTURE.md;
this file is the operator-facing summary.

## WAL checkpoint policy

`index.sqlite` runs in WAL mode
(ARCHITECTURE §4). SQLite's auto-checkpoint runs in-process whenever
the WAL crosses ~1000 pages (the SQLite default); the operator does
not normally need to intervene. Concurrent readers (the read API,
plus any external `sqlite3` session) coexist with the single writer
goroutine without explicit locking on the operator's part.

The one case where manual action helps: the `-wal` sidecar grows
larger than the operator is comfortable with — typically because a
long-running reader held an open snapshot across many writer commits,
so the in-process auto-checkpoint could not advance. Force a
checkpoint:

```bash
sqlite3 <data_dir>/index.sqlite "PRAGMA wal_checkpoint(TRUNCATE);"
```

`TRUNCATE` waits for all readers to drain, applies the WAL, and
truncates the `-wal` file back to zero bytes. Safe to run while
`api-log` is up; the writer goroutine will simply see a fresh WAL
on its next append. If readers are persistently active and the
truncate cannot complete, the command returns a non-zero status —
nothing is corrupted, retry once readers release.

Auto-checkpoint covers the steady state. The manual one-liner is
documented because operators ask; it is not part of the routine
runbook.

## Backup

Two pieces, in this priority order.

### 1. JSONL tree — the source of truth

```bash
tar czf api-log-jsonl-$(date -u +%Y%m%d).tar.gz \
  -C <data_dir> \
  --exclude=index.sqlite \
  --exclude='index.sqlite-*' \
  --exclude=tmp \
  --exclude=viewer-cache \
  --exclude=admin_token \
  --exclude=runtime_overrides.json \
  .
```

JSONL files are append-only and rotated daily; the active day's file
(`<date>/<keyhash8>.jsonl`) is the only one being written to. `tar`
reading it concurrently sees a consistent prefix — any line in
progress is bounded by the writer's single `Write` syscall per line
(ARCHITECTURE §7.1 step 8). A partial-line tail is not possible
because the writer buffers the whole line before issuing the write.

### 2. SQLite index — derived cache

Use SQLite's online backup, not a raw file copy:

```bash
sqlite3 <data_dir>/index.sqlite \
  ".backup '<backup_dir>/index.sqlite'"
```

`.backup` is WAL-safe; a raw `cp` of `index.sqlite` without the
`-wal` / `-shm` sidecars produces a torn copy.

### Restore order

JSONL first, SQLite second. If SQLite is missing on restore, the
binary's startup pass rebuilds it from JSONL — slower but correct
(see ARCHITECTURE §1 invariants). A future `api-log rebuild`
subcommand (ROADMAP "Day-2 operations") will surface this as an
explicit operator command rather than the implicit
startup-on-empty-data-dir behavior.

The JSONL tree is the only artifact a long-term backup strategy
needs. SQLite is a convenience; treat it as cache.

## Retention

Retention is documented separately — see [docs/retention.md](./retention.md).

The retention loop's `data_dir_bytes` accounting covers JSONL files
(`<date>/<keyhash8>.jsonl[.gz]`) and the extracted media tree
(`<date>/<keyhash8>/media/<trace_id>/`). It does **not** count:

- `index.sqlite` and its `-wal` / `-shm` siblings
- `tmp/` capture-time scratch
- `admin_token`
- `runtime_overrides.json`
- `viewer-cache/` (hosted viewer dist)

Operators sizing a disk budget should add headroom for those
out-of-band files. SQLite + WAL is typically a low single-digit
percentage of JSONL bytes at steady state, but media-heavy traffic
can move that ratio.

## Multi-instance

`api-log` is **single-node**. Two binaries cannot write to the same
`<data_dir>` at the same time. Two specific failure modes:

- **ULID collisions on trace IDs.** Each process generates ULIDs
  from its own entropy source; two binaries writing to the same
  `<date>/<keyhash8>.jsonl` can — extremely rarely — produce the
  same ID, and definitely produce out-of-order suffixes within a
  millisecond. SQLite's `PRIMARY KEY (id)` rejects the collision on
  whichever writer loses the race; that trace is dropped with
  `drop_sqlite_fail`.
- **SQLite write contention.** Both processes hold separate
  connection pools; the WAL serializes writes but `busy_timeout`
  (5 s default) can fire under sustained contention, dropping the
  losing trace at the writer-channel boundary.

Run one binary per `<data_dir>`. To horizontally scale capture (one
gateway in front of multiple LLM upstreams, one `api-log` per
upstream, etc.), give each instance its own data directory and run
a separate viewer pointing at each, or aggregate downstream from
the JSONL trees.

Clustering, shared-storage replication, and active-passive
fail-over are not on the roadmap. The project is shaped for one
listener per LLM gateway; horizontal scale is solved by deploying
more independent instances, not by clustering one.
