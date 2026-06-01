package storage

import (
	"errors"
	"fmt"
	"time"
)

// Config is the static configuration for a Coordinator. Set once at
// construction by New(); immutable after Start.
//
// Operator-mutable retention thresholds (max_bytes, max_age_days,
// warn_at_percent) are stored in a SEPARATE atomic.Pointer on the
// Coordinator and updated via UpdateConfig(); they are deliberately
// NOT part of Config so the static / dynamic split is enforced by
// the API shape.
type Config struct {
	// DataDir is the absolute root passed through to walkAndStat
	// and FileID.{CanonicalPath,MediaSubtree,FSExists}. Required.
	DataDir string

	// Initial retention values. After New, callers mutate these via
	// UpdateConfig — the in-memory atomic.Pointer is the live source
	// of truth, not the Config struct.
	MaxBytes      int64
	MaxAgeDays    int
	WarnAtPercent int

	// TickInterval is the monitor cadence. Default 1 hour.
	TickInterval time.Duration

	// EvictionCapPerTick bounds work per tick for both eviction and
	// orphan reconciliation. Default 1000 files.
	EvictionCapPerTick int

	// IdleEvictionLag is the grace period: files modified within
	// this window are NOT evicted. Default 5 minutes. Gives
	// just-rotated or just-released files breathing room.
	IdleEvictionLag time.Duration
}

// Counters is the narrow interface the Coordinator needs from the
// metrics package. Declared here (not imported from internal/counters)
// to keep storage free of cross-package dependencies on the metrics
// surface. The real *counters.Counters value satisfies it via duck
// typing; tests pass nil for a no-op.
//
// All methods are safe to call concurrently and safe to call on a
// nil Counters (the Coordinator nil-checks before each call).
type Counters interface {
	AddEvictedTraces(n int64)
	AddEvictedBytes(n int64)
}

// New constructs a Coordinator with the given static config + store
// backend + (optional) counters. Validates config; applies defaults
// for zero-value fields. Does NOT start the monitor goroutine — the
// caller invokes Start(ctx) when ready.
//
// store may be nil for unit tests that exercise lease semantics
// without a database. counters may be nil; the Coordinator nil-checks.
//
// Returns error on:
//   - empty DataDir
//   - WarnAtPercent outside [0, 100] (0 means "use default 80")
//   - negative duration fields
//   - negative integer fields
func New(cfg Config, store Store, counters Counters) (*Coordinator, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("storage.New: DataDir is required")
	}
	if cfg.WarnAtPercent < 0 || cfg.WarnAtPercent > 100 {
		return nil, fmt.Errorf("storage.New: WarnAtPercent must be in [0, 100], got %d", cfg.WarnAtPercent)
	}
	if cfg.MaxBytes < 0 {
		return nil, fmt.Errorf("storage.New: MaxBytes must be >= 0, got %d", cfg.MaxBytes)
	}
	if cfg.MaxAgeDays < 0 {
		return nil, fmt.Errorf("storage.New: MaxAgeDays must be >= 0, got %d", cfg.MaxAgeDays)
	}
	if cfg.TickInterval < 0 {
		return nil, fmt.Errorf("storage.New: TickInterval must be >= 0, got %v", cfg.TickInterval)
	}
	if cfg.EvictionCapPerTick < 0 {
		return nil, fmt.Errorf("storage.New: EvictionCapPerTick must be >= 0, got %d", cfg.EvictionCapPerTick)
	}
	if cfg.IdleEvictionLag < 0 {
		return nil, fmt.Errorf("storage.New: IdleEvictionLag must be >= 0, got %v", cfg.IdleEvictionLag)
	}

	// Apply defaults for zero-valued fields.
	if cfg.TickInterval == 0 {
		cfg.TickInterval = time.Hour
	}
	if cfg.EvictionCapPerTick == 0 {
		cfg.EvictionCapPerTick = 1000
	}
	if cfg.IdleEvictionLag == 0 {
		cfg.IdleEvictionLag = 5 * time.Minute
	}
	if cfg.WarnAtPercent == 0 {
		cfg.WarnAtPercent = 80
	}

	c := &Coordinator{
		dataDir:         cfg.DataDir,
		tickInterval:    cfg.TickInterval,
		evictionCap:     cfg.EvictionCapPerTick,
		idleEvictionLag: cfg.IdleEvictionLag,
		store:           store,
		counters:        counters,
	}

	// Seed the retention pointer if any cap is set. nil means
	// "retention is fully disabled" — monitor still runs to maintain
	// inventory + status.
	if cfg.MaxBytes > 0 || cfg.MaxAgeDays > 0 {
		c.retention.Store(&RetentionConfig{
			MaxBytes:      cfg.MaxBytes,
			MaxAgeDays:    cfg.MaxAgeDays,
			WarnAtPercent: cfg.WarnAtPercent,
		})
	}

	return c, nil
}

