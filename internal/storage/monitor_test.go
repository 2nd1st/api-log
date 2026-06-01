package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore is a minimal Store implementation for monitor tests.
// Tracks the paths it would delete; lets tests assert which orphans
// got removed.
type fakeStore struct {
	mu          sync.Mutex
	paths       []string
	deleted     []string
	listErr     error
	deleteErr   error
	listCallN   atomic.Int32
	deleteCalls atomic.Int32
}

func (s *fakeStore) ListDistinctJSONLPaths(_ context.Context) ([]string, error) {
	s.listCallN.Add(1)
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	out := make([]string, len(s.paths))
	copy(out, s.paths)
	s.mu.Unlock()
	return out, nil
}

func (s *fakeStore) DeleteByJSONLPath(_ context.Context, path string) (int64, error) {
	s.deleteCalls.Add(1)
	if s.deleteErr != nil {
		return 0, s.deleteErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Remove the deleted path from the live list so subsequent List
	// calls reflect reality.
	out := s.paths[:0]
	for _, p := range s.paths {
		if p != path {
			out = append(out, p)
		}
	}
	s.paths = out
	s.deleted = append(s.deleted, path)
	return 1, nil
}

// deleteCount returns the number of paths that have been deleted via
// DeleteByJSONLPath. Safe for concurrent use — integration tests
// invoke this while the monitor goroutine writes to s.deleted.
func (s *fakeStore) deleteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deleted)
}

// deletedAt returns the i-th deleted path or "" if out of range. Safe
// for concurrent use.
func (s *fakeStore) deletedAt(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.deleted) {
		return ""
	}
	return s.deleted[i]
}

// newTestCoordinator builds a Coordinator wired with a temp dataDir
// and a fake store. Caller can override fields before calling
// runTick / Start.
func newTestCoordinator(t *testing.T) (*Coordinator, *fakeStore, string) {
	t.Helper()
	tmp := t.TempDir()
	s := &fakeStore{}
	c := &Coordinator{
		dataDir:      tmp,
		store:        s,
		evictionCap:  1000,
		tickInterval: 50 * time.Millisecond, // short for tests
	}
	return c, s, tmp
}

func TestRunTick_PublishesStatusFromInventory(t *testing.T) {
	c, _, tmp := newTestCoordinator(t)
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), make([]byte, 12345))

	c.runTick(context.Background())

	s := c.Status()
	if s.DataDirBytes != 12345 {
		t.Errorf("DataDirBytes = %d, want 12345", s.DataDirBytes)
	}
	if !s.EngineRunning {
		t.Error("EngineRunning should be true after runTick")
	}
	if s.State != "disabled" {
		t.Errorf("State = %q, want %q (no retention configured)", s.State, "disabled")
	}
}

func TestRunTick_StatusReflectsActiveRetention(t *testing.T) {
	c, _, tmp := newTestCoordinator(t)
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), make([]byte, 85_000_000))

	cfg := &RetentionConfig{MaxBytes: 100_000_000, WarnAtPercent: 80}
	c.retention.Store(cfg)

	c.runTick(context.Background())

	s := c.Status()
	if s.UsagePct != 85 {
		t.Errorf("UsagePct = %d, want 85", s.UsagePct)
	}
	if s.State != "warning" {
		t.Errorf("State = %q, want %q", s.State, "warning")
	}
	if s.MaxBytes != 100_000_000 {
		t.Errorf("MaxBytes = %d, want 100_000_000", s.MaxBytes)
	}
}

func TestRunTick_PreservesPreviousEvictionStats(t *testing.T) {
	// When the current tick doesn't evict, status should carry over
	// the previous tick's LastEvictionTs / LastEvictedBytes rather
	// than blanking them.
	c, _, _ := newTestCoordinator(t)
	prev := Status{
		State:            "ok",
		LastEvictionTs:   time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		LastEvictedBytes: 999,
	}
	c.status.Store(&prev)

	c.runTick(context.Background())

	s := c.Status()
	if !s.LastEvictionTs.Equal(prev.LastEvictionTs) {
		t.Errorf("LastEvictionTs = %v, want %v (carried from prev)", s.LastEvictionTs, prev.LastEvictionTs)
	}
	if s.LastEvictedBytes != 999 {
		t.Errorf("LastEvictedBytes = %d, want 999 (carried from prev)", s.LastEvictedBytes)
	}
	if s.EvictionCapHit {
		t.Error("EvictionCapHit should reset to false on a non-eviction tick")
	}
}

func TestReconcileOrphans_DeletesMissingPathRows(t *testing.T) {
	c, store, tmp := newTestCoordinator(t)
	// Two rows in SQLite, one with an existing file, one orphan.
	live := filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl")
	mkFile(t, live, []byte("trace\n"))
	orphan := filepath.Join(tmp, "2026-06-01", "e5f6a7b8.jsonl")
	store.paths = []string{live, orphan}

	c.reconcileOrphans(context.Background())

	if len(store.deleted) != 1 {
		t.Fatalf("expected exactly 1 delete; got %d (%v)", len(store.deleted), store.deleted)
	}
	if store.deleted[0] != orphan {
		t.Errorf("deleted = %q, want %q", store.deleted[0], orphan)
	}
}

func TestReconcileOrphans_SkipsLeasedPaths(t *testing.T) {
	// A path that's currently held by a lease — a writer mid-create
	// — must not be reconciled even if its file doesn't yet exist
	// on disk.
	c, store, tmp := newTestCoordinator(t)
	orphanLooking := filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl")
	store.paths = []string{orphanLooking}

	fid, err := FileIDFromPath(tmp, orphanLooking)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	c.reconcileOrphans(context.Background())

	if len(store.deleted) != 0 {
		t.Errorf("expected 0 deletes (leased path); got %d (%v)", len(store.deleted), store.deleted)
	}
}

