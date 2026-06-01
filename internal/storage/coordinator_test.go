package storage

import (
	"sync/atomic"
	"testing"
	"time"
)

// fakeCounters records calls; nil-safe interface check is via
// passing nil to New, exercised in TestNew_NilCountersAccepted.
type fakeCounters struct {
	traces atomic.Int64
	bytes  atomic.Int64
}

func (f *fakeCounters) AddEvictedTraces(n int64) { f.traces.Add(n) }
func (f *fakeCounters) AddEvictedBytes(n int64)  { f.bytes.Add(n) }

// minimalCfg is the smallest Config that passes validation.
func minimalCfg() Config {
	return Config{DataDir: "/tmp/storage-test"}
}

func TestNew_RejectsEmptyDataDir(t *testing.T) {
	if _, err := New(Config{}, nil, nil); err == nil {
		t.Fatal("New with empty DataDir should error")
	}
}

func TestNew_RejectsInvalidWarnAtPercent(t *testing.T) {
	cases := []int{-1, 101, 200}
	for _, p := range cases {
		cfg := minimalCfg()
		cfg.WarnAtPercent = p
		if _, err := New(cfg, nil, nil); err == nil {
			t.Errorf("WarnAtPercent=%d should error", p)
		}
	}
}

func TestNew_RejectsNegativeNumericFields(t *testing.T) {
	t.Run("MaxBytes", func(t *testing.T) {
		cfg := minimalCfg()
		cfg.MaxBytes = -1
		if _, err := New(cfg, nil, nil); err == nil {
			t.Error("MaxBytes=-1 should error")
		}
	})
	t.Run("MaxAgeDays", func(t *testing.T) {
		cfg := minimalCfg()
		cfg.MaxAgeDays = -1
		if _, err := New(cfg, nil, nil); err == nil {
			t.Error("MaxAgeDays=-1 should error")
		}
	})
	t.Run("TickInterval", func(t *testing.T) {
		cfg := minimalCfg()
		cfg.TickInterval = -1 * time.Second
		if _, err := New(cfg, nil, nil); err == nil {
			t.Error("TickInterval=-1s should error")
		}
	})
	t.Run("EvictionCapPerTick", func(t *testing.T) {
		cfg := minimalCfg()
		cfg.EvictionCapPerTick = -1
		if _, err := New(cfg, nil, nil); err == nil {
			t.Error("EvictionCapPerTick=-1 should error")
		}
	})
	t.Run("IdleEvictionLag", func(t *testing.T) {
		cfg := minimalCfg()
		cfg.IdleEvictionLag = -1 * time.Second
		if _, err := New(cfg, nil, nil); err == nil {
			t.Error("IdleEvictionLag=-1s should error")
		}
	})
}

func TestNew_AppliesDefaults(t *testing.T) {
	c, err := New(minimalCfg(), nil, nil)
	if err != nil {
		t.Fatalf("New(minimal) error: %v", err)
	}
	if c.tickInterval != time.Hour {
		t.Errorf("tickInterval = %v, want 1h", c.tickInterval)
	}
	if c.evictionCap != 1000 {
		t.Errorf("evictionCap = %d, want 1000", c.evictionCap)
	}
	if c.idleEvictionLag != 5*time.Minute {
		t.Errorf("idleEvictionLag = %v, want 5m", c.idleEvictionLag)
	}
	// WarnAtPercent default is observable via Status only after a
	// tick (or via the retention pointer if seeded). With no caps
	// configured, retention stays nil — that's the "disabled" path.
	if c.retention.Load() != nil {
		t.Error("retention should be nil when no caps configured")
	}
}

func TestNew_SeedsRetentionWhenCapsSet(t *testing.T) {
	cfg := minimalCfg()
	cfg.MaxBytes = 10_000_000
	cfg.MaxAgeDays = 7
	cfg.WarnAtPercent = 75
	c, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	r := c.retention.Load()
	if r == nil {
		t.Fatal("retention should be seeded when MaxBytes>0 or MaxAgeDays>0")
	}
	if r.MaxBytes != 10_000_000 || r.MaxAgeDays != 7 || r.WarnAtPercent != 75 {
		t.Errorf("retention = %+v", *r)
	}
}

