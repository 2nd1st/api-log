package storage

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// setMtime backdates a file to the given time so age-based eviction
// has something predictable to work with. Fails the test on error.
func setMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("setMtime %s: %v", path, err)
	}
}

// evictTestCoordinator builds a coordinator wired with a temp dir
// and a fake store, set up for eviction tests with short timing
// constants (no idle lag) so tests don't have to wait minutes.
func evictTestCoordinator(t *testing.T) (*Coordinator, *fakeStore, string) {
	t.Helper()
	tmp := t.TempDir()
	s := &fakeStore{}
	c := &Coordinator{
		dataDir:         tmp,
		store:           s,
		evictionCap:     1000,
		idleEvictionLag: 0, // no grace window in eviction tests
	}
	return c, s, tmp
}

func TestRunEviction_NoFilesIsNoop(t *testing.T) {
	c, _, _ := evictTestCoordinator(t)
	inv := &Inventory{}
	ev := c.runEviction(context.Background(), inv, RetentionConfig{MaxAgeDays: 30})
	if ev.ran || ev.bytes != 0 || ev.capHit {
		t.Errorf("empty inv: ev = %+v, want zero", ev)
	}
}

func TestRunEviction_AgeCap(t *testing.T) {
	c, store, tmp := evictTestCoordinator(t)

	oldPath := filepath.Join(tmp, "2026-05-01", "a1b2c3d4.jsonl")
	mkFile(t, oldPath, make([]byte, 100))
	setMtime(t, oldPath, time.Now().Add(-40*24*time.Hour))

	newPath := filepath.Join(tmp, "2026-06-01", "e5f6a7b8.jsonl")
	mkFile(t, newPath, make([]byte, 50))
	setMtime(t, newPath, time.Now().Add(-5*24*time.Hour))

	store.paths = []string{oldPath, newPath}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxAgeDays: 30})

	if !ev.ran {
		t.Fatal("ev.ran should be true (one file evicted)")
	}
	if ev.bytes != 100 {
		t.Errorf("ev.bytes = %d, want 100 (only old file)", ev.bytes)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old file should be removed from disk")
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new file should remain: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != oldPath {
		t.Errorf("store.deleted = %v, want [%q]", store.deleted, oldPath)
	}
	// Inventory mutation: old file removed
	if len(inv.Files) != 1 || inv.Files[0].FileID.Date != "2026-06-01" {
		t.Errorf("inv.Files post-eviction = %+v, want only the new file", inv.Files)
	}
	if inv.TotalBytes != 50 {
		t.Errorf("inv.TotalBytes = %d, want 50", inv.TotalBytes)
	}
}

func TestRunEviction_ByteCap(t *testing.T) {
	c, store, tmp := evictTestCoordinator(t)

	// Three files: 1000, 2000, 3000 bytes; oldest first by mtime.
	files := []struct {
		path string
		size int
		age  time.Duration
	}{
		{filepath.Join(tmp, "2026-06-01", "a0000001.jsonl"), 3000, 30 * time.Minute},
		{filepath.Join(tmp, "2026-06-01", "a0000002.jsonl"), 2000, 20 * time.Minute},
		{filepath.Join(tmp, "2026-06-01", "a0000003.jsonl"), 1000, 10 * time.Minute},
	}
	for _, f := range files {
		mkFile(t, f.path, make([]byte, f.size))
		setMtime(t, f.path, time.Now().Add(-f.age))
		store.paths = append(store.paths, f.path)
	}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	if inv.TotalBytes != 6000 {
		t.Fatalf("inv.TotalBytes = %d, want 6000", inv.TotalBytes)
	}

	// Cap at 4000 — should evict oldest (3000) leaving 3000 = 2000+1000
	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxBytes: 4000})
	if ev.bytes != 3000 {
		t.Errorf("ev.bytes = %d, want 3000 (just the oldest file)", ev.bytes)
	}
	if inv.TotalBytes != 3000 {
		t.Errorf("inv.TotalBytes post = %d, want 3000", inv.TotalBytes)
	}
	if len(inv.Files) != 2 {
		t.Errorf("inv.Files len = %d, want 2", len(inv.Files))
	}
}

