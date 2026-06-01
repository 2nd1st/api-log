package sqlite

import (
	"context"
	"testing"
	"time"
)

// insertRowsAtPaths is a focused helper that appends rows differing
// only in jsonl_path. Keeps delete tests independent of the broader
// AppendTrace fixtures.
func insertRowsAtPaths(t *testing.T, s *Store, idPrefix string, paths ...string) {
	t.Helper()
	ts := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	for i, p := range paths {
		r := mkRow(idPrefix+"_"+toHexTest(i), ts.Add(time.Duration(i)*time.Second))
		r.JSONLPath = p
		if err := s.AppendTrace(r, nil); err != nil {
			t.Fatalf("AppendTrace #%d: %v", i, err)
		}
	}
}

// toHexTest produces a stable 4-char hex suffix for test IDs.
func toHexTest(n int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[(n>>12)&0xf], hex[(n>>8)&0xf], hex[(n>>4)&0xf], hex[n&0xf]})
}

func TestDeleteByJSONLPath_RemovesAllMatching(t *testing.T) {
	s := openTestStore(t)
	pathA := "data/2026-05-30/aaaaaaaa.jsonl"
	pathB := "data/2026-05-30/bbbbbbbb.jsonl"

	// Two rows on pathA, one on pathB.
	insertRowsAtPaths(t, s, "A", pathA, pathA, pathB)

	n, err := s.DeleteByJSONLPath(context.Background(), pathA)
	if err != nil {
		t.Fatalf("DeleteByJSONLPath: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted rows = %d, want 2", n)
	}

	cnt, err := s.CountRows()
	if err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("remaining rows = %d, want 1 (only pathB)", cnt)
	}

	// pathB still listed.
	paths, err := s.ListDistinctJSONLPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != pathB {
		t.Errorf("ListDistinctJSONLPaths after delete = %v, want [%s]", paths, pathB)
	}
}

func TestDeleteByJSONLPath_NoMatchIsNotAnError(t *testing.T) {
	s := openTestStore(t)
	n, err := s.DeleteByJSONLPath(context.Background(), "data/2026-05-30/ghost.jsonl")
	if err != nil {
		t.Fatalf("DeleteByJSONLPath on empty: %v", err)
	}
	if n != 0 {
		t.Errorf("affected = %d, want 0", n)
	}
}

func TestDeleteByJSONLPath_RespectsContext(t *testing.T) {
	s := openTestStore(t)
	insertRowsAtPaths(t, s, "C", "data/2026-05-30/cccccccc.jsonl")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.DeleteByJSONLPath(ctx, "data/2026-05-30/cccccccc.jsonl"); err == nil {
		t.Error("expected canceled-ctx to surface as error")
	}
}

func TestListDistinctJSONLPaths_DeduplicatesAndSorts(t *testing.T) {
	s := openTestStore(t)
	insertRowsAtPaths(t, s,
		"L",
		"data/2026-05-30/cccccccc.jsonl",
		"data/2026-05-30/aaaaaaaa.jsonl",
		"data/2026-05-30/bbbbbbbb.jsonl",
		"data/2026-05-30/aaaaaaaa.jsonl", // duplicate
	)

	paths, err := s.ListDistinctJSONLPaths(context.Background())
	if err != nil {
		t.Fatalf("ListDistinctJSONLPaths: %v", err)
	}
	want := []string{
		"data/2026-05-30/aaaaaaaa.jsonl",
		"data/2026-05-30/bbbbbbbb.jsonl",
		"data/2026-05-30/cccccccc.jsonl",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i, p := range want {
		if paths[i] != p {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], p)
		}
	}
}

func TestListDistinctJSONLPaths_EmptyStore(t *testing.T) {
	s := openTestStore(t)
	paths, err := s.ListDistinctJSONLPaths(context.Background())
	if err != nil {
		t.Fatalf("ListDistinctJSONLPaths: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("empty store paths = %v, want empty", paths)
	}
}
