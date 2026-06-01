package storage

import "time"

// Status is the publishable snapshot of storage state. The monitor
// goroutine atomically stores a Status after each tick; readers
// (healthz, Landing OPS strip, Settings storage card) atomically
// load it. The struct is value-only (no methods, no internal
// references) so it can be JSON-marshaled directly.
//
// All sizes are "retention-managed bytes" — the JSONL + media
// footprint that walkAndStat returns in Inventory.TotalBytes.
// SQLite, tmp/, admin_token, runtime_overrides.json are explicitly
// EXCLUDED. Adopters who want raw disk usage run `du -sb data/`
// separately; documented in retention.md.
type Status struct {
	// DataDirBytes is the current retention-managed footprint
	// (sum of JSONL + media across all (date, keyhash) buckets).
	DataDirBytes int64

	// MaxBytes mirrors the active RetentionConfig.MaxBytes for ops
	// visibility; 0 when byte cap is disabled.
	MaxBytes int64

	// MaxAgeDays mirrors the active RetentionConfig.MaxAgeDays; 0
	// when age cap is disabled.
	MaxAgeDays int

	// UsagePct = 100 * DataDirBytes / MaxBytes, clamped to [0, 100+].
	// 0 when MaxBytes == 0 (no byte cap, so the percentage is
	// meaningless). The value can exceed 100 briefly between writer
	// activity and the next eviction tick.
	UsagePct int

	// State summarizes the storage situation in one word so the
	// Landing OPS strip can color-code without doing arithmetic:
	//
	//   "disabled" — both knobs are 0; monitor still runs but no
	//                eviction can fire.
	//   "ok"       — under thresholds.
	//   "warning"  — usage at or above warn_at_percent of max_bytes.
	//   "critical" — usage at or above 100% of max_bytes.
	//   "pending"  — monitor hasn't ticked yet; Status() returns
	//                this when atomic.Pointer is still nil at first
	//                load.
	State string

	// LastEvictionTs is the wall-clock time of the most recent
	// eviction tick that actually deleted something. Zero value
	// (time.Time{}) means "no eviction has run yet OR no files
	// were eligible at last tick".
	LastEvictionTs time.Time

	// LastEvictedBytes is the byte total deleted by the most recent
	// eviction tick. Zero when LastEvictionTs is zero.
	LastEvictedBytes int64

	// EvictionCapHit is true when the most recent tick exhausted the
	// per-tick eviction cap (default 1000 files) with more eligible
	// files remaining. Operator-facing signal that another tick is
	// needed to fully catch up — useful during the first-enable
	// migration.
	EvictionCapHit bool

	// NextEvictionEst is a best-effort projection of when the byte
	// cap would be hit at the current growth rate. Always zero in
	// v0.1.1; v0.2 may populate it from inventory deltas.
	NextEvictionEst time.Time

	// EngineRunning is true when monitor.Start has been called and
	// the goroutine is alive. Distinguishes "monitor hasn't started
	// yet" (e.g. unit test instantiation without Start) from
	// "monitor is up but hasn't ticked yet".
	EngineRunning bool
}

// RetentionConfig is the dynamic subset of retention settings —
// operators mutate this via PUT /api/config/retention without a
// restart. The Coordinator holds it in an atomic.Pointer so the
// monitor's tick loop sees the latest value with no lock.
//
// Static settings (data_dir, tick_interval, eviction_cap_per_tick,
// idle_eviction_lag) live on the immutable Config in coordinator.go
// (B1.7) — they require restart to change.
type RetentionConfig struct {
	MaxBytes      int64
	MaxAgeDays    int
	WarnAtPercent int
}

// computeStatus derives the size-related fields of Status from an
// Inventory and the current RetentionConfig. Pure: callers (monitor
// tick) layer in the eviction-related fields (LastEvictionTs,
// LastEvictedBytes, EvictionCapHit) and EngineRunning before
// publishing.
//
// cfg may be nil — that's the "no retention configured" path; the
// resulting Status has DataDirBytes set from the inventory but
// MaxBytes/MaxAgeDays/UsagePct all zero and State == "disabled".
func computeStatus(inv Inventory, cfg *RetentionConfig) Status {
	s := Status{DataDirBytes: inv.TotalBytes}

	if cfg == nil {
		s.State = "disabled"
		return s
	}

	s.MaxBytes = cfg.MaxBytes
	s.MaxAgeDays = cfg.MaxAgeDays

	// Both knobs zero — engine runs (for inventory + status) but no
	// eviction will fire. Surface this explicitly so adopters see
	// "engine up, no policy".
	if cfg.MaxBytes <= 0 && cfg.MaxAgeDays <= 0 {
		s.State = "disabled"
		return s
	}

	// UsagePct only makes sense when there's a byte cap.
	warn := cfg.WarnAtPercent
	if warn <= 0 {
		warn = 80 // documented default; coordinator.go validates Config but defends here too
	}
	if cfg.MaxBytes > 0 {
		// Integer percentage; cap math is 100 * bytes / max. We do
		// NOT clamp to 100 — usage can transiently exceed 100% if
		// writer outpaces the hourly tick, and adopters need to see
		// that. The "critical" threshold below uses >= 100.
		s.UsagePct = int((inv.TotalBytes * 100) / cfg.MaxBytes)
	}

	switch {
	case cfg.MaxBytes > 0 && s.UsagePct >= 100:
		s.State = "critical"
	case cfg.MaxBytes > 0 && s.UsagePct >= warn:
		s.State = "warning"
	default:
		s.State = "ok"
	}
	return s
}

// Status returns the most recently published status, or a "pending"
// snapshot when the monitor hasn't ticked yet (or hasn't been
// started). Always safe to call; never blocks; never returns nil
// because Status is a value type.
//
// This is THE single source of truth for /healthz, Landing OPS,
// Settings storage card, and the startup banner. Adding a sibling
// "what's our current size?" computation anywhere else in the code
// would be the patchwork v5 was meant to prevent — route through
// this method.
func (c *Coordinator) Status() Status {
	if s := c.status.Load(); s != nil {
		return *s
	}
	return Status{State: "pending"}
}
