package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStreamMatching_VisitsAllRowsInOrder(t *testing.T) {
	s := openTestStore(t)
	ts := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	for i, id := range []string{"01H_A", "01H_B", "01H_C"} {
		r := mkRow(id, ts.Add(time.Duration(i)*time.Second))
		r.JSONLPath = "data/2026-05-30/" + toHexTest(i) + "0001.jsonl"
		if err := s.AppendTrace(r, nil); err != nil {
			t.Fatalf("AppendTrace #%d: %v", i, err)
		}
	}

	var seen []string
	err := s.StreamMatching(context.Background(), ListFilters{}, func(r Row) error {
		seen = append(seen, r.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamMatching: %v", err)
	}
	want := []string{"01H_A", "01H_B", "01H_C"}
	if len(seen) != len(want) {
		t.Fatalf("visited %v, want %v", seen, want)
	}
	for i, id := range want {
		if seen[i] != id {
			t.Errorf("seen[%d] = %q, want %q (ts_start ASC ordering)", i, seen[i], id)
		}
	}
}

func TestStreamMatching_VisitErrorAborts(t *testing.T) {
	s := openTestStore(t)
	insertRowsAtPaths(t, s, "E",
		"data/2026-05-30/aaaa1111.jsonl",
		"data/2026-05-30/bbbb2222.jsonl",
		"data/2026-05-30/cccc3333.jsonl",
	)

	sentinel := errors.New("stop")
	visited := 0
	err := s.StreamMatching(context.Background(), ListFilters{}, func(_ Row) error {
		visited++
		if visited == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
	if visited != 2 {
		t.Errorf("visited = %d, want 2 (stop on second row)", visited)
	}
}

func TestStreamMatching_FilterAppliedViaBuildListConds(t *testing.T) {
	s := openTestStore(t)
	ts := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	r1 := mkRow("F1", ts)
	r1.Path = "/v1/messages"
	r1.JSONLPath = "data/2026-05-30/aaaa1111.jsonl"
	if err := s.AppendTrace(r1, nil); err != nil {
		t.Fatal(err)
	}
	r2 := mkRow("F2", ts.Add(time.Second))
	r2.Path = "/v1/chat/completions"
	r2.JSONLPath = "data/2026-05-30/bbbb2222.jsonl"
	if err := s.AppendTrace(r2, nil); err != nil {
		t.Fatal(err)
	}

	var seen []string
	err := s.StreamMatching(context.Background(),
		ListFilters{Path: "/v1/messages"},
		func(r Row) error {
			seen = append(seen, r.ID)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "F1" {
		t.Errorf("filtered stream = %v, want [F1]", seen)
	}
}

func TestStreamMatching_RespectsContextCancel(t *testing.T) {
	s := openTestStore(t)
	// 5 rows, cancel after first visit.
	for i := 0; i < 5; i++ {
		insertRowsAtPaths(t, s, "C"+toHexTest(i), "data/2026-05-30/"+toHexTest(i)+"feed.jsonl")
	}

	ctx, cancel := context.WithCancel(context.Background())
	visited := 0
	err := s.StreamMatching(ctx, ListFilters{}, func(_ Row) error {
		visited++
		if visited == 1 {
			cancel()
		}
		return nil
	})
	if err == nil {
		t.Error("expected context cancel error")
	}
	if visited >= 5 {
		t.Errorf("ctx cancel did not abort: visited = %d", visited)
	}
}

func TestStreamMatching_EmptyStoreNoVisitsNoError(t *testing.T) {
	s := openTestStore(t)
	calls := 0
	err := s.StreamMatching(context.Background(), ListFilters{}, func(_ Row) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 0 {
		t.Errorf("visit called %d times on empty store", calls)
	}
}
