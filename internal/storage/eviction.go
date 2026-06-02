package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// evictResult is what runEviction returns to runTick for inclusion
// in the published Status.
type evictResult struct {
	ran    bool  // true if eviction actually deleted at least one file
	bytes  int64 // total bytes deleted this tick (JSONL + media)
	capHit bool  // true if per-tick cap was hit with more candidates eligible
}

// runEviction selects files from inv that meet the age or byte cap,
// atomically deletes them, and returns aggregate stats.
//
// Selection rules (oldest-first by ModTime):
//  1. Files modified within idleEvictionLag (default 5m) are
//     EXCLUDED — gives just-rotated / just-released files a
//     breathing window before they become eviction candidates.
//     Files with zero ModTime (media-only orphans) bypass this
//     check (treated as "infinitely old").
//  2. Age eligibility: ModTime < now - MaxAgeDays. Only fires when
//     MaxAgeDays > 0.
//  3. Byte eligibility: while currentBytes > MaxBytes, the oldest
//     remaining file is eligible. Only fires when MaxBytes > 0.
//  4. A file matching EITHER rule gets evicted.
//
// Per-tick cap (default 1000) caps work per tick. capHit=true when
// MORE eligible files remain after the cap is reached — this is the
// signal that another tick is needed (e.g. first-enable migration).
//
// MUTATES inv: removes evicted FileInfos from inv.Files and
// subtracts deleted bytes from inv.TotalBytes. The caller's
// computeStatus then reads the updated inventory for an accurate
// post-eviction UsagePct.
//
// Errors during individual file delete are logged but do NOT abort
// the tick — best-effort progress is preferable to halting on one
// bad file. Files that errored remain in inv (size unchanged); next
// tick will retry.
func (c *Coordinator) runEviction(ctx context.Context, inv *Inventory, cfg RetentionConfig) evictResult {
	if inv == nil || len(inv.Files) == 0 {
		return evictResult{}
	}

	lag := c.idleEvictionLag
	if lag <= 0 {
		lag = 5 * time.Minute
	}
	cap_ := c.evictionCap
	if cap_ <= 0 {
		cap_ = 1000
	}

	now := time.Now().UTC()
	var ageCutoff time.Time
	if cfg.MaxAgeDays > 0 {
		ageCutoff = now.AddDate(0, 0, -cfg.MaxAgeDays)
	}

	// Sort by ModTime ASC (oldest first). Stable sort on FileID as
	// secondary key for deterministic test output.
	candidates := make([]*FileInfo, 0, len(inv.Files))
	for i := range inv.Files {
		candidates = append(candidates, &inv.Files[i])
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].ModTime.Equal(candidates[j].ModTime) {
			return candidates[i].ModTime.Before(candidates[j].ModTime)
		}
		// Secondary sort by canonical path for determinism
		return candidates[i].FileID.CanonicalPath() < candidates[j].FileID.CanonicalPath()
	})

	// First pass: classify each candidate. Tracks running totals so
	// byte-cap eligibility can be computed sequentially (delete
	// oldest-first until under cap).
	eligible := make(map[FileID]bool, len(candidates))
	currentBytes := inv.TotalBytes
	for _, fi := range candidates {
		// Idle lag: skip if within grace window. Files with zero
		// ModTime (media-only orphan) bypass the check.
		if !fi.ModTime.IsZero() && now.Sub(fi.ModTime) < lag {
			continue
		}

		ageHit := !ageCutoff.IsZero() && !fi.ModTime.IsZero() && fi.ModTime.Before(ageCutoff)
		// Media-only orphans (zero ModTime + zero SizeBytes + nonzero
		// MediaSize) are eligible regardless of age — they have no
		// real "age" but clearly shouldn't accumulate.
		mediaOrphan := fi.ModTime.IsZero() && fi.SizeBytes == 0 && fi.MediaSize > 0
		byteHit := cfg.MaxBytes > 0 && currentBytes > cfg.MaxBytes

		if ageHit || byteHit || mediaOrphan {
			eligible[fi.FileID] = true
			currentBytes -= fi.SizeBytes + fi.MediaSize
		}
	}

	if len(eligible) == 0 {
		return evictResult{}
	}

	// Second pass: delete in mtime ASC order, respecting per-tick cap.
	deletedFids := make(map[FileID]struct{}, len(eligible))
	deletedBytes := int64(0)
	capHit := false
	for _, fi := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		if !eligible[fi.FileID] {
			continue
		}
		if len(deletedFids) >= cap_ {
			capHit = true
			break
		}
		ok, err := c.deleteIfIdle(ctx, fi.FileID)
		if err != nil {
			slog.Warn("storage eviction: delete failed",
				"date", fi.FileID.Date, "key_hash8", fi.FileID.KeyHash8, "err", err)
			continue
		}
		if !ok {
			// Leased — skip; next tick will retry.
			continue
		}
		deletedFids[fi.FileID] = struct{}{}
		deletedBytes += fi.SizeBytes + fi.MediaSize
	}

	// MUTATE inv: filter out deleted FileInfos + recompute TotalBytes.
	if len(deletedFids) > 0 {
		out := inv.Files[:0]
		for _, fi := range inv.Files {
			if _, deleted := deletedFids[fi.FileID]; !deleted {
				out = append(out, fi)
			}
		}
		inv.Files = out
		inv.TotalBytes -= deletedBytes
		if c.counters != nil {
			c.counters.AddEvictedTraces(int64(len(deletedFids)))
			c.counters.AddEvictedBytes(deletedBytes)
		}
		slog.Info("storage eviction: tick complete",
			"deleted_count", len(deletedFids),
			"deleted_bytes", deletedBytes,
			"cap_hit", capHit)
	}

	return evictResult{
		ran:    len(deletedFids) > 0,
		bytes:  deletedBytes,
		capHit: capHit,
	}
}