// UpdateConfig atomically swaps the dynamic retention thresholds —
// the subset operators mutate via PUT /api/config/retention without
// restart.
//
// Validates inputs the same way New does, then publishes via
// atomic.Pointer. Recomputes Status SYNCHRONOUSLY against the cached
// inventory so /healthz reflects the new thresholds immediately;
// eviction itself runs at next tick.
//
// Mirrors the save_attachments pattern (api/config.go) — same
// shape: PUT updates immediately, side effects pick it up on the
// next polling cycle.
//
// Passing the zero-value RetentionConfig (all fields 0) disables
// retention without restarting the monitor.
func (c *Coordinator) UpdateConfig(retention RetentionConfig) error {
	if retention.MaxBytes < 0 {
		return fmt.Errorf("UpdateConfig: MaxBytes must be >= 0, got %d", retention.MaxBytes)
	}
	if retention.MaxAgeDays < 0 {
		return fmt.Errorf("UpdateConfig: MaxAgeDays must be >= 0, got %d", retention.MaxAgeDays)
	}
	if retention.WarnAtPercent < 0 || retention.WarnAtPercent > 100 {
		return fmt.Errorf("UpdateConfig: WarnAtPercent must be in [0, 100], got %d", retention.WarnAtPercent)
	}
	if retention.WarnAtPercent == 0 {
		retention.WarnAtPercent = 80
	}

	// Swap retention.
	if retention.MaxBytes == 0 && retention.MaxAgeDays == 0 {
		// Both knobs zero means "disable retention". Clear the
		// pointer so the monitor branch (`if cfg != nil && ...`)
		// skips eviction at next tick.
		c.retention.Store(nil)
	} else {
		c.retention.Store(&retention)
	}

	// Recompute Status synchronously so PUT-then-GET callers see the
	// new thresholds without waiting for the next monitor tick.
	//
	// Two paths:
	//   - Prior status published (normal case): carry forward
	//     inventory + eviction fields, replace the retention-driven
	//     fields via computeStatus.
	//   - No prior status (UpdateConfig fired before the first tick,
	//     typical for fresh deployments where an operator sets
	//     retention immediately): synthesize a baseline status with
	//     DataDirBytes=0. The next tick will replace it with real
	//     inventory; in the meantime callers see their just-set
	//     MaxBytes / MaxAgeDays instead of an opaque "pending".
	cfg := c.retention.Load()
	if prev := c.status.Load(); prev != nil {
		fresh := *prev
		newStatus := computeStatus(Inventory{TotalBytes: fresh.DataDirBytes}, cfg)
		// Carry forward fields computeStatus doesn't touch
		newStatus.LastEvictionTs = fresh.LastEvictionTs
		newStatus.LastEvictedBytes = fresh.LastEvictedBytes
		newStatus.EvictionCapHit = fresh.EvictionCapHit
		newStatus.EngineRunning = fresh.EngineRunning
		newStatus.NextEvictionEst = fresh.NextEvictionEst
		c.status.Store(&newStatus)
	} else {
		// Synthesize a baseline. EngineRunning stays false — the
		// monitor goroutine hasn't started yet; flipping it to true
		// here would lie about the state.
		newStatus := computeStatus(Inventory{}, cfg)
		c.status.Store(&newStatus)
	}
	return nil
}

// Inventory returns a snapshot of the most recent walkAndStat result.
// Used by the Landing OPS strip + Settings storage card for the
// "current files" view (potential v0.2 expansion). For v0.1.1 the
// returned slice is what monitor.runTick last produced; callers should
// not mutate.
//
// Returns nil if the monitor hasn't ticked yet.
func (c *Coordinator) Inventory() []FileInfo {
	c.inventoryMu.RLock()
	defer c.inventoryMu.RUnlock()
	if c.lastInventory == nil {
		return nil
	}
	// Return a defensive copy — callers (especially viewer-facing
	// JSON marshallers) might iterate concurrently while monitor
	// runs next tick.
	out := make([]FileInfo, len(c.lastInventory))
	copy(out, c.lastInventory)
	return out
}

// setInventory is monitor.runTick's hook to refresh the cached
// FileInfo slice. Private — Inventory() is the public read accessor.
func (c *Coordinator) setInventory(files []FileInfo) {
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	// Take ownership of the slice the monitor passes; the caller
	// must not mutate it after handing it off.
	c.lastInventory = files
}
