package writer

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/2nd1st/api-log/internal/store/sqlite"
	"github.com/2nd1st/api-log/internal/trace"
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

	w := New(dir, 16, store, nil, nil, nil, func() time.Time {
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

	w := New(dir, 16, store, nil, nil, nil, func() time.Time {
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

// chatTraceWithUsage builds a /v1/chat/completions trace whose Resp.Body
// carries an OpenAI-shaped usage block + finish_reason. The Req.Headers
// also carries a User-Agent so R5a's parser.ExtractClient finds a kind +
// version at finalize. Used to verify both T3's usage wiring and R5a's
// client wiring in a single fixture — both extractors run off the same
// trace and feed the same Row, so co-locating keeps the writer test
// surface tight. (Per PHILOSOPHY §1 the writer only copies named
// protocol / header fields; this fixture matches the canonical OpenAI
// shape on the body and a CLI-shape on the header.)
func chatTraceWithUsage(id string) trace.Trace {
	reqBytes, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	respBytes, _ := json.Marshal(map[string]any{
		"id":    "resp_x",
		"model": "test-model",
		"choices": []map[string]any{
			{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "hello"}},
		},
		"usage": map[string]any{
			"prompt_tokens":     11,
			"completion_tokens": 22,
			"total_tokens":      33,
		},
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
			Headers: trace.Headers{
				"Content-Type": {"application/json"},
				"User-Agent":   {"claude-cli/1.0"},
			},
			Body: json.RawMessage(reqBytes),
		},
		Resp: trace.Body{
			Headers: trace.Headers{"Content-Type": {"application/json"}},
			Body:    json.RawMessage(respBytes),
		},
	}
}

// TestWriterPersistsExtractedUsage verifies T3 writer wiring: the writer
// calls parser.ExtractUsage at finalize and propagates the result to the
// SQLite Row so the named protocol fields land in their columns.
//
// Scoped to the 5 columns the canonical chatTraceWithUsage fixture's body
// populates (model, finish_reason, prompt_tokens, completion_tokens,
// total_tokens). The Row also binds cached_tokens / cache_creation_tokens
// / reasoning_tokens — round-trip for those is covered by
// TestAppendTraceUsageFieldsRoundTrip in internal/store/sqlite, which
// exercises the column binding directly with a fully populated Row.
func TestWriterPersistsExtractedUsage(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	w := New(dir, 16, store, nil, nil, nil, func() time.Time {
		return time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	})
	stop := w.Start()
	w.TrySend(Record{Trace: chatTraceWithUsage("tu1"), KeyHash: "uuuuuuuu00000000"})
	stop()

	db, err := sql.Open("sqlite", filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var model, finishReason, clientKind, clientVersion sql.NullString
	var promptTokens, completionTokens, totalTokens sql.NullInt64
	row := db.QueryRow(`SELECT model, finish_reason, prompt_tokens, completion_tokens, total_tokens, client_kind, client_version FROM traces WHERE id = 'tu1'`)
	if err := row.Scan(&model, &finishReason, &promptTokens, &completionTokens, &totalTokens, &clientKind, &clientVersion); err != nil {
		t.Fatal(err)
	}
	if !model.Valid || model.String != "test-model" {
		t.Errorf("model = %v, want test-model", model)
	}
	if !finishReason.Valid || finishReason.String != "stop" {
		t.Errorf("finish_reason = %v, want stop", finishReason)
	}
	if !promptTokens.Valid || promptTokens.Int64 != 11 {
		t.Errorf("prompt_tokens = %v, want 11", promptTokens)
	}
	if !completionTokens.Valid || completionTokens.Int64 != 22 {
		t.Errorf("completion_tokens = %v, want 22", completionTokens)
	}
	if !totalTokens.Valid || totalTokens.Int64 != 33 {
		t.Errorf("total_tokens = %v, want 33", totalTokens)
	}
	if !clientKind.Valid || clientKind.String != "claude-cli" {
		t.Errorf("client_kind = %v, want claude-cli", clientKind)
	}
	if !clientVersion.Valid || clientVersion.String != "1.0" {
		t.Errorf("client_version = %v, want 1.0", clientVersion)
	}
}

func TestWriterAcceptsNilStore(t *testing.T) {
	// Writer must work without a store (pure-JSONL mode used by tests).
	dir := t.TempDir()
	w := New(dir, 4, nil, nil, nil, nil, nil)
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
