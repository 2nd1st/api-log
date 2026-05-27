// Package writer is the single-writer goroutine described in ARCHITECTURE
// § 7 and § 8. M2 scope: JSONL append only. M3 will extend with SQLite
// upsert + session inference inside this same goroutine.
//
// Daily rotation per § 7.4: file date is decided by *write time*, not by
// trace.TsStart. A trace whose ts_start is yesterday but whose finalize
// happens after midnight lands in today's file. Consumers reconcile via
// ts_start if they care about request-arrival grouping.
package writer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/leoyun/api-log/internal/ids"
	"github.com/leoyun/api-log/internal/trace"
)

// Record is one item on the writer channel. It carries the trace plus a
// key_hash for file naming. We compute key_hash here (not in the trace
// struct) because it's not part of the JSONL schema — it's only used by
// the writer to pick the file, and by M3's SQLite mirror.
type Record struct {
	Trace   trace.Trace
	KeyHash string // 16-hex full key_hash; writer uses KeyHashShort for filename
}

// AppendResult is returned via Record.Reply (if non-nil) after the line
// is written. M3 uses Offset to populate SQLite's jsonl_offset column.
type AppendResult struct {
	JSONLPath   string // e.g. "data/2026-05-27/a1b2c3d4.jsonl"
	JSONLOffset int64  // pre-write byte offset where the line starts
	Err         error
}

// Writer owns the JSONL writing pipeline. Exactly one goroutine writes
// to disk; producers send Records on a bounded channel via TrySend.
type Writer struct {
	dataDir string
	clock   func() time.Time
	in      chan Record

	// gzip workers run in the background when files rotate. We wait for
	// them on Close so a shutdown doesn't leave a half-gzipped file behind.
	gzWG sync.WaitGroup

	// Counters wires drop / overflow events to /healthz in M4. M2 keeps
	// them inline as atomic counters available via Stats().
	stats Stats
	mu    sync.Mutex // guards stats only

	// Per-(date, keyhash) open file handles.
	files map[string]*openFile
}

// Stats are the cumulative drop / overflow counters this writer exposes.
// Will be lifted into a shared Counters package in M4.
type Stats struct {
	DropWriterFull int64 // TrySend dropped because channel was full
	DropJSONLFail  int64 // append failed (disk full, EIO, etc.)
	Appended       int64 // successful appends
}

// New creates a Writer with the given chan capacity. Call Start to
// launch its goroutine. Use TrySend to enqueue records. Call Close on
// shutdown to drain the channel and wait for background gzip workers.
//
// clock may be nil; defaults to time.Now.
func New(dataDir string, chanCap int, clock func() time.Time) *Writer {
	if clock == nil {
		clock = time.Now
	}
	return &Writer{
		dataDir: dataDir,
		clock:   clock,
		in:      make(chan Record, chanCap),
		files:   make(map[string]*openFile),
	}
}

// Start launches the writer goroutine. Returns a stop function; calling
// stop closes the channel and waits for the goroutine to drain it
// (then waits for background gzip workers). Safe to call stop multiple
// times; only the first call does work.
func (w *Writer) Start() func() {
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		w.runLoop()
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			close(w.in)
			<-doneCh
			w.gzWG.Wait()
			// Close any still-open file handles. Files closed cleanly
			// in runLoop on rotation; only the current-day handles
			// remain here.
			for _, of := range w.files {
				_ = of.f.Close()
			}
		})
	}
}

// TrySend non-blockingly enqueues a record. Returns false (and bumps
// DropWriterFull) if the channel is full. Producers MUST NOT block on
// the writer per ARCHITECTURE § 7.5 step 3.
func (w *Writer) TrySend(r Record) bool {
	select {
	case w.in <- r:
		return true
	default:
		w.mu.Lock()
		w.stats.DropWriterFull++
		w.mu.Unlock()
		return false
	}
}

// SnapshotStats returns a copy of the current counters. Safe for
// concurrent callers (M4's /healthz will use this).
func (w *Writer) SnapshotStats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// ---- internal goroutine ----

func (w *Writer) runLoop() {
	for rec := range w.in {
		w.appendOne(rec)
	}
}

