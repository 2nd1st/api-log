package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tests build a small data dir tree on disk and verify walkAndStat
// produces the expected Inventory. Each test isolates in t.TempDir()
// so they parallelize cleanly.

func mkFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func findFI(inv Inventory, date, keyHash string) (FileInfo, bool) {
	for _, fi := range inv.Files {
		if fi.FileID.Date == date && fi.FileID.KeyHash8 == keyHash {
			return fi, true
		}
	}
	return FileInfo{}, false
}

func TestWalkAndStat_EmptyDir(t *testing.T) {
	tmp := t.TempDir()
	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatalf("walkAndStat: %v", err)
	}
	if len(inv.Files) != 0 {
		t.Errorf("empty dir: Files len = %d, want 0", len(inv.Files))
	}
	if inv.TotalBytes != 0 {
		t.Errorf("empty dir: TotalBytes = %d, want 0", inv.TotalBytes)
	}
}

func TestWalkAndStat_OnlyJSONL(t *testing.T) {
	tmp := t.TempDir()
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), []byte("hello\nworld\n"))

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Files) != 1 {
		t.Fatalf("Files len = %d, want 1", len(inv.Files))
	}
	fi, ok := findFI(inv, "2026-06-01", "a1b2c3d4")
	if !ok {
		t.Fatal("expected FileInfo for 2026-06-01/a1b2c3d4 not found")
	}
	if fi.SizeBytes != 12 {
		t.Errorf("SizeBytes = %d, want 12", fi.SizeBytes)
	}
	if fi.MediaSize != 0 {
		t.Errorf("MediaSize = %d, want 0", fi.MediaSize)
	}
	if fi.ModTime.IsZero() {
		t.Error("ModTime should be non-zero for an on-disk file")
	}
	if inv.TotalBytes != 12 {
		t.Errorf("TotalBytes = %d, want 12", inv.TotalBytes)
	}
}

func TestWalkAndStat_OnlyGzip(t *testing.T) {
	tmp := t.TempDir()
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl.gz"), []byte("compressed-bytes"))

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	fi, ok := findFI(inv, "2026-06-01", "a1b2c3d4")
	if !ok {
		t.Fatal("FileInfo not found")
	}
	if fi.SizeBytes != 16 {
		t.Errorf("SizeBytes = %d, want 16", fi.SizeBytes)
	}
}

func TestWalkAndStat_BothFormsSumDuringRotation(t *testing.T) {
	// During the rotation window writer.compressInPlace can leave
	// BOTH .jsonl and .jsonl.gz on disk briefly. Inventory sums
	// both so retention's eviction sees the true on-disk footprint.
	tmp := t.TempDir()
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), []byte("plain12345"))
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl.gz"), []byte("gz1234"))

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	fi, ok := findFI(inv, "2026-06-01", "a1b2c3d4")
	if !ok {
		t.Fatal("FileInfo not found")
	}
	if fi.SizeBytes != 16 {
		t.Errorf("SizeBytes = %d, want 16 (10 plain + 6 gz)", fi.SizeBytes)
	}
}

func TestWalkAndStat_JSONLPlusMedia(t *testing.T) {
	tmp := t.TempDir()
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), []byte("trace-row\n"))
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4", "media", "01ABCDEF", "0.png"), make([]byte, 100))
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4", "media", "01ABCDEF", "1.png"), make([]byte, 200))
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4", "media", "01ZYXWVU", "0.pdf"), make([]byte, 50))

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Files) != 1 {
		t.Fatalf("Files len = %d, want 1 (one FileID, JSONL + media merged)", len(inv.Files))
	}
	fi, ok := findFI(inv, "2026-06-01", "a1b2c3d4")
	if !ok {
		t.Fatal("FileInfo not found")
	}
	if fi.SizeBytes != 10 {
		t.Errorf("SizeBytes = %d, want 10", fi.SizeBytes)
	}
	if fi.MediaSize != 350 {
		t.Errorf("MediaSize = %d, want 350 (100+200+50)", fi.MediaSize)
	}
	if inv.TotalBytes != 360 {
		t.Errorf("TotalBytes = %d, want 360", inv.TotalBytes)
	}
}

func TestWalkAndStat_MediaWithoutJSONL(t *testing.T) {
	// An orphan media subtree (JSONL was deleted but media wasn't,
	// or media outlived the JSONL via crash). Inventory should still
	// surface it so retention's eviction can clean it up.
	tmp := t.TempDir()
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4", "media", "01ABCDEF", "0.png"), make([]byte, 100))

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Files) != 1 {
		t.Fatalf("Files len = %d, want 1", len(inv.Files))
	}
	fi, ok := findFI(inv, "2026-06-01", "a1b2c3d4")
	if !ok {
		t.Fatal("FileInfo not found")
	}
	if fi.SizeBytes != 0 {
		t.Errorf("SizeBytes = %d, want 0 (no JSONL on disk)", fi.SizeBytes)
	}
	if fi.MediaSize != 100 {
		t.Errorf("MediaSize = %d, want 100", fi.MediaSize)
	}
	if !fi.ModTime.IsZero() {
		t.Errorf("ModTime should be zero when JSONL doesn't exist; got %v", fi.ModTime)
	}
}

