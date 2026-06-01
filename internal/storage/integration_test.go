package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Integration tests for the storage package. Each test exercises the
// full Start → tick → (eviction + reconciliation) → Status loop with
// a real filesystem and a fake Store, validating that the pieces
// composed together behave as the v5.2.2 spec describes.
//
// Unit tests for individual primitives live in lease_test.go,
// inventory_test.go, status_test.go, monitor_test.go, eviction_test.go,
// and coordinator_test.go. This file targets only cross-component
// behavior; do not duplicate primitive coverage here.

// integrationFixture builds a Coordinator from the real New()
// constructor with test-friendly intervals + a fake store but does
// NOT yet start the monitor goroutine. Callers populate disk + store
// state, then call start() to launch the monitor. The returned
// shutdown closes the goroutine + waits for it to exit cleanly.
type integrationCtx struct {
	c        *Coordinator
	store    *fakeStore
	tmp      string
	start    func()
	shutdown func()
}

func newIntegrationFixture(t *testing.T) *integrationCtx {
	t.Helper()
	tmp := t.TempDir()
	store := &fakeStore{}
	c, err := New(Config{
		DataDir:            tmp,
		TickInterval:       20 * time.Millisecond,
		EvictionCapPerTick: 1000,
		IdleEvictionLag:    time.Nanosecond, // bypass grace window for tests
	}, store, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	started := false

	start := func() {
		if started {
			return
		}
		started = true
		go func() { done <- c.Start(ctx) }()
	}
	shutdown := func() {
		cancel()
		if !started {
			return
		}
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) && err != nil {
				t.Errorf("Start returned %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Start did not return after cancel")
		}
	}
	return &integrationCtx{c: c, store: store, tmp: tmp, start: start, shutdown: shutdown}
}

// waitFor polls until pred returns true or the deadline elapses.
// Returns true on success, false on timeout.
func waitFor(t *testing.T, timeout time.Duration, pred func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return pred()
}

func TestIntegration_RetentionTriggersEviction(t *testing.T) {
	f := newIntegrationFixture(t)
	defer f.shutdown()

	// Three files, each 1MB; ascending mtime so eviction selects the
	// oldest. Total = 3MB; cap = 1.5MB → expect oldest 2 deleted.
	now := time.Now()
	for i, kh := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		p := filepath.Join(f.tmp, "2026-05-30", kh+".jsonl")
		mkFile(t, p, make([]byte, 1_000_000))
		setMTime(t, p, now.Add(-time.Duration(3-i)*time.Hour))
		f.store.paths = append(f.store.paths, p)
	}
	// Pre-configure retention before goroutine starts — no Update
	// race with the first tick.
	if err := f.c.UpdateConfig(RetentionConfig{MaxBytes: 1_500_000}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	f.start()

	// Wait for eviction to have actually fired (LastEvictedBytes is
	// the ground truth — DataDirBytes can transiently match the pred
	// during pre-eviction Status snapshots).
	if !waitFor(t, 2*time.Second, func() bool {
		return f.c.Status().LastEvictedBytes > 0
	}) {
		t.Fatalf("eviction did not run within 2s: Status = %+v", f.c.Status())
	}

	s := f.c.Status()
	if s.DataDirBytes > 1_500_000 {
		t.Errorf("post-eviction DataDirBytes = %d, want <= 1.5MB", s.DataDirBytes)
	}
	if s.State != "ok" && s.State != "warning" {
		t.Errorf("post-eviction State = %q, want ok/warning", s.State)
	}
	if f.store.deleteCount() < 2 {
		t.Errorf("expected at least 2 SQLite row deletes; got %d", f.store.deleteCount())
	}
	// Oldest (aaaaaaaa) must be the first deleted.
	want := filepath.Join(f.tmp, "2026-05-30", "aaaaaaaa.jsonl")
	if got := f.store.deletedAt(0); got != want {
		t.Errorf("first delete = %q, want oldest %q", got, want)
	}
}

