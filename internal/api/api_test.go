package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2nd1st/api-log/internal/counters"
	"github.com/2nd1st/api-log/internal/store/sqlite"
	"github.com/2nd1st/api-log/internal/trace"
	"github.com/2nd1st/api-log/internal/writer"
)

func newTestServer(t *testing.T, token string) (*httptest.Server, *sqlite.Store, *writer.Writer, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctrs := counters.New()
	wrtr := writer.New(dir, 16, store, ctrs, nil, nil, func() time.Time {
		return time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	})
	stop := wrtr.Start()
	t.Cleanup(stop)

	mux := NewMux(Deps{
		Store:      store,
		Counters:   ctrs,
		AdminToken: token,
		Version:    "test",
		StartedAt:  time.Date(2026, 5, 27, 11, 0, 0, 0, time.UTC),
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store, wrtr, dir
}

func enqueueTrace(t *testing.T, w *writer.Writer, id, keyHash string, msgs []map[string]any) {
	t.Helper()
	bodyBytes, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": msgs,
	})
	tr := trace.Trace{
		ID:       id,
		TsStart:  time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC),
		TsEnd:    time.Date(2026, 5, 27, 12, 0, 1, 0, time.UTC),
		Client:   "127.0.0.1:1",
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
			Body:    json.RawMessage(`{"ok":true}`),
		},
	}
	if !w.TrySend(writer.Record{Trace: tr, KeyHash: keyHash}) {
		t.Fatal("TrySend dropped")
	}
}

// Wait for the writer goroutine to drain the channel and flush to SQLite.
// Simple polling — robust against scheduling jitter without relying on
// timing assumptions.
func waitForRows(t *testing.T, store *sqlite.Store, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.CountRows()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := store.CountRows()
	t.Fatalf("expected at least %d rows, got %d within deadline", n, got)
}

func TestHealthz(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "tok-test")
	// /healthz is intentionally unauthenticated so k8s liveness probes
	// and alertmanager (neither of which carry bearer tokens) can probe
	// the binary. See server.go NewMux docstring.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "ok" {
		t.Errorf("status = %v", got["status"])
	}
	if got["counters"] == nil {
		t.Errorf("counters missing: %s", body)
	}
}