func TestNew_WarnAtPercentDefaultAppliedToRetention(t *testing.T) {
	cfg := minimalCfg()
	cfg.MaxBytes = 1_000_000 // any cap so retention gets seeded
	c, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	r := c.retention.Load()
	if r == nil || r.WarnAtPercent != 80 {
		t.Errorf("WarnAtPercent default not applied to retention: %+v", r)
	}
}

func TestNew_NilCountersAccepted(t *testing.T) {
	c, err := New(minimalCfg(), nil, nil)
	if err != nil {
		t.Fatalf("New(nil counters) error: %v", err)
	}
	if c.counters != nil {
		t.Error("nil counters arg should leave c.counters nil")
	}
}

func TestNew_CountersInterfaceWired(t *testing.T) {
	fc := &fakeCounters{}
	c, err := New(minimalCfg(), nil, fc)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if c.counters == nil {
		t.Fatal("counters should be wired")
	}
	c.counters.AddEvictedTraces(3)
	c.counters.AddEvictedBytes(1024)
	if fc.traces.Load() != 3 || fc.bytes.Load() != 1024 {
		t.Errorf("counters routing broken: traces=%d bytes=%d", fc.traces.Load(), fc.bytes.Load())
	}
}

func TestUpdateConfig_RejectsInvalid(t *testing.T) {
	c, _ := New(minimalCfg(), nil, nil)
	cases := []RetentionConfig{
		{MaxBytes: -1},
		{MaxAgeDays: -1},
		{WarnAtPercent: -1},
		{WarnAtPercent: 101},
	}
	for i, rc := range cases {
		if err := c.UpdateConfig(rc); err == nil {
			t.Errorf("case %d (%+v): expected error", i, rc)
		}
	}
}

func TestUpdateConfig_SwapsRetentionAtomically(t *testing.T) {
	c, _ := New(minimalCfg(), nil, nil)
	if c.retention.Load() != nil {
		t.Fatal("precondition: retention should start nil")
	}
	if err := c.UpdateConfig(RetentionConfig{MaxBytes: 50_000_000, MaxAgeDays: 14}); err != nil {
		t.Fatalf("UpdateConfig error: %v", err)
	}
	r := c.retention.Load()
	if r == nil || r.MaxBytes != 50_000_000 || r.MaxAgeDays != 14 {
		t.Fatalf("retention not swapped: %+v", r)
	}
	// WarnAtPercent default applied when caller passes 0.
	if r.WarnAtPercent != 80 {
		t.Errorf("WarnAtPercent default not applied: %d", r.WarnAtPercent)
	}
}

func TestUpdateConfig_BothKnobsZeroClearsPointer(t *testing.T) {
	cfg := minimalCfg()
	cfg.MaxBytes = 1_000_000
	c, _ := New(cfg, nil, nil)
	if c.retention.Load() == nil {
		t.Fatal("precondition: retention should be seeded")
	}
	if err := c.UpdateConfig(RetentionConfig{}); err != nil {
		t.Fatalf("UpdateConfig({}) error: %v", err)
	}
	if c.retention.Load() != nil {
		t.Error("both-knobs-zero should clear retention pointer")
	}
}

func TestUpdateConfig_RecomputesStatusSynchronously(t *testing.T) {
	c, _ := New(minimalCfg(), nil, nil)
	// Seed a prior Status as if a tick had run, with 800K bytes in
	// the data dir, no caps. State should currently be "disabled".
	seed := Status{
		DataDirBytes:  800_000,
		State:         "disabled",
		EngineRunning: true,
	}
	c.status.Store(&seed)

	// Operator sets a byte cap below current usage. Synchronous
	// recompute should immediately flip State to "critical" and
	// expose UsagePct, MaxBytes — without waiting for the next tick.
	if err := c.UpdateConfig(RetentionConfig{MaxBytes: 500_000}); err != nil {
		t.Fatalf("UpdateConfig error: %v", err)
	}
	s := c.Status()
	if s.MaxBytes != 500_000 {
		t.Errorf("MaxBytes = %d, want 500000", s.MaxBytes)
	}
	if s.State != "critical" {
		t.Errorf("State = %q, want %q (800K > 500K)", s.State, "critical")
	}
	if s.UsagePct < 100 {
		t.Errorf("UsagePct = %d, want >= 100", s.UsagePct)
	}
	if !s.EngineRunning {
		t.Error("EngineRunning should be carried forward")
	}
	if s.DataDirBytes != 800_000 {
		t.Errorf("DataDirBytes should carry forward via prior status, got %d", s.DataDirBytes)
	}
}