func TestRunEviction_AgeAndByteBothApply(t *testing.T) {
	// Three files: ancient (age-eligible), middle, recent.
	// Byte cap is set so even after age-deletion, byte cap still over;
	// runEviction should keep deleting until under byte cap.
	c, store, tmp := evictTestCoordinator(t)

	mkFile(t, filepath.Join(tmp, "2026-05-01", "a0000001.jsonl"), make([]byte, 1000))
	setMtime(t, filepath.Join(tmp, "2026-05-01", "a0000001.jsonl"), time.Now().Add(-40*24*time.Hour))

	mkFile(t, filepath.Join(tmp, "2026-05-20", "a0000002.jsonl"), make([]byte, 1000))
	setMtime(t, filepath.Join(tmp, "2026-05-20", "a0000002.jsonl"), time.Now().Add(-15*24*time.Hour))

	mkFile(t, filepath.Join(tmp, "2026-06-01", "a0000003.jsonl"), make([]byte, 500))
	setMtime(t, filepath.Join(tmp, "2026-06-01", "a0000003.jsonl"), time.Now().Add(-1*24*time.Hour))

	store.paths = []string{
		filepath.Join(tmp, "2026-05-01", "a0000001.jsonl"),
		filepath.Join(tmp, "2026-05-20", "a0000002.jsonl"),
		filepath.Join(tmp, "2026-06-01", "a0000003.jsonl"),
	}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	// max_age=30 days → only ancient is age-eligible (1000 bytes).
	// Initial bytes = 2500. After deleting ancient = 1500.
	// max_bytes=1000 → still over by 500 → next-oldest also eligible
	// (1500 - 1000 = 500 to free; second file gives 1000).
	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxAgeDays: 30, MaxBytes: 1000})

	if ev.bytes != 2000 {
		t.Errorf("ev.bytes = %d, want 2000 (ancient 1000 + middle 1000)", ev.bytes)
	}
	if inv.TotalBytes != 500 {
		t.Errorf("inv.TotalBytes post = %d, want 500", inv.TotalBytes)
	}
}

func TestRunEviction_IdleLagPreventsEviction(t *testing.T) {
	// File is old enough by age, but within idleEvictionLag — should
	// be skipped this tick.
	c, store, tmp := evictTestCoordinator(t)
	c.idleEvictionLag = 10 * time.Minute

	path := filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl")
	mkFile(t, path, make([]byte, 100))
	// mtime is "now" — within idle lag
	setMtime(t, path, time.Now())
	store.paths = []string{path}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	// Even with MaxAgeDays=0 (i.e. "delete instantly") byte cap forces
	// eviction — but idle lag should override.
	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxBytes: 50})

	if ev.ran {
		t.Errorf("idle-lag file should not be evicted; got ev=%+v", ev)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should remain: %v", err)
	}
}

func TestRunEviction_CapStopsAtN(t *testing.T) {
	c, store, tmp := evictTestCoordinator(t)
	c.evictionCap = 3

	// 10 ancient files, all eligible by age
	for i := 0; i < 10; i++ {
		path := filepath.Join(tmp, "2026-05-01", toHex8(i)+".jsonl")
		mkFile(t, path, make([]byte, 100))
		setMtime(t, path, time.Now().Add(-time.Duration(40+i)*24*time.Hour))
		store.paths = append(store.paths, path)
	}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxAgeDays: 30})

	if !ev.capHit {
		t.Error("ev.capHit should be true (10 eligible, cap=3)")
	}
	if ev.bytes != 300 {
		t.Errorf("ev.bytes = %d, want 300 (3 × 100)", ev.bytes)
	}
	if len(store.deleted) != 3 {
		t.Errorf("store.deleted = %d, want 3", len(store.deleted))
	}
}

