package writer

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/leoyun/api-log/internal/store/sqlite"
	"github.com/leoyun/api-log/internal/trace"
	_ "modernc.org/sqlite"
)

// chatTrace builds a trace with a parseable /v1/chat/completions body.
func chatTrace(id string, msgs []map[string]any) trace.Trace {
	bodyBytes, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": msgs,
	})
	return trace.Trace{
		ID:       id,
		TsStart:  time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC),
		TsEnd:    time.Date(2026, 5, 27, 10, 0, 1, 0, time.UTC),
		Client:   "127.0.0.1:1234",
		Method:   "POST",
		Path:     "/v1/chat/completions",
		Upstream: "http://gw",
		Status:   200,
		Req: trace.Body{
			Headers: trace.Headers{"Content-Type": {"application/json"}},
			Body:    json.RawMessage(bodyBytes),
		},
		Resp: trace.Body{
			Headers: trace.Headers{"Content-Type": {"application/json"}},
			Body:    json.RawMessage(`{"id":"resp_x"}`),
		},
	}
}

func TestWriterAppendsToJSONLAndSQLite(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	w := New(dir, 16, store, nil, func() time.Time {
		return time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	})
	stop := w.Start()

	tr := chatTrace("t1", []map[string]any{{"role": "user", "content": "hi"}})
	w.TrySend(Record{Trace: tr, KeyHash: "aaaaaaaa11111111"})
	stop()

	// Inspect SQLite directly.
	db, err := sql.Open("sqlite", filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id, sessionRoot string
	var parentID sql.NullString
	var prefixLen sql.NullInt64
	row := db.QueryRow(`SELECT id, COALESCE(parent_id, ''), session_root_id, COALESCE(prefix_len, -1) FROM traces`)
	var pidStr string
	if err := row.Scan(&id, &pidStr, &sessionRoot, &prefixLen); err != nil {
		t.Fatal(err)
	}
	if id != "t1" {
		t.Errorf("id = %q", id)
	}
	if sessionRoot != "t1" {
		t.Errorf("session_root = %q, want t1 (self-root, first trace)", sessionRoot)
	}
	if pidStr != "" {
		t.Errorf("parent_id = %q, want empty", pidStr)
	}
	if !prefixLen.Valid || prefixLen.Int64 != 1 {
		t.Errorf("prefix_len = %v, want 1", prefixLen)
	}
	_ = parentID
}

func TestWriterSessionInferenceAcrossTraces(t *testing.T) {
	dir := t.TempDir()
	store, _ := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	defer store.Close()

	w := New(dir, 16, store, nil, func() time.Time {
		return time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	})
	stop := w.Start()

	// T1: single user message.
	t1 := chatTrace("t1", []map[string]any{{"role": "user", "content": "hi"}})
	w.TrySend(Record{Trace: t1, KeyHash: "kkkkkkkk00000000"})

	// T2: same key_hash, messages array extended → should chain to T1.
	t2 := chatTrace("t2", []map[string]any{
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "hello"},
		{"role": "user", "content": "how are you"},
	})
	w.TrySend(Record{Trace: t2, KeyHash: "kkkkkkkk00000000"})

	stop()

	db, _ := sql.Open("sqlite", filepath.Join(dir, "index.sqlite"))
	defer db.Close()
	var pid, root string
	row := db.QueryRow(`SELECT COALESCE(parent_id,''), session_root_id FROM traces WHERE id = 't2'`)
	if err := row.Scan(&pid, &root); err != nil {
		t.Fatal(err)
	}
	if pid != "t1" {
		t.Errorf("t2.parent = %q, want t1", pid)
	}
	if root != "t1" {
		t.Errorf("t2.session_root = %q, want t1", root)
	}
}

func TestWriterAcceptsNilStore(t *testing.T) {
	// Writer must work without a store (pure-JSONL mode used by tests).
	dir := t.TempDir()
	w := New(dir, 4, nil, nil, nil)
	stop := w.Start()
	w.TrySend(Record{Trace: chatTrace("t1", []map[string]any{{"role": "user", "content": "hi"}}), KeyHash: "xxxxxxxx11111111"})
	stop()
	s := w.SnapshotStats()
	if s.Appended != 1 {
		t.Errorf("Appended = %d, want 1", s.Appended)
	}
	if s.Indexed != 0 {
		t.Errorf("Indexed = %d, want 0 (no store)", s.Indexed)
	}
}