func TestAuthRejectsMissingToken(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "tok-test")
	resp, err := http.Get(srv.URL + "/api/traces")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthRejectsWrongToken(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "tok-test")
	req, _ := http.NewRequest("GET", srv.URL+"/api/traces", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestListEmpty(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "tok-test")
	req, _ := http.NewRequest("GET", srv.URL+"/api/traces", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"traces":[]`) {
		t.Errorf("expected empty traces array, got %s", body)
	}
}

func TestListAfterIngest(t *testing.T) {
	srv, store, w, _ := newTestServer(t, "tok-test")

	enqueueTrace(t, w, "t1", "aaaaaaaa11111111",
		[]map[string]any{{"role": "user", "content": "hi"}})
	enqueueTrace(t, w, "t2", "aaaaaaaa11111111",
		[]map[string]any{{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
			{"role": "user", "content": "more"}})

	waitForRows(t, store, 2)

	req, _ := http.NewRequest("GET", srv.URL+"/api/traces", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	traces := got["traces"].([]any)
	if len(traces) != 2 {
		t.Errorf("len(traces) = %d, want 2", len(traces))
	}
	// Newest first.
	first := traces[0].(map[string]any)
	if first["id"] != "t2" {
		t.Errorf("first.id = %v, want t2 (newest)", first["id"])
	}
}

func TestListFiltersByKeyHashPrefix(t *testing.T) {
	srv, store, w, _ := newTestServer(t, "tok-test")
	enqueueTrace(t, w, "a", "aaaaaaaa11111111", []map[string]any{{"role": "user", "content": "hi"}})
	enqueueTrace(t, w, "b", "bbbbbbbb22222222", []map[string]any{{"role": "user", "content": "hi"}})
	waitForRows(t, store, 2)

	req, _ := http.NewRequest("GET", srv.URL+"/api/traces?key_hash=aaaaaaaa", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	tr := got["traces"].([]any)
	if len(tr) != 1 {
		t.Fatalf("filter result len = %d, want 1", len(tr))
	}
	if tr[0].(map[string]any)["id"] != "a" {
		t.Errorf("filtered trace id = %v, want a", tr[0].(map[string]any)["id"])
	}
}

func TestListBadParam(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "tok-test")
	req, _ := http.NewRequest("GET", srv.URL+"/api/traces?status=notnumber", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "bad_param") {
		t.Errorf("expected bad_param in body: %s", body)
	}
}

func TestGetByIDFetchesFromJSONL(t *testing.T) {
	srv, store, w, _ := newTestServer(t, "tok-test")
	enqueueTrace(t, w, "fetch-me", "ffffffff00000000",
		[]map[string]any{{"role": "user", "content": "find me"}})
	waitForRows(t, store, 1)

	req, _ := http.NewRequest("GET", srv.URL+"/api/traces/fetch-me", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, string(body))
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if got["row"] == nil || got["trace"] == nil {
		t.Errorf("response missing row or trace: %s", body)
	}
	traceObj := got["trace"].(map[string]any)
	if traceObj["id"] != "fetch-me" {
		t.Errorf("trace.id = %v, want fetch-me", traceObj["id"])
	}
}

func TestGetByIDNotFound(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "tok-test")
	req, _ := http.NewRequest("GET", srv.URL+"/api/traces/does-not-exist", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRootReturnsJSONPointer(t *testing.T) {
	// GET / returns a small unauthenticated JSON pointer (the binary
	// ships zero HTML; the separate api-log-viewer project is the
	// frontend). The pointer is a signpost for adopters and the
	// cheapest possible liveness probe.
	srv, _, _, _ := newTestServer(t, "tok-test")
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"viewer", "api", "healthz", "docs"} {
		if _, ok := body[k]; !ok {
			t.Errorf("body missing key %q (got: %v)", k, body)
		}
	}
}

func TestReplayMissingTrace(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "tok-test")
	req, _ := http.NewRequest("GET", srv.URL+"/api/traces/no-such/replay", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReplayNotStreaming(t *testing.T) {
	srv, store, w, _ := newTestServer(t, "tok-test")
	// Standard chatTrace ingested in tests has resp.body (non-streaming).
	enqueueTrace(t, w, "non-stream", "rrrrrrrr11111111",
		[]map[string]any{{"role": "user", "content": "hi"}})
	waitForRows(t, store, 1)

	req, _ := http.NewRequest("GET", srv.URL+"/api/traces/non-stream/replay", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (not_streaming)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not_streaming") {
		t.Errorf("expected not_streaming in body: %s", body)
	}
}

func TestReplayInvalidSpeed(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "tok-test")
	req, _ := http.NewRequest("GET", srv.URL+"/api/traces/x/replay?speed=0", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPaginationCursor(t *testing.T) {
	srv, store, w, _ := newTestServer(t, "tok-test")
	for i := 0; i < 5; i++ {
		enqueueTrace(t, w, "t"+string(rune('0'+i)), "aaaaaaaa11111111",
			[]map[string]any{{"role": "user", "content": "msg"}})
	}
	waitForRows(t, store, 5)

	req, _ := http.NewRequest("GET", srv.URL+"/api/traces?limit=2", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page1 map[string]any
	body1, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body1, &page1)
	tr1 := page1["traces"].([]any)
	if len(tr1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(tr1))
	}
	cursor, _ := page1["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("next_cursor empty, want non-empty: %s", body1)
	}

	req2, _ := http.NewRequest("GET", srv.URL+"/api/traces?limit=2&cursor="+cursor, nil)
	req2.Header.Set("Authorization", "Bearer tok-test")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	var page2 map[string]any
	_ = json.Unmarshal(body2, &page2)
	tr2 := page2["traces"].([]any)
	if len(tr2) != 2 {
		t.Errorf("page2 len = %d, want 2", len(tr2))
	}
	if tr1[0].(map[string]any)["id"] == tr2[0].(map[string]any)["id"] {
		t.Errorf("pagination didn't advance: first ids equal")
	}
}

// enqueueTraceWithSystem builds a /v1/messages trace with a system
// prompt that the parser will resolve to a known project name. Mirrors
// enqueueTrace's signature but takes an explicit project name string —
// the helper wraps it in a `# <name>` heading so
// parser.ExtractProjectContext picks "first-heading".
func enqueueTraceWithSystem(t *testing.T, w *writer.Writer, id, keyHash, projectName string) {
	t.Helper()
	reqBytes, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"system":   "# " + projectName + "\n\nbody",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	tr := trace.Trace{
		ID:       id,
		TsStart:  time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC),
		TsEnd:    time.Date(2026, 5, 27, 12, 0, 1, 0, time.UTC),
		Client:   "127.0.0.1:1",
		Method:   "POST",
		Path:     "/v1/messages",
		Upstream: "http://gw",
		Status:   200,
		Req: trace.Body{
			Headers: trace.Headers{"Content-Type": {"application/json"}},
			Body:    json.RawMessage(reqBytes),
		},
		Resp: trace.Body{
			Headers: trace.Headers{"Content-Type": {"application/json"}},
			Body:    json.RawMessage(`{"id":"x"}`),
		},
	}
	if !w.TrySend(writer.Record{Trace: tr, KeyHash: keyHash}) {
		t.Fatal("TrySend dropped")
	}
}

// TestListFiltersByProject verifies W4.1 Phase 2's project filter end
// to end: the writer extracts the project name from the request body's
// system text, stores it in client_project, and ?project=X returns only
// the matching rows.
func TestListFiltersByProject(t *testing.T) {
	srv, store, w, _ := newTestServer(t, "tok-test")
	enqueueTraceWithSystem(t, w, "p-alpha", "aaaaaaaa11111111", "alpha")
	enqueueTraceWithSystem(t, w, "p-beta", "bbbbbbbb22222222", "beta")
	enqueueTraceWithSystem(t, w, "p-alpha-2", "cccccccc33333333", "alpha")
	waitForRows(t, store, 3)

	req, _ := http.NewRequest("GET", srv.URL+"/api/traces?project=alpha", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	tr := got["traces"].([]any)
	if len(tr) != 2 {
		t.Fatalf("filter len = %d, want 2 (only alpha rows). body=%s", len(tr), body)
	}
	for _, row := range tr {
		m := row.(map[string]any)
		// rowJSON.ClientProject is omitempty, so a populated row carries
		// the field as a string; missing rows wouldn't show up here.
		if m["client_project"] != "alpha" {
			t.Errorf("filtered row %v has client_project = %v, want alpha",
				m["id"], m["client_project"])
		}
	}

	// No filter returns all three rows.
	req2, _ := http.NewRequest("GET", srv.URL+"/api/traces", nil)
	req2.Header.Set("Authorization", "Bearer tok-test")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	var got2 map[string]any
	_ = json.Unmarshal(body2, &got2)
	if len(got2["traces"].([]any)) != 3 {
		t.Errorf("unfiltered len = %d, want 3", len(got2["traces"].([]any)))
	}
}

// Avoid unused imports in this file when iterating.
var _ = os.Open
