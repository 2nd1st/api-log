package exporter

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/leoyun/api-log/internal/counters"
	"github.com/leoyun/api-log/internal/store/sqlite"
	"github.com/leoyun/api-log/internal/trace"
	"github.com/leoyun/api-log/internal/writer"
)

// TestWriteZipNoFilters builds a tiny on-disk corpus via the real writer
// (two key_hashes → two JSONL files in one day's directory), then runs
// WriteZip with no filters and asserts the zip carries:
//   - data/<date>/<keyhash>.jsonl  for each file (complete, not partial)
//   - agent/CLAUDE.md
//   - agent/jq-cheatsheet.md
//   - README.md
//
// We use writer.New + TrySend so JSONLOffset on disk matches what SQLite
// records — fewer chances for offset drift to silently invalidate the
// "matched lines" pass.
func TestWriteZipNoFilters(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctrs := counters.New()
	fixedNow := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	w := writer.New(dir, 16, store, ctrs, func() time.Time { return fixedNow })
	stop := w.Start()
	defer stop()

	// Two key_hashes → two distinct (date, keyhash) files. Each gets 2 traces
	// (4 total rows on disk; 4 lines across 2 files).
	mkTrace := func(id, path string) trace.Trace {
		return trace.Trace{
			ID:       id,
			TsStart:  fixedNow,
			TsEnd:    fixedNow.Add(time.Second),
			Client:   "127.0.0.1:1234",
			Method:   "POST",
			Path:     path,
			Upstream: "http://gw",
			Status:   200,
			Req: trace.Body{
				Headers: trace.Headers{"Content-Type": {"application/json"}},
				Body:    json.RawMessage(`{"model":"m","messages":[]}`),
			},
			Resp: trace.Body{
				Headers: trace.Headers{"Content-Type": {"application/json"}},
				Body:    json.RawMessage(`{"ok":true}`),
			},
		}
	}

	traces := []struct {
		id      string
		path    string
		keyHash string
	}{
		{"01H0000000000000000000A001", "/v1/chat/completions", "aaaa1111aaaa1111"},
		{"01H0000000000000000000A002", "/v1/chat/completions", "aaaa1111aaaa1111"},
		{"01H0000000000000000000B001", "/v1/embeddings", "bbbb2222bbbb2222"},
		{"01H0000000000000000000B002", "/v1/embeddings", "bbbb2222bbbb2222"},
	}
	for _, tc := range traces {
		if !w.TrySend(writer.Record{Trace: mkTrace(tc.id, tc.path), KeyHash: tc.keyHash}) {
			t.Fatalf("writer chan dropped %s", tc.id)
		}
	}

	// Wait for the writer to flush the 4 rows into SQLite.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := store.CountRows()
		if n == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n, _ := store.CountRows(); n != 4 {
		t.Fatalf("expected 4 rows after writer flush, got %d", n)
	}

	// Force the writer to release file handles so the exporter sees
	// stable byte counts on disk. stop() also waits on background gzip
	// workers — fine here because nothing rotated.
	stop()

	var buf bytes.Buffer
	if err := WriteZip(&buf, store, dir, sqlite.ListFilters{}, 0); err != nil {
		t.Fatalf("WriteZip: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}

	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)

	// Expected entries: two data files (complete .jsonl, not .partial),
	// two agent docs, one README.
	wantPresent := []string{
		"README.md",
		"agent/CLAUDE.md",
		"agent/jq-cheatsheet.md",
		"data/2026-05-27/aaaa1111.jsonl",
		"data/2026-05-27/bbbb2222.jsonl",
	}
	for _, want := range wantPresent {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("zip missing entry %q; entries: %v", want, names)
		}
	}

	// Negative check: nothing should land as .partial when filter is empty.
	for _, n := range names {
		if strings.Contains(n, ".partial.jsonl") {
			t.Errorf("unexpected partial entry %q with no filters", n)
		}
	}

	// Spot-check one data file has 2 JSON lines (one per trace for that key_hash).
	for _, f := range zr.File {
		if f.Name == "data/2026-05-27/aaaa1111.jsonl" {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open zip entry: %v", err)
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
			if len(lines) != 2 {
				t.Errorf("aaaa1111.jsonl has %d lines, want 2", len(lines))
			}
			// Each line must parse as JSON.
			for i, ln := range lines {
				var v map[string]any
				if err := json.Unmarshal(ln, &v); err != nil {
					t.Errorf("line %d not valid JSON: %v", i, err)
				}
			}
		}
	}

	// Check README mentions the matched count.
	for _, f := range zr.File {
		if f.Name == "README.md" {
			rc, _ := f.Open()
			data, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Contains(data, []byte("Traces matched: 4")) {
				t.Errorf("README does not mention matched count 4; got:\n%s", data)
			}
		}
	}
}

// TestWriteZipEmpty asserts the zip still carries the agent/ docs and a
// README when zero rows match (contract § Empty Export).
func TestWriteZipEmpty(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var buf bytes.Buffer
	if err := WriteZip(&buf, store, dir, sqlite.ListFilters{}, 0); err != nil {
		t.Fatalf("WriteZip empty: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	got := map[string]bool{}
	for _, f := range zr.File {
		got[f.Name] = true
	}
	for _, want := range []string{"README.md", "agent/CLAUDE.md", "agent/jq-cheatsheet.md"} {
		if !got[want] {
			t.Errorf("empty zip missing %q", want)
		}
	}
	// No data/ files.
	for n := range got {
		if strings.HasPrefix(n, "data/") {
			t.Errorf("empty zip should not contain %q", n)
		}
	}
}
