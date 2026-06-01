package sqlite

import (
	"context"
	"testing"
	"time"
)

func TestCountMatching_NoFilters(t *testing.T) {
	s := openTestStore(t)
	insertRowsAtPaths(t, s, "N",
		"data/2026-05-30/aaaaaaaa.jsonl",
		"data/2026-05-30/bbbbbbbb.jsonl",
		"data/2026-05-30/cccccccc.jsonl",
	)
	n, err := s.CountMatching(context.Background(), ListFilters{}, 0)
	if err != nil {
		t.Fatalf("CountMatching: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}

func TestCountMatching_CapStopsAtBoundary(t *testing.T) {
	s := openTestStore(t)
	// Insert 5 rows on distinct paths so any filter sees them all.
	for i := 0; i < 5; i++ {
		insertRowsAtPaths(t, s, "C"+toHexTest(i), "data/2026-05-30/"+toHexTest(i)+"abc1.jsonl")
	}
	// Cap=2: query should return 3 (= cap + 1 because LIMIT is cap+1).
	n, err := s.CountMatching(context.Background(), ListFilters{}, 2)
	if err != nil {
		t.Fatalf("CountMatching: %v", err)
	}
	if n != 3 {
		t.Errorf("capped count = %d, want 3 (cap=2 + 1 sentinel)", n)
	}
}

func TestCountMatching_FilterAppliedViaBuildListConds(t *testing.T) {
	s := openTestStore(t)
	ts := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	r1 := mkRow("F1", ts)
	r1.Path = "/v1/messages"
	r1.JSONLPath = "data/2026-05-30/aaaaaaaa.jsonl"
	if err := s.AppendTrace(r1, nil); err != nil {
		t.Fatal(err)
	}
	r2 := mkRow("F2", ts.Add(time.Second))
	r2.Path = "/v1/chat/completions"
	r2.JSONLPath = "data/2026-05-30/bbbbbbbb.jsonl"
	if err := s.AppendTrace(r2, nil); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountMatching(context.Background(), ListFilters{Path: "/v1/messages"}, 0)
	if err != nil {
		t.Fatalf("CountMatching: %v", err)
	}
	if n != 1 {
		t.Errorf("filtered count = %d, want 1", n)
	}
}

func TestCountMatching_EmptyStore(t *testing.T) {
	s := openTestStore(t)
	n, err := s.CountMatching(context.Background(), ListFilters{}, 1000)
	if err != nil {
		t.Fatalf("CountMatching: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}