func TestUpdateConfig_SynthesizesBaselineStatusPreTick(t *testing.T) {
	c, _ := New(minimalCfg(), nil, nil)
	// No prior tick — Status() returns "pending".
	if s := c.Status(); s.State != "pending" {
		t.Fatalf("precondition: State = %q, want pending", s.State)
	}
	if err := c.UpdateConfig(RetentionConfig{MaxBytes: 1_000_000}); err != nil {
		t.Fatalf("UpdateConfig error: %v", err)
	}
	// Synthesized baseline so PUT-then-GET callers see the new
	// thresholds without waiting for the first monitor tick.
	s := c.Status()
	if s.State == "pending" {
		t.Error("UpdateConfig pre-tick: State stayed pending; should synthesize")
	}
	if s.MaxBytes != 1_000_000 {
		t.Errorf("MaxBytes = %d, want 1_000_000", s.MaxBytes)
	}
	if s.DataDirBytes != 0 {
		t.Errorf("DataDirBytes = %d, want 0 (no inventory yet)", s.DataDirBytes)
	}
	if s.EngineRunning {
		t.Error("EngineRunning should stay false until monitor starts")
	}
	// And retention was updated.
	if r := c.retention.Load(); r == nil || r.MaxBytes != 1_000_000 {
		t.Errorf("retention not swapped: %+v", r)
	}
}

func TestInventory_NilBeforeFirstTick(t *testing.T) {
	c, _ := New(minimalCfg(), nil, nil)
	if got := c.Inventory(); got != nil {
		t.Errorf("Inventory() pre-tick = %v, want nil", got)
	}
}

func TestInventory_ReturnsDefensiveCopy(t *testing.T) {
	c, _ := New(minimalCfg(), nil, nil)
	files := []FileInfo{
		{FileID: FileID{DataDir: "/d", Date: "2026-06-01", KeyHash8: "deadbeef"}, SizeBytes: 100},
		{FileID: FileID{DataDir: "/d", Date: "2026-06-01", KeyHash8: "cafebabe"}, SizeBytes: 200},
	}
	c.setInventory(files)

	copy1 := c.Inventory()
	if len(copy1) != 2 || copy1[0].SizeBytes != 100 {
		t.Fatalf("Inventory() = %+v", copy1)
	}
	// Mutate the returned slice — should NOT affect the next call.
	copy1[0].SizeBytes = 99999
	copy2 := c.Inventory()
	if copy2[0].SizeBytes != 100 {
		t.Errorf("Inventory() leaked internal state: got SizeBytes=%d after caller mutation", copy2[0].SizeBytes)
	}
}

func TestInventory_ReflectsLatestSet(t *testing.T) {
	c, _ := New(minimalCfg(), nil, nil)
	c.setInventory([]FileInfo{{FileID: FileID{DataDir: "/d", Date: "2026-06-01", KeyHash8: "00000000"}}})
	if len(c.Inventory()) != 1 {
		t.Fatal("first set didn't take")
	}
	c.setInventory([]FileInfo{
		{FileID: FileID{DataDir: "/d", Date: "2026-06-01", KeyHash8: "11111111"}},
		{FileID: FileID{DataDir: "/d", Date: "2026-06-01", KeyHash8: "22222222"}},
	})
	if got := c.Inventory(); len(got) != 2 {
		t.Errorf("second set: len=%d, want 2", len(got))
	}
}

func TestStatus_PendingPreFirstTick(t *testing.T) {
	c, _ := New(minimalCfg(), nil, nil)
	s := c.Status()
	if s.State != "pending" {
		t.Errorf("pre-tick State = %q, want pending", s.State)
	}
	if s.EngineRunning {
		t.Error("EngineRunning should be false before first tick")
	}
}