func TestReconcileOrphans_RespectsCap(t *testing.T) {
	c, store, tmp := newTestCoordinator(t)
	c.evictionCap = 3

	// 10 orphan rows; cap should stop at 3.
	for i := 0; i < 10; i++ {
		store.paths = append(store.paths, filepath.Join(tmp, "2026-06-01", toHex8(i)+".jsonl"))
	}

	c.reconcileOrphans(context.Background())

	if len(store.deleted) != 3 {
		t.Errorf("expected exactly 3 deletes (cap=3); got %d", len(store.deleted))
	}
}

func TestReconcileOrphans_SkipsMalformedPaths(t *testing.T) {
	c, store, tmp := newTestCoordinator(t)
	store.paths = []string{
		"/totally/not/our/shape.jsonl",
		filepath.Join(tmp, "bad-date", "a1b2c3d4.jsonl"),
		filepath.Join(tmp, "2026-06-01", "BADHASH.jsonl"),
	}

	c.reconcileOrphans(context.Background())

	if len(store.deleted) != 0 {
		t.Errorf("expected 0 deletes (all malformed); got %d (%v)", len(store.deleted), store.deleted)
	}
}

func TestReconcileOrphans_NilStoreIsNoop(t *testing.T) {
	// Zero-value Coordinator (used by lease-only tests) has no
	// store; reconcileOrphans must not panic.
	c := &Coordinator{}
	c.reconcileOrphans(context.Background()) // expect no panic
}

func TestReconcileOrphans_ListErrorIsLoggedNotFatal(t *testing.T) {
	c, store, _ := newTestCoordinator(t)
	store.listErr = errors.New("simulated db error")
	c.reconcileOrphans(context.Background())
	if len(store.deleted) != 0 {
		t.Errorf("with list error, no deletes should happen; got %d", len(store.deleted))
	}
}

func TestReconcileOrphans_ContextCancel(t *testing.T) {
	c, store, tmp := newTestCoordinator(t)
	for i := 0; i < 100; i++ {
		store.paths = append(store.paths, filepath.Join(tmp, "2026-06-01", toHex8(i)+".jsonl"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	c.reconcileOrphans(ctx)
	// First iteration sees ctx.Err and returns; should not delete anything.
	if len(store.deleted) != 0 {
		t.Errorf("expected 0 deletes (ctx canceled); got %d", len(store.deleted))
	}
}

func TestStart_RunsInitialTickThenLoops(t *testing.T) {
	c, _, tmp := newTestCoordinator(t)
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), make([]byte, 100))
	c.tickInterval = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Start(ctx)
	}()

	// Wait for initial tick to publish status
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if c.Status().DataDirBytes == 100 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.Status().DataDirBytes != 100 {
		t.Fatalf("initial tick didn't publish: Status() = %+v", c.Status())
	}

	// Cancel; Start should return ctx.Err()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func TestStart_TicksOnInterval(t *testing.T) {
	// Verify Start triggers re-ticks by adding files between ticks
	// and observing Status updates.
	c, _, tmp := newTestCoordinator(t)
	c.tickInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	// Initial tick has nothing
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.Status().EngineRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.Status().DataDirBytes != 0 {
		t.Fatalf("initial tick should see empty dataDir; got %d", c.Status().DataDirBytes)
	}

	// Add a file; next tick should pick it up
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), make([]byte, 200))
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if c.Status().DataDirBytes == 200 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subsequent tick didn't pick up new file: Status() = %+v", c.Status())
}

func TestRunTick_HandlesWalkError(t *testing.T) {
	c := &Coordinator{
		dataDir:      "/this/does/not/exist",
		tickInterval: 50 * time.Millisecond,
	}
	c.runTick(context.Background()) // expect: log warning, status stays nil
	if s := c.status.Load(); s != nil {
		t.Errorf("walk error should not publish a status; got %+v", *s)
	}
	// Public Status() returns "pending" since nothing was stored.
	if got := c.Status().State; got != "pending" {
		t.Errorf("Status().State = %q, want %q", got, "pending")
	}
}

// toHex8 makes an 8-character lowercase hex string for test
// keyhashes. Reuses the same shape as ids.KeyHashShort.
func toHex8(n int) string {
	const hex = "0123456789abcdef"
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = hex[n&0xf]
		n >>= 4
	}
	// Fill any leading bytes that are still zero with '0'.
	for i := range b {
		if b[i] == 0 {
			b[i] = '0'
		}
	}
	return string(b[:])
}

func TestUnusedDataDirComputedFromHelper(t *testing.T) {
	// Sanity check toHex8 produces well-formed keyhashes that
	// FileIDFromPath accepts.
	for _, n := range []int{0, 1, 255, 1234567} {
		s := toHex8(n)
		if !keyHashRe.MatchString(s) {
			t.Errorf("toHex8(%d) = %q does not match keyHashRe", n, s)
		}
	}
}

// Sanity: the os.Stat helper used by FSExists distinguishes ENOENT
// from real errors. We don't unit-test fs permissions (CI doesn't
// reliably grant arbitrary chmod) but the helper has a logical
// branch we exercise indirectly via the missing-file path above.
func TestFSExists_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	fid := FileID{DataDir: tmp, Date: "2026-06-01", KeyHash8: "a1b2c3d4"}
	if _, err := os.Stat(fid.CanonicalPath()); err == nil {
		t.Fatal("test setup: file should not exist")
	}
	path, err := fid.FSExists()
	if err != nil {
		t.Errorf("FSExists on missing file: err=%v, want nil", err)
	}
	if path != "" {
		t.Errorf("FSExists on missing file: path=%q, want \"\"", path)
	}
}