// deleteIfIdle is the eviction-time atomic delete primitive.
//
// Acquires an exclusive eviction mark (refused if any lease is held);
// then deletes the on-disk JSONL (both `.jsonl` and `.jsonl.gz`
// forms, if either exists), the media subtree (best-effort, errors
// logged not returned), and the SQLite row.
//
// Returns:
//   - (true, nil) on success
//   - (false, nil) if the path is leased — caller should skip
//   - (false, err) on IO error (stat, remove, or DELETE) — caller
//     logs and continues; orphan rows from partial failures get
//     reconciled by the next monitor tick.
//
// Delete order: JSONL → media → SQLite row. Crash mid-sequence
// leaves a row pointing at a missing file; monitor.reconcileOrphans
// cleans this up on next tick (defense in depth, since the in-memory
// ARCHITECTURE.md "rebuild from JSONL" claim isn't implemented).
//
// `c.store == nil` (zero-value Coordinator in some unit tests) is
// treated as "row delete is a no-op" — the on-disk parts still
// happen. Real deployment always sets store via New().
func (c *Coordinator) deleteIfIdle(ctx context.Context, fid FileID) (bool, error) {
	if !c.markDeleting(fid) {
		return false, nil
	}
	defer c.unmarkDeleting(fid)

	// Sanity-check FSExists for permission/IO errors before touching
	// anything. ErrNotExist (file gone) is fine; other errors abort.
	if _, err := fid.FSExists(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat for delete: %w", err)
	}

	// Remove both `.jsonl` and `.jsonl.gz` forms — handles all three
	// states (plain only, gz only, both during rotation window) in
	// one shot. fs.ErrNotExist is the "already gone" success case.
	plainPath := fid.CanonicalPath()
	gzPath := plainPath + ".gz"
	for _, path := range []string{plainPath, gzPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("remove %s: %w", path, err)
		}
	}

	// Media — remove the per-keyhash directory (which contains
	// exactly the media/ subtree per the on-disk layout), not just
	// the media/ subdir. This keeps the date directory clean as
	// buckets evict. Best-effort: failure here is non-fatal — the
	// JSONL is gone, the row will be gone; an orphan media tree
	// is operationally annoying but not corrupting.
	mediaParent := filepath.Dir(fid.MediaSubtree())
	if err := os.RemoveAll(mediaParent); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("storage eviction: media subtree remove failed; orphan media",
			"path", mediaParent, "err", err)
	}

	// SQLite row deletion. Nil store means "test context with no
	// database" — no-op.
	if c.store != nil {
		if _, err := c.store.DeleteByJSONLPath(ctx, fid.CanonicalPath()); err != nil {
			return false, fmt.Errorf("delete row %s: %w", fid.CanonicalPath(), err)
		}
	}

	return true, nil
}
