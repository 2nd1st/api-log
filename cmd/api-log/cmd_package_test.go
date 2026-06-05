package main

import (
	"archive/zip"
	"encoding/json"
	"io"
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

// TestParsePackageTime covers the two shapes the CLI accepts for the
// -from / -to flags. RFC3339 is the canonical form; bare YYYY-MM-DD is
// the operator's shorthand. Empty input must return the zero time so
// the caller can leave the bound open.
func TestParsePackageTime(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    time.Time
		wantErr bool
	}{
		{
			name: "empty returns zero time (open bound)",
			in:   "",
			want: time.Time{},
		},
		{
			name: "RFC3339 UTC parsed verbatim",
			in:   "2026-06-08T03:14:15Z",
			want: time.Date(2026, 6, 8, 3, 14, 15, 0, time.UTC),
		},
		{
			name: "RFC3339 with offset coerced to UTC",
			in:   "2026-06-08T03:14:15+08:00",
			want: time.Date(2026, 6, 7, 19, 14, 15, 0, time.UTC),
		},
		{
			name: "bare date interpreted as UTC midnight",
			in:   "2026-06-08",
			want: time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "whitespace trimmed",
			in:   "  2026-06-08  ",
			want: time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "garbage rejected with hint",
			in:      "yesterday",
			wantErr: true,
		},
		{
			name:    "epoch seconds rejected — would silently produce wrong window",
			in:      "1717804800",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePackageTime(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

// TestOpenPackageOutputDefaultFilename confirms the default-name path
// produces a portable filename (no colons — Windows / S3 / NAS shares
// reject them in object keys). The stamp's RFC3339 source uses colons
// in HH:MM:SS so the replacement is load-bearing.
func TestOpenPackageOutputDefaultFilename(t *testing.T) {
	dir := t.TempDir()
	// Go 1.22 has no t.Chdir; restore manually so the test stays
	// self-contained.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	_, closer, err := openPackageOutput("")
	if err != nil {
		t.Fatalf("openPackageOutput: %v", err)
	}
	closer()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var matched string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "api-log-package-") && strings.HasSuffix(name, ".zip") {
			matched = name
		}
	}
	if matched == "" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no api-log-package-*.zip created; saw %v", names)
	}
	if strings.Contains(matched, ":") {
		t.Fatalf("filename %q contains colon; not portable to Windows / S3", matched)
	}
	// Sanity: file is on disk, openable.
	if _, err := os.Stat(filepath.Join(dir, matched)); err != nil {
		t.Fatalf("stat: %v", err)
	}
}

// TestRunPackageEndToEnd seeds a tiny corpus through the real writer
// (so SQLite offsets match bytes on disk), then drives runPackage end
// to end the way an operator would — read the resulting zip and assert
// it carries the expected JSONL + agent/CLAUDE.md + README. The point
// is to exercise the wiring: arg parsing → config.Load → sqlite.Open →
// exporter.WriteZip → zip file on disk. The exporter package owns its
// own deeper coverage; this is the surface-area test.
func TestRunPackageEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	outDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dataDir, "index.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	ctrs := counters.New()
	fixedNow := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	w := writer.New(dataDir, 16, store, ctrs, nil, nil, nil, func() time.Time { return fixedNow })
	stop := w.Start()

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
		id, path, key string
	}{
		{"01H0000000000000000000A001", "/v1/chat/completions", "aaaa1111aaaa1111"},
		{"01H0000000000000000000A002", "/v1/chat/completions", "aaaa1111aaaa1111"},
	}
	for _, tr := range traces {
		if !w.TrySend(writer.Record{Trace: mkTrace(tr.id, tr.path), KeyHash: tr.key}) {
			t.Fatalf("writer chan dropped %s", tr.id)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := store.CountRows(); n == int64(len(traces)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Flush the writer so the JSONL file is closed and stable on disk
	// before runPackage opens it through its own SQLite handle.
	stop()
	_ = store.Close()

	outPath := filepath.Join(outDir, "result.zip")
	t.Setenv("APILOG_STORAGE_DATA_DIR", dataDir)
	if err := runPackage([]string{"-out", outPath, "-from", "2026-06-08", "-to", "2026-06-09"}); err != nil {
		t.Fatalf("runPackage: %v", err)
	}

	f, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("open result: %v", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}

	wantPresent := []string{
		"README.md",
		"agent/CLAUDE.md",
		"agent/jq-cheatsheet.md",
		"data/2026-06-08/aaaa1111.jsonl",
	}
	have := make(map[string]bool, len(zr.File))
	for _, ze := range zr.File {
		have[ze.Name] = true
	}
	for _, want := range wantPresent {
		if !have[want] {
			names := make([]string, 0, len(have))
			for k := range have {
				names = append(names, k)
			}
			t.Errorf("zip missing %q; entries: %v", want, names)
		}
	}

	// Spot-check the data file has 2 valid JSON lines.
	for _, ze := range zr.File {
		if ze.Name != "data/2026-06-08/aaaa1111.jsonl" {
			continue
		}
		rc, err := ze.Open()
		if err != nil {
			t.Fatalf("open jsonl: %v", err)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("aaaa1111.jsonl has %d lines, want 2", len(lines))
		}
		for i, ln := range lines {
			var v map[string]any
			if err := json.Unmarshal([]byte(ln), &v); err != nil {
				t.Fatalf("line %d not valid JSON: %v", i, err)
			}
		}
	}
}