func (w *Writer) appendOne(rec Record) {
	now := w.clock()
	date := now.UTC().Format("2006-01-02")
	hashShort := ids.KeyHashShort(rec.KeyHash)
	if hashShort == "" {
		hashShort = ids.KeyHashShort(ids.AllZeroKeyHash)
	}

	of, err := w.fileFor(date, hashShort)
	if err != nil {
		w.bumpFail()
		_ = err // M2 logs at the call site if needed; writer is silent here.
		return
	}

	line, err := marshalLine(rec.Trace)
	if err != nil {
		w.bumpFail()
		return
	}

	// Record the pre-write offset so M3's SQLite mirror can later seek
	// to this line by (jsonl_path, jsonl_offset).
	offset, err := currentSize(of.f)
	if err != nil {
		w.bumpFail()
		return
	}
	if _, err := of.f.Write(line); err != nil {
		w.bumpFail()
		return
	}
	w.mu.Lock()
	w.stats.Appended++
	w.mu.Unlock()

	// Pre-write offset is recorded; M3 will route this back to producers.
	_ = offset
}

func marshalLine(t trace.Trace) ([]byte, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("marshal trace: %w", err)
	}
	// Newline-terminated for jq / line-oriented tools.
	b = append(b, '\n')
	return b, nil
}

func currentSize(f *os.File) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (w *Writer) bumpFail() {
	w.mu.Lock()
	w.stats.DropJSONLFail++
	w.mu.Unlock()
}

// ---- per-file handles + rotation ----

type openFile struct {
	path string
	date string
	hash string // keyhash[:8]
	f    *os.File
}

func (of *openFile) key() string { return of.date + "/" + of.hash }

func keyFor(date, hashShort string) string { return date + "/" + hashShort }

// fileFor returns an open file handle for the given (date, keyhash) pair,
// rotating closed files in the background as the date crosses midnight.
//
// Rotation policy: when a date != currentDate appears on the writer
// channel, we close that old handle (if open) and schedule a background
// gzip on the closed file's path. We do NOT close handles on every
// append — only when their date is no longer current for any key_hash
// observed since.
func (w *Writer) fileFor(date, hashShort string) (*openFile, error) {
	k := keyFor(date, hashShort)
	if of, ok := w.files[k]; ok {
		return of, nil
	}

	// Close + schedule gzip for any handle whose date is < this date.
	for oldKey, oldOf := range w.files {
		if oldOf.date < date {
			delete(w.files, oldKey)
			oldPath := oldOf.path
			if err := oldOf.f.Close(); err != nil {
				_ = err // best-effort; the gzip below will still try.
			}
			w.gzWG.Add(1)
			go func() {
				defer w.gzWG.Done()
				_ = compressInPlace(oldPath)
			}()
		}
	}

	// Open new handle. mkdir -p of `data/<date>/`.
	dir := filepath.Join(w.dataDir, date)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, hashShort+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	of := &openFile{path: path, date: date, hash: hashShort, f: f}
	w.files[k] = of
	return of, nil
}

// compressInPlace writes <path>.gz from <path>, then removes <path>.
// If anything goes wrong the original .jsonl stays on disk so the next
// startup can recover.
func compressInPlace(path string) error {
	// Defer import of gzip; compress/gzip is stdlib so cheap.
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	gzPath := path + ".gz"
	dst, err := os.OpenFile(gzPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	gz := newGzWriter(dst)
	if _, err := io.Copy(gz, src); err != nil {
		_ = gz.Close()
		_ = dst.Close()
		_ = os.Remove(gzPath)
		return err
	}
	if err := gz.Close(); err != nil {
		_ = dst.Close()
		_ = os.Remove(gzPath)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(gzPath)
		return err
	}
	if err := os.Remove(path); err != nil {
		// gzip succeeded but original removal failed; not fatal —
		// next startup will rerun and overwrite the .gz.
		return err
	}
	return nil
}

// sentinel for the test helper to mock failure
var errInjected = errors.New("injected")

var _ = errInjected // M2 has no injection points yet; placeholder for M6 chaos tests.
