package sqlite

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mkRow(id string, ts time.Time) Row {
	return Row{
		ID:          id,
		TsStart:     ts,
		TsEnd:       ts.Add(time.Second),
		Client:      "127.0.0.1:1234",
		Method:      "POST",
		Path:        "/v1/messages",
		Upstream:    "http://gw",
		Status:      200,
		KeyHash:     "abcd1234abcd1234",
		JSONLPath:   "data/2026-05-27/abcd1234.jsonl",
		JSONLOffset: 0,
	}
}

func TestOpenAndMigrate(t *testing.T) {
	s := openTestStore(t)
	n, err := s.CountRows()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("fresh store row count = %d, want 0", n)
	}
}

func TestAppendTraceNoSession(t *testing.T) {
	s := openTestStore(t)
	ts := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	r := mkRow("01H_first", ts)
	if err := s.AppendTrace(r, nil); err != nil {
		t.Fatalf("AppendTrace: %v", err)
	}
	n, _ := s.CountRows()
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

func TestAppendTraceSessionInferenceChain(t *testing.T) {
	s := openTestStore(t)
	ts := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	t1Prefix := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"hi"}`),
	}
	t2Prefix := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"hi"}`),
		json.RawMessage(`{"role":"assistant","content":"hello"}`),
		json.RawMessage(`{"role":"user","content":"how are you"}`),
	}

	r1 := mkRow("01H_T1", ts)
	if err := s.AppendTrace(r1, t1Prefix); err != nil {
		t.Fatal(err)
	}

	r2 := mkRow("01H_T2", ts.Add(time.Second))
	if err := s.AppendTrace(r2, t2Prefix); err != nil {
		t.Fatal(err)
	}

	// Query r2 to confirm parent + root.
	pid, root, plen, ok, err := s.FindParent(r2.KeyHash, []string{
		// Build hashes for prefixes of t2Prefix in length-desc order
		// (longer first). Length 2 strict prefix is t1Prefix.messages
		// — t1Prefix has length 1 so t2's strict prefix at k=1 should
		// match t1.prefix_canonical_hash.
	})
	_ = pid
	_ = root
	_ = plen
	_ = ok
	_ = err

	// Direct table introspection: easier to verify.
	var parentID, sessionRoot string
	row := s.db.QueryRow("SELECT COALESCE(parent_id, ''), session_root_id FROM traces WHERE id = ?", "01H_T2")
	if err := row.Scan(&parentID, &sessionRoot); err != nil {
		t.Fatal(err)
	}
	if parentID != "01H_T1" {
		t.Errorf("T2 parent = %q, want 01H_T1", parentID)
	}
	if sessionRoot != "01H_T1" {
		t.Errorf("T2 session_root = %q, want 01H_T1", sessionRoot)
	}

	// T1 should be its own root with no parent.
	row = s.db.QueryRow("SELECT COALESCE(parent_id, ''), session_root_id FROM traces WHERE id = ?", "01H_T1")
	if err := row.Scan(&parentID, &sessionRoot); err != nil {
		t.Fatal(err)
	}
	if parentID != "" {
		t.Errorf("T1 should have no parent, got %q", parentID)
	}
	if sessionRoot != "01H_T1" {
		t.Errorf("T1 session_root = %q, want 01H_T1", sessionRoot)
	}
}

func TestAppendTraceDifferentKeysIsolated(t *testing.T) {
	s := openTestStore(t)
	ts := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	prefix := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}

	rA := mkRow("01H_A", ts)
	rA.KeyHash = "aaaaaaaaaaaaaaaa"
	if err := s.AppendTrace(rA, prefix); err != nil {
		t.Fatal(err)
	}

	rB := mkRow("01H_B", ts.Add(time.Second))
	rB.KeyHash = "bbbbbbbbbbbbbbbb"
	if err := s.AppendTrace(rB, prefix); err != nil {
		t.Fatal(err)
	}

	// Same messages, different keys → neither is the other's parent.
	for _, id := range []string{"01H_A", "01H_B"} {
		var pid string
		row := s.db.QueryRow("SELECT COALESCE(parent_id, '') FROM traces WHERE id = ?", id)
		if err := row.Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != "" {
			t.Errorf("trace %s: parent_id should be empty, got %q", id, pid)
		}
	}
}