func TestIntegration_LeaseBlocksEvictionUntilRelease(t *testing.T) {
	f := newIntegrationFixture(t)
	defer f.shutdown()

	p := filepath.Join(f.tmp, "2026-05-30", "deadbeef.jsonl")
	mkFile(t, p, make([]byte, 1_000_000))
	setMTime(t, p, time.Now().Add(-2*time.Hour))
	f.store.paths = []string{p}

	fid, err := FileIDFromPath(f.tmp, p)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.c.AcquireLease(fid)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.UpdateConfig(RetentionConfig{MaxBytes: 1}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	f.start()

	// Wait for a tick to happen (status published) AND eviction to
	// have been attempted. The leased file must still be present.
	if !waitFor(t, time.Second, func() bool {
		return f.c.Status().EngineRunning && f.c.Status().DataDirBytes == 1_000_000
	}) {
		t.Fatalf("initial tick didn't publish leased-file state: Status = %+v", f.c.Status())
	}
	// Give a few extra ticks the chance to (incorrectly) evict.
	time.Sleep(120 * time.Millisecond)
	if _, err := fid.FSExists(); err != nil {
		t.Fatalf("file lookup error mid-test: %v", err)
	}
	if got := f.c.Status().DataDirBytes; got != 1_000_000 {
		t.Errorf("leased file should NOT be evicted; DataDirBytes = %d", got)
	}
	if f.store.deleteCount() != 0 {
		t.Errorf("no SQLite deletes expected while leased; got %d", f.store.deleteCount())
	}

	lease.Release()

	if !waitFor(t, 2*time.Second, func() bool {
		return f.c.Status().LastEvictedBytes > 0
	}) {
		t.Fatalf("post-release eviction did not run: Status = %+v", f.c.Status())
	}
	if f.store.deleteCount() != 1 || f.store.deletedAt(0) != p {
		t.Errorf("expected one delete of %q; got count=%d first=%q",
			p, f.store.deleteCount(), f.store.deletedAt(0))
	}
}

func TestIntegration_OrphanReconciliationDeletesMissingRows(t *testing.T) {
	f := newIntegrationFixture(t)
	defer f.shutdown()

	// Two store rows; one path on disk, one orphan.
	live := filepath.Join(f.tmp, "2026-05-30", "11111111.jsonl")
	mkFile(t, live, []byte("trace\n"))
	orphan := filepath.Join(f.tmp, "2026-05-30", "22222222.jsonl")
	f.store.paths = []string{live, orphan}

	f.start()

	if !waitFor(t, 2*time.Second, func() bool {
		return f.store.deleteCount() == 1
	}) {
		t.Fatalf("orphan row not reconciled within 2s: count=%d first=%q",
			f.store.deleteCount(), f.store.deletedAt(0))
	}
	if got := f.store.deletedAt(0); got != orphan {
		t.Errorf("reconciled %q, want orphan %q", got, orphan)
	}
}

func TestIntegration_UpdateConfigLiveSwap(t *testing.T) {
	f := newIntegrationFixture(t)
	defer f.shutdown()

	// 600K of data; no retention yet.
	p := filepath.Join(f.tmp, "2026-05-30", "33333333.jsonl")
	mkFile(t, p, make([]byte, 600_000))

	f.start()

	if !waitFor(t, time.Second, func() bool { return f.c.Status().DataDirBytes == 600_000 }) {
		t.Fatalf("initial inventory not picked up: %+v", f.c.Status())
	}
	if got := f.c.Status().State; got != "disabled" {
		t.Errorf("pre-update State = %q, want disabled", got)
	}

	// Live-swap retention. Status should immediately reflect new
	// MaxBytes via UpdateConfig's synchronous recompute — without
	// waiting for the next tick.
	if err := f.c.UpdateConfig(RetentionConfig{MaxBytes: 500_000}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	s := f.c.Status()
	if s.MaxBytes != 500_000 {
		t.Errorf("post-update MaxBytes = %d, want 500_000 (synchronous recompute)", s.MaxBytes)
	}
	if s.State != "critical" {
		t.Errorf("post-update State = %q, want critical (600K > 500K)", s.State)
	}
}

func TestIntegration_CountersReceiveEvictionTotals(t *testing.T) {
	tmp := t.TempDir()
	store := &fakeStore{}
	fc := &fakeCounters{}
	c, err := New(Config{
		DataDir:            tmp,
		TickInterval:       20 * time.Millisecond,
		EvictionCapPerTick: 1000,
		IdleEvictionLag:    time.Nanosecond,
	}, store, fc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	p := filepath.Join(tmp, "2026-05-30", "44444444.jsonl")
	mkFile(t, p, make([]byte, 800_000))
	setMTime(t, p, time.Now().Add(-time.Hour))
	store.paths = []string{p}
	if err := c.UpdateConfig(RetentionConfig{MaxBytes: 1}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Start did not return after cancel")
		}
	}()

	if !waitFor(t, 2*time.Second, func() bool {
		return fc.traces.Load() >= 1 && fc.bytes.Load() >= 800_000
	}) {
		t.Fatalf("counters not bumped: traces=%d bytes=%d", fc.traces.Load(), fc.bytes.Load())
	}
}

func TestIntegration_AcquireLeaseDuringDeletionFails(t *testing.T) {
	// Direct markDeleting → AcquireLease ErrFileBeingDeleted. Doesn't
	// need Start; the lease primitive is what's under test, but in
	// the integration context where eviction would call markDeleting.
	f := newIntegrationFixture(t)
	defer f.shutdown()

	fid := FileID{DataDir: f.tmp, Date: "2026-05-30", KeyHash8: "55555555"}
	if !f.c.markDeleting(fid) {
		t.Fatal("markDeleting should succeed on unleased path")
	}
	defer f.c.unmarkDeleting(fid)

	if _, err := f.c.AcquireLease(fid); !errors.Is(err, ErrFileBeingDeleted) {
		t.Errorf("AcquireLease err = %v, want ErrFileBeingDeleted", err)
	}
}

// setMTime overrides a file's mtime so eviction's age check can be
// exercised without sleeping. Helper kept local to this file to avoid
// polluting other test files' helper namespace.
func setMTime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}
