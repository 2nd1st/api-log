package storage

import (
	"testing"
	"time"
)

func TestComputeStatus_NilConfig(t *testing.T) {
	inv := Inventory{TotalBytes: 1_000_000}
	s := computeStatus(inv, nil)
	if s.State != "disabled" {
		t.Errorf("nil cfg State = %q, want %q", s.State, "disabled")
	}
	if s.DataDirBytes != 1_000_000 {
		t.Errorf("DataDirBytes = %d, want 1_000_000", s.DataDirBytes)
	}
	if s.MaxBytes != 0 || s.MaxAgeDays != 0 || s.UsagePct != 0 {
		t.Errorf("nil cfg should leave cap fields zero; got %+v", s)
	}
}

func TestComputeStatus_BothKnobsZero(t *testing.T) {
	inv := Inventory{TotalBytes: 5_000_000_000}
	cfg := &RetentionConfig{}
	s := computeStatus(inv, cfg)
	if s.State != "disabled" {
		t.Errorf("both knobs 0: State = %q, want %q", s.State, "disabled")
	}
	if s.DataDirBytes != 5_000_000_000 {
		t.Errorf("DataDirBytes = %d, want 5_000_000_000", s.DataDirBytes)
	}
}

func TestComputeStatus_OnlyAgeKnob(t *testing.T) {
	// max_bytes == 0 + max_age_days > 0 — eviction will fire on age
	// but there's no byte signal to drive warning/critical state.
	// Should be "ok".
	inv := Inventory{TotalBytes: 5_000_000_000}
	cfg := &RetentionConfig{MaxAgeDays: 30}
	s := computeStatus(inv, cfg)
	if s.State != "ok" {
		t.Errorf("age-only cfg: State = %q, want %q", s.State, "ok")
	}
	if s.MaxAgeDays != 30 {
		t.Errorf("MaxAgeDays = %d, want 30", s.MaxAgeDays)
	}
	if s.UsagePct != 0 {
		t.Errorf("UsagePct = %d, want 0 (no byte cap)", s.UsagePct)
	}
}

func TestComputeStatus_OkBelowWarn(t *testing.T) {
	inv := Inventory{TotalBytes: 10_000_000_000} // 10 GB
	cfg := &RetentionConfig{
		MaxBytes:      100_000_000_000, // 100 GB
		WarnAtPercent: 80,
	}
	s := computeStatus(inv, cfg)
	if s.State != "ok" {
		t.Errorf("10%% usage: State = %q, want %q", s.State, "ok")
	}
	if s.UsagePct != 10 {
		t.Errorf("UsagePct = %d, want 10", s.UsagePct)
	}
}

func TestComputeStatus_WarningAtThreshold(t *testing.T) {
	// Exactly at warn_at_percent — the comparison is >=, so this
	// is "warning" not "ok".
	inv := Inventory{TotalBytes: 80_000_000_000}
	cfg := &RetentionConfig{
		MaxBytes:      100_000_000_000,
		WarnAtPercent: 80,
	}
	s := computeStatus(inv, cfg)
	if s.State != "warning" {
		t.Errorf("80%% usage at threshold: State = %q, want %q", s.State, "warning")
	}
	if s.UsagePct != 80 {
		t.Errorf("UsagePct = %d, want 80", s.UsagePct)
	}
}

func TestComputeStatus_CriticalAtFullCap(t *testing.T) {
	// At exactly 100% — comparison is >=, so this is "critical".
	inv := Inventory{TotalBytes: 100_000_000_000}
	cfg := &RetentionConfig{
		MaxBytes:      100_000_000_000,
		WarnAtPercent: 80,
	}
	s := computeStatus(inv, cfg)
	if s.State != "critical" {
		t.Errorf("100%% usage: State = %q, want %q", s.State, "critical")
	}
	if s.UsagePct != 100 {
		t.Errorf("UsagePct = %d, want 100", s.UsagePct)
	}
}

func TestComputeStatus_CriticalOverCap(t *testing.T) {
	// Usage can exceed 100% between writer bursts and the next tick.
	// Adopters need to see that; we don't clamp.
	inv := Inventory{TotalBytes: 120_000_000_000}
	cfg := &RetentionConfig{
		MaxBytes:      100_000_000_000,
		WarnAtPercent: 80,
	}
	s := computeStatus(inv, cfg)
	if s.State != "critical" {
		t.Errorf("120%% usage: State = %q, want %q", s.State, "critical")
	}
	if s.UsagePct != 120 {
		t.Errorf("UsagePct = %d, want 120 (no clamping)", s.UsagePct)
	}
}

