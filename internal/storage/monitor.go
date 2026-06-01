package storage

import (
	"context"
	"log/slog"
	"time"
)

// Store is the narrow interface monitor + eviction need from the
// trace database. Declared here (not imported from
// internal/store/sqlite) to keep this package free of database
// dependencies — the sqlite Store satisfies it via duck typing.
//
// B2 wires the real implementation; tests pass a fake.
type Store interface {
	// ListDistinctJSONLPaths returns every distinct jsonl_path
	// stored in the traces table, in unspecified order. Backed by
	// the idx_jsonl_path index added by B2's schema migration.
	ListDistinctJSONLPaths(ctx context.Context) ([]string, error)

	// DeleteByJSONLPath removes all trace rows referencing the
	// given canonical path. Returns affected row count.
	DeleteByJSONLPath(ctx context.Context, jsonlPath string) (int64, error)
}

// Start launches the monitor goroutine. Runs an immediate tick so
// Status() returns real data within milliseconds of Start (not at
// the end of the first ticker interval). Then ticks every
// tickInterval until ctx is canceled.
//
// Returns ctx.Err() when canceled — main.go's graceful-shutdown
// sequence waits on this return.
//
// Call exactly once per Coordinator. v0.1.1 doesn't enforce this
// with a mutex/atomic; B1.7's New constructor + the wire-up in
// cmd/api-log/main.go owns the single-call contract.
func (c *Coordinator) Start(ctx context.Context) error {
	c.runTick(ctx)

	interval := c.tickInterval
	if interval <= 0 {
		interval = time.Hour // documented default
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.runTick(ctx)
		}
	}
}

// runTick is the per-interval body. Sequence per roadmap §5:
//
//  1. Walk data dir → inventory
//  2. Reconcile orphan SQLite rows (defense in depth against
//     crash gaps where JSONL got deleted but the row stayed)
//  3. If eviction mode active, run eviction (MUTATES inv)
//  4. Recompute status from POST-eviction inv
//  5. Atomic publish status
//
// The post-eviction recompute is load-bearing per v5.1 review: an
// earlier draft published pre-eviction stats which went stale
// until the next tick.
//
// runTick is package-private but unexported-method-on-public-type;
// tests in the same package can call it directly without spinning
// a goroutine.
func (c *Coordinator) runTick(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}

	// 1. Inventory
	inv, err := walkAndStat(ctx, c.dataDir)
	if err != nil {
		slog.Warn("storage monitor: walk failed", "err", err)
		return
	}
	if ctx.Err() != nil {
		return
	}

	// 2. Reconcile orphans — runs even when retention is disabled
	// (orphans can result from crashes regardless of policy).
	c.reconcileOrphans(ctx)
	if ctx.Err() != nil {
		return
	}

	// 3. Eviction (no-op stub until B1.6 fills runEviction)
	cfg := c.retention.Load()
	var ev evictResult
	if cfg != nil && (cfg.MaxBytes > 0 || cfg.MaxAgeDays > 0) {
		ev = c.runEviction(ctx, &inv, *cfg)
	}

	// 4. Recompute status from POST-eviction inv
	status := computeStatus(inv, cfg)
	status.EngineRunning = true

	if ev.ran {
		status.LastEvictionTs = time.Now().UTC()
		status.LastEvictedBytes = ev.bytes
		status.EvictionCapHit = ev.capHit
	} else if prev := c.status.Load(); prev != nil {
		// Carry over previous eviction stats — don't blank them
		// just because the current tick didn't fire.
		status.LastEvictionTs = prev.LastEvictionTs
		status.LastEvictedBytes = prev.LastEvictedBytes
		// EvictionCapHit reflects only the CURRENT tick; don't
		// carry false alarms.
		status.EvictionCapHit = false
	}

	// 5. Publish — status + cached inventory together so callers
	// reading both see the same post-eviction state.
	c.setInventory(inv.Files)
	c.status.Store(&status)
}

// reconcileOrphans is defensive cleanup for crash-gap inconsistency
// between SQLite rows and on-disk JSONL files.
//
// Source of orphans:
//   - Crash mid-deleteIfIdle (after JSONL os.Remove, before SQLite
//     DELETE). Next tick finds the row pointing at a missing file.
//   - Manual filesystem rm by an operator (rare but possible).
//
// Race protection (the v5.1 fix): enumerate distinct jsonl_paths
// from SQLite, then for EACH path re-stat the filesystem AND check
// the lease state. We do NOT trust a stale inventory snapshot — by
// the time we'd act on inv, a writer could have just created the
// file with a freshly-inserted row.
//
// Per-tick reconciliation cap (same value as eviction cap) bounds
// work in pathological "thousands of orphans on first run" scenarios.
//
// nil store (zero-value Coordinator in unit tests that exercise
// other paths) is a no-op rather than a panic.
func (c *Coordinator) reconcileOrphans(ctx context.Context) {
	if c.store == nil {
		return
	}

	paths, err := c.store.ListDistinctJSONLPaths(ctx)
	if err != nil {
		slog.Warn("storage monitor: list paths for reconcile failed", "err", err)
		return
	}

	cap_ := c.evictionCap
	if cap_ <= 0 {
		cap_ = 1000 // documented default
	}

	deleted := 0
	skipped := 0
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return
		}
		if deleted >= cap_ {
			slog.Warn("storage monitor: reconcile cap hit; remaining orphans deferred to next tick",
				"cap", cap_,
				"deleted_so_far", deleted,
				"total_paths_scanned", deleted+skipped+1)
			return
		}

		fid, err := FileIDFromPath(c.dataDir, p)
		if err != nil {
			// Row's jsonl_path doesn't match our shape — operator
			// intervention needed; don't auto-delete unknown rows.
			skipped++
			continue
		}

		// Re-stat NOW (not from inv). Protects against the
		// "writer created file between walk and reconcile" race.
		fsPath, err := fid.FSExists()
		if err != nil {
			// Permission / I/O error — can't decide; safe-skip.
			skipped++
			continue
		}
		if fsPath != "" {
			// File exists; not an orphan.
			skipped++
			continue
		}

		// File missing. Check lease — could be a writer mid-create
		// who AcquireLease'd but hasn't OpenFile'd yet.
		if c.isLeased(p) {
			skipped++
			continue
		}

		// Truly orphan. Delete row.
		if _, err := c.store.DeleteByJSONLPath(ctx, p); err != nil {
			slog.Warn("storage monitor: orphan row delete failed",
				"path", p, "err", err)
			skipped++
			continue
		}
		deleted++
	}

	if deleted > 0 {
		slog.Info("storage monitor: reconciled orphan rows",
			"deleted", deleted, "scanned", deleted+skipped)
	}
}
