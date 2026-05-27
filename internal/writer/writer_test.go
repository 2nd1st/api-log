package writer

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leoyun/api-log/internal/trace"
)

func makeTrace(id string) trace.Trace {
	return trace.Trace{
		ID:       id,
		TsStart:  time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC),
		TsEnd:    time.Date(2026, 5, 27, 10, 0, 1, 0, time.UTC),
		Client:   "127.0.0.1:1234",
		Method:   "POST",
		Path:     "/v1/messages",
		Upstream: "http://gw:7860",
		Status:   200,
		Req: trace.Body{
			Headers: trace.Headers{"Content-Type": {"application/json"}},
			Body:    json.RawMessage(`{"k":"v"}`),
		},
		Resp: trace.Body{
			Headers: trace.Headers{"Content-Type": {"application/json"}},
			Body:    json.RawMessage(`{"ok":true}`),
		},
	}
}

func TestAppendOneWritesJSONLLine(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	w := New(dir, 16, nil, func() time.Time { return fixed })
	stop := w.Start()

	if !w.TrySend(Record{Trace: makeTrace("01H"), KeyHash: "a1b2c3d4e5f6a7b8"}) {
		t.Fatal("TrySend dropped")
	}
	stop()

	// File should exist at data/2026-05-27/a1b2c3d4.jsonl
	path := filepath.Join(dir, "2026-05-27", "a1b2c3d4.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected file %s: %v", path, err)
	}

	// Must be exactly one newline-terminated line.
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("line missing trailing newline: %q", string(data))
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Errorf("expected 1 newline, got %d", n)
	}

	// Round-trip via json.
	var tr trace.Trace
	if err := json.Unmarshal(data[:len(data)-1], &tr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tr.ID != "01H" {
		t.Errorf("ID = %q, want 01H", tr.ID)
	}
}

func TestPerKeyHashFileSeparation(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	w := New(dir, 16, nil, func() time.Time { return fixed })
	stop := w.Start()

	w.TrySend(Record{Trace: makeTrace("a-1"), KeyHash: "aaaaaaaa11111111"})
	w.TrySend(Record{Trace: makeTrace("b-1"), KeyHash: "bbbbbbbb22222222"})
	stop()

	entries, _ := os.ReadDir(filepath.Join(dir, "2026-05-27"))
	if len(entries) != 2 {
		t.Errorf("expected 2 files, got %d", len(entries))
	}
}

func TestNoAuthGoesToZeroHashFile(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, 4, nil, nil) // clock = time.Now
	stop := w.Start()
	w.TrySend(Record{Trace: makeTrace("01H"), KeyHash: ""})
	stop()

	// Find any .jsonl file under data/.
	var found string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".jsonl") {
			found = p
		}
		return nil
	})
	if !strings.Contains(found, "00000000.jsonl") {
		t.Errorf("expected 00000000.jsonl for empty key, got %s", found)
	}
}

func TestDailyRotationGzipsOldFile(t *testing.T) {
	dir := t.TempDir()
	var (
		muClock sync.Mutex
		now     = time.Date(2026, 5, 27, 23, 59, 59, 0, time.UTC)
	)
	clock := func() time.Time {
		muClock.Lock()
		defer muClock.Unlock()
		return now
	}
	w := New(dir, 16, nil, clock)
	stop := w.Start()

	// First trace lands on day 2026-05-27.
	w.TrySend(Record{Trace: makeTrace("a"), KeyHash: "aaaaaaaa00000000"})

	// Sleep a beat to ensure the goroutine processed the send.
	time.Sleep(50 * time.Millisecond)

	// Advance the clock past midnight.
	muClock.Lock()
	now = time.Date(2026, 5, 28, 0, 0, 1, 0, time.UTC)
	muClock.Unlock()

	// Second trace lands on day 2026-05-28; rotation should fire for
	// the old (date, key) handle.
	w.TrySend(Record{Trace: makeTrace("b"), KeyHash: "aaaaaaaa00000000"})

	stop() // waits for background gzip workers

	// 2026-05-27/aaaaaaaa.jsonl should be GONE and .jsonl.gz should exist.
	oldPlain := filepath.Join(dir, "2026-05-27", "aaaaaaaa.jsonl")
	oldGz := filepath.Join(dir, "2026-05-27", "aaaaaaaa.jsonl.gz")
	if _, err := os.Stat(oldPlain); !os.IsNotExist(err) {
		t.Errorf("plain old file should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(oldGz); err != nil {
		t.Errorf("gz file should exist, stat err = %v", err)
	}

	// And 2026-05-28/aaaaaaaa.jsonl should exist (still active day).
	newPath := filepath.Join(dir, "2026-05-28", "aaaaaaaa.jsonl")
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new-day file should exist, stat err = %v", err)
	}

	// Verify the gzipped content round-trips.
	f, err := os.Open(oldGz)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(gz)
	if !scanner.Scan() {
		t.Fatal("gz file empty")
	}
	var tr trace.Trace
	if err := json.Unmarshal(scanner.Bytes(), &tr); err != nil {
		t.Fatalf("unmarshal gz line: %v", err)
	}
	if tr.ID != "a" {
		t.Errorf("gz line ID = %q, want a", tr.ID)
	}
}

func TestTrySendDropsWhenFull(t *testing.T) {
	dir := t.TempDir()
	// Tiny channel + no goroutine started → all sends drop after one.
	w := New(dir, 1, nil, nil)
	// Don't call Start so nothing consumes.

	if !w.TrySend(Record{Trace: makeTrace("first"), KeyHash: "x"}) {
		t.Fatal("first send unexpectedly dropped")
	}
	// Subsequent sends drop.
	for i := 0; i < 5; i++ {
		if w.TrySend(Record{Trace: makeTrace("more"), KeyHash: "x"}) {
			t.Fatal("subsequent send unexpectedly accepted")
		}
	}
	s := w.SnapshotStats()
	if s.DropWriterFull != 5 {
		t.Errorf("DropWriterFull = %d, want 5", s.DropWriterFull)
	}
}

func TestStopIdempotent(t *testing.T) {
	w := New(t.TempDir(), 4, nil, nil)
	stop := w.Start()
	stop()
	// Second call must not panic.
	stop()
}