func TestAppendTraceSessionForkSiblings(t *testing.T) {
	s := openTestStore(t)
	ts := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	t1 := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}
	tA := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"hi"}`),
		json.RawMessage(`{"role":"assistant","content":"hello"}`),
		json.RawMessage(`{"role":"user","content":"branch-A"}`),
	}
	tB := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"hi"}`),
		json.RawMessage(`{"role":"assistant","content":"hello"}`),
		json.RawMessage(`{"role":"user","content":"branch-B"}`),
	}

	r1 := mkRow("01H_root", ts)
	if err := s.AppendTrace(r1, t1); err != nil {
		t.Fatal(err)
	}
	rA := mkRow("01H_A", ts.Add(time.Second))
	if err := s.AppendTrace(rA, tA); err != nil {
		t.Fatal(err)
	}
	rB := mkRow("01H_B", ts.Add(2*time.Second))
	if err := s.AppendTrace(rB, tB); err != nil {
		t.Fatal(err)
	}

	// Both branches: parent should be root, session_root should be root.
	for _, id := range []string{"01H_A", "01H_B"} {
		var pid, root string
		row := s.db.QueryRow("SELECT COALESCE(parent_id, ''), session_root_id FROM traces WHERE id = ?", id)
		if err := row.Scan(&pid, &root); err != nil {
			t.Fatal(err)
		}
		if pid != "01H_root" {
			t.Errorf("%s parent = %q, want 01H_root", id, pid)
		}
		if root != "01H_root" {
			t.Errorf("%s session_root = %q, want 01H_root", id, root)
		}
	}
}

func TestAppendTraceMediaCountRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ts := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)

	// Row with a non-zero media_count — mirrors what the writer fills in
	// after the Phase K extractor returns the count of extracted files.
	r := mkRow("01H_media", ts)
	r.MediaCount = 3
	if err := s.AppendTrace(r, nil); err != nil {
		t.Fatalf("AppendTrace: %v", err)
	}

	got, err := s.GetByID("01H_media")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.MediaCount != 3 {
		t.Errorf("MediaCount round-trip = %d, want 3", got.MediaCount)
	}

	// Row without an explicit MediaCount should default to 0.
	r0 := mkRow("01H_no_media", ts.Add(time.Second))
	if err := s.AppendTrace(r0, nil); err != nil {
		t.Fatalf("AppendTrace zero: %v", err)
	}
	got0, err := s.GetByID("01H_no_media")
	if err != nil {
		t.Fatalf("GetByID zero: %v", err)
	}
	if got0.MediaCount != 0 {
		t.Errorf("default MediaCount = %d, want 0", got0.MediaCount)
	}
}

func TestMigrateIdempotentOnReopen(t *testing.T) {
	// Open + close + re-open the same file. The ALTER TABLE on the second
	// migrate() pass must NOT error (PHILOSOPHY § 6: schema is append-only,
	// migrations must be idempotent).
	path := filepath.Join(t.TempDir(), "reopen.sqlite")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open (idempotent migrate failed): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
}

func TestAppendTraceIdempotentByID(t *testing.T) {
	s := openTestStore(t)
	ts := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	r := mkRow("01H_dup", ts)
	if err := s.AppendTrace(r, nil); err != nil {
		t.Fatal(err)
	}
	// Same ID again — INSERT OR REPLACE keeps row count at 1.
	if err := s.AppendTrace(r, nil); err != nil {
		t.Fatal(err)
	}
	n, _ := s.CountRows()
	if n != 1 {
		t.Errorf("count = %d, want 1 (INSERT OR REPLACE)", n)
	}
}