func TestWalkAndStat_MultipleBuckets(t *testing.T) {
	tmp := t.TempDir()
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), make([]byte, 100))
	mkFile(t, filepath.Join(tmp, "2026-06-01", "e5f6a7b8.jsonl"), make([]byte, 200))
	mkFile(t, filepath.Join(tmp, "2026-06-02", "a1b2c3d4.jsonl"), make([]byte, 300))

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Files) != 3 {
		t.Errorf("Files len = %d, want 3", len(inv.Files))
	}
	if inv.TotalBytes != 600 {
		t.Errorf("TotalBytes = %d, want 600", inv.TotalBytes)
	}
}

func TestWalkAndStat_ExcludedFiles(t *testing.T) {
	tmp := t.TempDir()
	mkFile(t, filepath.Join(tmp, "admin_token"), []byte("0123456789abcdef0123456789abcdef"))
	mkFile(t, filepath.Join(tmp, "index.sqlite"), make([]byte, 10000))
	mkFile(t, filepath.Join(tmp, "index.sqlite-wal"), make([]byte, 5000))
	mkFile(t, filepath.Join(tmp, "index.sqlite-shm"), make([]byte, 32))
	mkFile(t, filepath.Join(tmp, "runtime_overrides.json"), []byte(`{}`))
	mkFile(t, filepath.Join(tmp, "tmp", "abc.req.bin"), make([]byte, 5000))
	mkFile(t, filepath.Join(tmp, "tmp", "abc.resp.bin"), make([]byte, 50000))
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), make([]byte, 100))

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Files) != 1 {
		t.Errorf("Files len = %d, want 1 (only the JSONL counts)", len(inv.Files))
	}
	if inv.TotalBytes != 100 {
		t.Errorf("TotalBytes = %d, want 100 (admin_token + index.sqlite + wal/shm + overrides + tmp all excluded)", inv.TotalBytes)
	}
}

func TestWalkAndStat_IgnoresMalformedNames(t *testing.T) {
	tmp := t.TempDir()
	// Valid bucket
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl"), make([]byte, 50))
	// Malformed: bad date format
	mkFile(t, filepath.Join(tmp, "2026-6-1", "a1b2c3d4.jsonl"), make([]byte, 1000))
	// Malformed: bad keyhash length
	mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c3.jsonl"), make([]byte, 1000))
	// Malformed: uppercase keyhash
	mkFile(t, filepath.Join(tmp, "2026-06-01", "A1B2C3D4.jsonl"), make([]byte, 1000))
	// Stray non-jsonl file in date dir
	mkFile(t, filepath.Join(tmp, "2026-06-01", "stray.txt"), make([]byte, 1000))

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	if inv.TotalBytes != 50 {
		t.Errorf("TotalBytes = %d, want 50 (only the well-formed bucket); malformed entries silently ignored", inv.TotalBytes)
	}
}

func TestWalkAndStat_ContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	// Populate enough entries that cancellation can fire mid-walk
	for i := 0; i < 100; i++ {
		mkFile(t, filepath.Join(tmp, "2026-06-01", "a1b2c"+toHex2(i)+".jsonl"), make([]byte, 10))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel; walk should return ~immediately with ctx error

	_, err := walkAndStat(ctx, tmp)
	if err == nil {
		t.Error("expected context.Canceled, got nil")
	}
}

// toHex2 returns a 3-hex-char string built from the low 12 bits of n
// — used only to manufacture 8-char keyhashes (5 fixed + 3 varying)
// for the cancellation stress test.
func toHex2(n int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[(n>>8)&0xf], hex[(n>>4)&0xf], hex[n&0xf]})
}

func TestWalkAndStat_NonexistentDir(t *testing.T) {
	_, err := walkAndStat(context.Background(), "/this/path/does/not/exist")
	if err == nil {
		t.Error("expected error walking nonexistent dataDir")
	}
}

func TestWalkAndStat_ModTimeReflectsLatest(t *testing.T) {
	tmp := t.TempDir()
	plain := filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl")
	gz := filepath.Join(tmp, "2026-06-01", "a1b2c3d4.jsonl.gz")
	mkFile(t, plain, []byte("old"))
	mkFile(t, gz, []byte("new"))

	// Backdate plain so we can verify ModTime picks gz's newer time.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(plain, past, past); err != nil {
		t.Fatal(err)
	}

	inv, err := walkAndStat(context.Background(), tmp)
	if err != nil {
		t.Fatal(err)
	}
	fi, ok := findFI(inv, "2026-06-01", "a1b2c3d4")
	if !ok {
		t.Fatal("FileInfo not found")
	}
	if !fi.ModTime.After(past.Add(time.Hour)) {
		t.Errorf("ModTime = %v should be the gz's (recent) mtime, not the plain's (2h ago)", fi.ModTime)
	}
}