func TestRunEviction_LeasedFileSkipped(t *testing.T) {
	c, store, tmp := evictTestCoordinator(t)

	leasedPath := filepath.Join(tmp, "2026-05-01", "a1b2c3d4.jsonl")
	mkFile(t, leasedPath, make([]byte, 100))
	setMtime(t, leasedPath, time.Now().Add(-40*24*time.Hour))
	store.paths = []string{leasedPath}

	fid, err := FileIDFromPath(tmp, leasedPath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxAgeDays: 30})

	if ev.ran {
		t.Errorf("leased file should not be evicted; got ev=%+v", ev)
	}
	if _, err := os.Stat(leasedPath); err != nil {
		t.Errorf("leased file should remain: %v", err)
	}
}

func TestRunEviction_BothFormsRemoved(t *testing.T) {
	// File during rotation window has both .jsonl and .jsonl.gz.
	// deleteIfIdle should remove both.
	c, store, tmp := evictTestCoordinator(t)

	plainPath := filepath.Join(tmp, "2026-05-01", "a1b2c3d4.jsonl")
	gzPath := plainPath + ".gz"
	mkFile(t, plainPath, make([]byte, 100))
	mkFile(t, gzPath, make([]byte, 30))
	setMtime(t, plainPath, time.Now().Add(-40*24*time.Hour))
	setMtime(t, gzPath, time.Now().Add(-40*24*time.Hour))
	store.paths = []string{plainPath}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxAgeDays: 30})

	if !ev.ran {
		t.Fatal("should have evicted")
	}
	if _, err := os.Stat(plainPath); !os.IsNotExist(err) {
		t.Error(".jsonl should be removed")
	}
	if _, err := os.Stat(gzPath); !os.IsNotExist(err) {
		t.Error(".jsonl.gz should be removed")
	}
}

func TestRunEviction_DeletesMediaSubtree(t *testing.T) {
	c, store, tmp := evictTestCoordinator(t)

	jsonlPath := filepath.Join(tmp, "2026-05-01", "a1b2c3d4.jsonl")
	mkFile(t, jsonlPath, make([]byte, 100))
	setMtime(t, jsonlPath, time.Now().Add(-40*24*time.Hour))
	mediaFile := filepath.Join(tmp, "2026-05-01", "a1b2c3d4", "media", "trace1", "0.png")
	mkFile(t, mediaFile, make([]byte, 500))
	store.paths = []string{jsonlPath}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxAgeDays: 30})

	if ev.bytes != 600 {
		t.Errorf("ev.bytes = %d, want 600 (100 jsonl + 500 media)", ev.bytes)
	}
	if _, err := os.Stat(mediaFile); !os.IsNotExist(err) {
		t.Errorf("media file should be removed; got %v", err)
	}
	mediaDir := filepath.Join(tmp, "2026-05-01", "a1b2c3d4")
	if _, err := os.Stat(mediaDir); !os.IsNotExist(err) {
		t.Errorf("media subtree should be removed; got %v", err)
	}
}

func TestRunEviction_OrderIsMtimeAscending(t *testing.T) {
	// Three files with predictable ages; eviction should delete in
	// oldest-first order. With cap=1, only the oldest is removed.
	c, store, tmp := evictTestCoordinator(t)
	c.evictionCap = 1

	type fileSpec struct {
		path string
		age  time.Duration
	}
	files := []fileSpec{
		{filepath.Join(tmp, "2026-05-01", "aaaaaaa1.jsonl"), 50 * 24 * time.Hour},
		{filepath.Join(tmp, "2026-05-15", "bbbbbbb2.jsonl"), 40 * 24 * time.Hour},
		{filepath.Join(tmp, "2026-05-30", "ccccccc3.jsonl"), 35 * 24 * time.Hour},
	}
	for _, f := range files {
		mkFile(t, f.path, make([]byte, 100))
		setMtime(t, f.path, time.Now().Add(-f.age))
		store.paths = append(store.paths, f.path)
	}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxAgeDays: 30})

	if !ev.capHit {
		t.Error("capHit should be true (3 eligible, cap=1)")
	}
	if len(store.deleted) != 1 {
		t.Fatalf("store.deleted = %v, want 1", store.deleted)
	}
	if store.deleted[0] != files[0].path {
		t.Errorf("deleted = %q, want oldest %q", store.deleted[0], files[0].path)
	}
}