func TestComputeStatus_WarnDefaultsTo80(t *testing.T) {
	// WarnAtPercent zero in cfg means "use default 80" inside
	// computeStatus (defense in depth — coordinator.go validates
	// Config too, but computeStatus shouldn't depend on that).
	inv := Inventory{TotalBytes: 85_000_000_000}
	cfg := &RetentionConfig{
		MaxBytes:      100_000_000_000,
		WarnAtPercent: 0, // intentionally unset
	}
	s := computeStatus(inv, cfg)
	if s.State != "warning" {
		t.Errorf("85%% usage with default warn=80: State = %q, want %q", s.State, "warning")
	}
}

func TestComputeStatus_WarnAt90(t *testing.T) {
	inv := Inventory{TotalBytes: 85_000_000_000}
	cfg := &RetentionConfig{
		MaxBytes:      100_000_000_000,
		WarnAtPercent: 90, // higher than 85% usage → should be "ok"
	}
	s := computeStatus(inv, cfg)
	if s.State != "ok" {
		t.Errorf("85%% usage with warn=90: State = %q, want %q", s.State, "ok")
	}
}

func TestComputeStatus_EmptyInventory(t *testing.T) {
	cfg := &RetentionConfig{MaxBytes: 100_000_000_000, WarnAtPercent: 80}
	s := computeStatus(Inventory{}, cfg)
	if s.State != "ok" {
		t.Errorf("empty inv: State = %q, want %q", s.State, "ok")
	}
	if s.UsagePct != 0 || s.DataDirBytes != 0 {
		t.Errorf("empty inv: UsagePct=%d DataDirBytes=%d, want 0/0", s.UsagePct, s.DataDirBytes)
	}
}

func TestCoordinatorStatus_PreTickReturnsPending(t *testing.T) {
	c := &Coordinator{}
	s := c.Status()
	if s.State != "pending" {
		t.Errorf("zero-value Coordinator: Status().State = %q, want %q", s.State, "pending")
	}
}

func TestCoordinatorStatus_LoadStoredValue(t *testing.T) {
	c := &Coordinator{}
	stored := Status{
		DataDirBytes:     12_345_678,
		MaxBytes:         100_000_000_000,
		UsagePct:         12,
		State:            "ok",
		LastEvictionTs:   time.Now().UTC().Add(-2 * time.Hour),
		LastEvictedBytes: 1_000_000,
		EngineRunning:    true,
	}
	c.status.Store(&stored)
	got := c.Status()
	if got != stored {
		t.Errorf("Status() = %+v, want %+v", got, stored)
	}
}

func TestCoordinatorStatus_OverwriteIsAtomic(t *testing.T) {
	// Two sequential Stores; Status() always returns the latest.
	c := &Coordinator{}
	a := Status{State: "ok", DataDirBytes: 1}
	b := Status{State: "warning", DataDirBytes: 2}
	c.status.Store(&a)
	if got := c.Status(); got.State != "ok" || got.DataDirBytes != 1 {
		t.Errorf("after store a: got %+v, want %+v", got, a)
	}
	c.status.Store(&b)
	if got := c.Status(); got.State != "warning" || got.DataDirBytes != 2 {
		t.Errorf("after store b: got %+v, want %+v", got, b)
	}
}

func TestCoordinatorStatus_DefensiveCopyOnLoad(t *testing.T) {
	// Caller modifying the returned Status MUST NOT affect the
	// stored value. Status is a value type, so the language gives
	// this to us — but verify in case someone changes Status to
	// contain a slice/map field in the future.
	c := &Coordinator{}
	stored := Status{DataDirBytes: 100, State: "ok"}
	c.status.Store(&stored)
	got := c.Status()
	got.DataDirBytes = 999
	got.State = "critical"
	again := c.Status()
	if again.DataDirBytes != 100 || again.State != "ok" {
		t.Errorf("caller mutation leaked into stored Status: %+v", again)
	}
}