func TestRunEviction_ContextCancelMidLoop(t *testing.T) {
	c, store, tmp := evictTestCoordinator(t)
	for i := 0; i < 5; i++ {
		path := filepath.Join(tmp, "2026-05-01", toHex8(i)+".jsonl")
		mkFile(t, path, make([]byte, 100))
		setMtime(t, path, time.Now().Add(-40*24*time.Hour))
		store.paths = append(store.paths, path)
	}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	ev := c.runEviction(ctx, &inv, RetentionConfig{MaxAgeDays: 30})

	if ev.ran {
		t.Errorf("ctx-canceled eviction should not have run; got %+v", ev)
	}
}

func TestDeleteIfIdle_RefusesLeasedPath(t *testing.T) {
	c, store, tmp := evictTestCoordinator(t)

	path := filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl")
	mkFile(t, path, make([]byte, 100))
	store.paths = []string{path}

	fid, _ := FileIDFromPath(tmp, path)
	lease, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	deleted, err := c.deleteIfIdle(context.Background(), fid)
	if err != nil {
		t.Fatalf("deleteIfIdle returned error: %v", err)
	}
	if deleted {
		t.Error("deleteIfIdle should return false when leased")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should not have been deleted: %v", err)
	}
}

func TestDeleteIfIdle_HandlesAlreadyGoneFile(t *testing.T) {
	// File doesn't exist on disk but row exists in SQLite. deleteIfIdle
	// should successfully delete the row even though the file is gone.
	c, store, tmp := evictTestCoordinator(t)
	path := filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl")
	store.paths = []string{path}
	fid, _ := FileIDFromPath(tmp, path)

	deleted, err := c.deleteIfIdle(context.Background(), fid)
	if err != nil {
		t.Fatalf("deleteIfIdle on missing file: %v", err)
	}
	if !deleted {
		t.Error("deleteIfIdle should succeed with already-gone file")
	}
	if len(store.deleted) != 1 {
		t.Errorf("store.deleted = %d, want 1 (row should still be deleted)", len(store.deleted))
	}
}

func TestDeleteIfIdle_AllowsNilStore(t *testing.T) {
	// Some unit tests use zero-value Coordinator without a store.
	// deleteIfIdle should succeed (file-only deletion).
	tmp := t.TempDir()
	c := &Coordinator{dataDir: tmp}
	path := filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl")
	mkFile(t, path, make([]byte, 100))

	fid, _ := FileIDFromPath(tmp, path)
	deleted, err := c.deleteIfIdle(context.Background(), fid)
	if err != nil {
		t.Fatalf("deleteIfIdle with nil store: %v", err)
	}
	if !deleted {
		t.Error("deleteIfIdle should succeed even with nil store")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should be removed")
	}
}

func TestRunEviction_DeterministicOrder(t *testing.T) {
	// Multiple files with the SAME mtime — secondary sort by canonical
	// path keeps eviction order predictable.
	c, store, tmp := evictTestCoordinator(t)
	c.evictionCap = 2

	sameTime := time.Now().Add(-40 * 24 * time.Hour)
	paths := []string{
		filepath.Join(tmp, "2026-05-01", "ccccccc3.jsonl"),
		filepath.Join(tmp, "2026-05-01", "aaaaaaa1.jsonl"),
		filepath.Join(tmp, "2026-05-01", "bbbbbbb2.jsonl"),
	}
	for _, p := range paths {
		mkFile(t, p, make([]byte, 100))
		setMtime(t, p, sameTime)
		store.paths = append(store.paths, p)
	}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}

	ev := c.runEviction(context.Background(), &inv, RetentionConfig{MaxAgeDays: 30})
	if !ev.capHit {
		t.Error("capHit should be true")
	}

	// Deterministic order means deleted = [aaaa..., bbbb...] when sorted
	got := append([]string{}, store.deleted...)
	sort.Strings(got)
	wantSorted := []string{paths[1], paths[2]}
	sort.Strings(wantSorted)
	if got[0] != wantSorted[0] || got[1] != wantSorted[1] {
		t.Errorf("deterministic delete order failed: got %v, want %v", got, wantSorted)
	}
}
