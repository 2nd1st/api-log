// Package writer is the single-writer goroutine described in ARCHITECTURE
// § 7 and § 8. It appends JSONL and mirrors traces into SQLite with session
// inference inside the same goroutine.
//
// Daily rotation per § 7.4: file date is decided by *write time*, not by
// trace.TsStart. A trace whose ts_start is yesterday but whose finalize
// happens after midnight lands in today's file. Consumers reconcile via
// ts_start if they care about request-arrival grouping.
package writer

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/2nd1st/api-log/internal/counters"
	"github.com/2nd1st/api-log/internal/ids"
	"github.com/2nd1st/api-log/internal/media"
	"github.com/2nd1st/api-log/internal/parser"
	"github.com/2nd1st/api-log/internal/session"
	"github.com/2nd1st/api-log/internal/storage"
	"github.com/2nd1st/api-log/internal/store/sqlite"
	"github.com/2nd1st/api-log/internal/trace"
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
	dataDir  string
	clock    func() time.Time
	in       chan Record
	store    *sqlite.Store      // optional; nil = JSONL-only mode (used in tests / M2 mode)
	counters *counters.Counters // optional; nil = no counter wiring (tests can omit)

	// Phase K — media extraction. Both fields optional; either nil means
	// "extraction disabled" and the writer behaves exactly as before. The
	// toggle is *atomic.Bool (not a plain bool) because PUT /api/config/media
	// can flip it at runtime from a different goroutine; the writer goroutine
	// reads it on every trace finalize.
	mediaExtractor *media.Extractor
	mediaEnabled   *atomic.Bool

	// v0.1.1 — storage coordinator. Optional; nil disables lease
	// arbitration (tests + JSONL-only setups). When non-nil, fileFor()
	// acquires a lease BEFORE opening the OS file, and the lease is
	// held for the lifetime of the open handle. Eviction is then safe
	// to interleave with writes: deleteIfIdle skips any path with
	// active leases, and the writer skips traces whose bucket is being
	// deleted by returning ErrFileBeingDeleted upward.
	coord *storage.Coordinator

	// idleTimeout is the duration after which an open (date, keyhash)
	// bucket with no recent appends gets its handle closed + lease
	// released. 0 (the default) disables idle-close entirely — files
	// stay open until date-cross rotation or process shutdown.
	//
	// Why not zero by default in prod: long-tail keys keep their fds
	// open forever otherwise, and the retention loop can't touch
	// today's quiet buckets until idle-close drops their lease.
	// Configure via SetIdleTimeout BEFORE calling Start.
	//
	// idleSweep does NOT gzip — the bucket may legitimately re-open
	// later in the day for more appends, and date-cross is still the
	// only event that triggers gzip rotation. Idle-close just frees
	// fds + leases; the JSONL stays in plain (uncompressed) form on
	// disk until the day flips.
	idleTimeout time.Duration

	// gzip workers run in the background when files rotate. We wait for
	// them on Close so a shutdown doesn't leave a half-gzipped file behind.
	gzWG sync.WaitGroup

	// Stats keeps a writer-local copy for tests / SnapshotStats. The
	// counters package is the shared cross-process view used by /healthz;
	// Stats is just convenience for unit tests.
	stats Stats
	mu    sync.Mutex // guards stats only

	// Per-(date, keyhash) open file handles.
	files map[string]*openFile
}

// Stats are the cumulative drop / overflow counters this writer exposes.
type Stats struct {
	DropWriterFull int64 // TrySend dropped because channel was full
	DropJSONLFail  int64 // JSONL append failed (disk full, EIO, etc.)
	DropSQLiteFail int64 // SQLite upsert failed (busy timeout, schema err, …)
	Appended       int64 // successful JSONL appends
	Indexed        int64 // successful SQLite upserts (≤ Appended)
}

// New creates a Writer with the given chan capacity. Call Start to
// launch its goroutine. Use TrySend to enqueue records. Call the stop
// fn (from Start) on shutdown to drain the channel and wait for
// background gzip workers.
//
// store may be nil; tests and pure-JSONL deployments leave it nil.
// ctrs may be nil; counters skipped if so.
// clock may be nil; defaults to time.Now.
//
// mediaExt and mediaEnabled (Phase K) may both be nil; either nil means
// "no extraction." When both are non-nil, the writer reads mediaEnabled
// on each finalize and, if true, invokes mediaExt.Extract on the trace
// after the JSONL line is on disk. Extraction failures log WARN inside
// the extractor and never block the writer or affect MediaCount past the
// returned slice length (capture should never interfere).
//
// coord (v0.1.1) is the storage coordinator. nil disables lease
// arbitration entirely — the writer opens/closes files without
// notifying retention. Production main wires a non-nil coord whose
// DataDir matches the writer's dataDir; the coord-side eviction loop
// can then safely interleave with appends.
func New(dataDir string, chanCap int, store *sqlite.Store, ctrs *counters.Counters, mediaExt *media.Extractor, mediaEnabled *atomic.Bool, coord *storage.Coordinator, clock func() time.Time) *Writer {
	if clock == nil {
		clock = time.Now
	}
	return &Writer{
		dataDir:        dataDir,
		clock:          clock,
		in:             make(chan Record, chanCap),
		store:          store,
		counters:       ctrs,
		mediaExtractor: mediaExt,
		mediaEnabled:   mediaEnabled,
		coord:          coord,
		files:          make(map[string]*openFile),
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
			// remain here. Release lease BEFORE / alongside Close —
			// order doesn't matter (refcount, not file lock).
			for _, of := range w.files {
				if of.lease != nil {
					of.lease.Release()
				}
				_ = of.f.Close()
			}
		})
	}
}

// SetIdleTimeout configures the idle-close threshold. Must be called
// BEFORE Start. Zero (the default) disables idle-close entirely.
// Non-zero values are clamped at the sweep level: the writer ticks at
// idleTimeout / 2 (or once a minute, whichever is shorter) and closes
// any handle whose last append is at least idleTimeout ago.
func (w *Writer) SetIdleTimeout(d time.Duration) { w.idleTimeout = d }

// TrySend non-blockingly enqueues a record. Returns false (and bumps
// DropWriterFull) if the channel is full. Producers MUST NOT block on
// the writer per ARCHITECTURE § 7.5 step 3.
func (w *Writer) TrySend(r Record) bool {
	// Observe high-water mark for /healthz.
	if w.counters != nil {
		w.counters.ObserveWriterChanLen(len(w.in))
	}
	select {
	case w.in <- r:
		return true
	default:
		w.mu.Lock()
		w.stats.DropWriterFull++
		w.mu.Unlock()
		if w.counters != nil {
			w.counters.IncDropWriterFull()
		}
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
	// Idle-close disabled — keep the cheap drain loop.
	if w.idleTimeout <= 0 {
		for rec := range w.in {
			w.appendOne(rec)
		}
		return
	}

	// Idle-close enabled — multiplex appendOne with periodic sweeps.
	// Sweep cadence = min(idleTimeout/2, 1 minute). 1m floor caps the
	// goroutine wakeup rate in prod where idleTimeout might be hours;
	// idleTimeout/2 keeps tests responsive when idleTimeout is small.
	sweepEvery := w.idleTimeout / 2
	if sweepEvery > time.Minute {
		sweepEvery = time.Minute
	}
	if sweepEvery <= 0 {
		// Defensive — idleTimeout > 0 implies /2 > 0 except for
		// nanosecond values; fall back to 1ms to keep the ticker alive.
		sweepEvery = time.Millisecond
	}
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()

	for {
		select {
		case rec, ok := <-w.in:
			if !ok {
				return // channel closed → graceful shutdown
			}
			w.appendOne(rec)
		case <-ticker.C:
			w.idleSweep()
		}
	}
}

// idleSweep closes any open bucket whose last append is older than
// w.idleTimeout. Releases the storage lease BEFORE closing the OS
// handle — order doesn't matter (refcount, not file lock), but doing
// it first means a fast eviction loop catches the release sooner.
//
// Does NOT gzip the closed bucket. The next append for the same
// (date, keyhash) goes through fileFor → fresh open + lease, and
// gzip still fires at date-cross rotation in fileFor's existing
// branch. Idle-close just trims live fds + lease holds; the on-disk
// .jsonl stays put.
//
// Runs in the writer goroutine (serialized with appendOne) so we
// don't need extra locking around w.files.
func (w *Writer) idleSweep() {
	if w.idleTimeout <= 0 {
		return
	}
	now := w.clock()
	for k, of := range w.files {
		if now.Sub(of.lastWrite) < w.idleTimeout {
			continue
		}
		// Eligible — close + release.
		if of.lease != nil {
			of.lease.Release()
		}
		if err := of.f.Close(); err != nil {
			slog.Warn("idle-close: file close failed", "path", of.path, "err", err)
		}
		delete(w.files, k)
	}
}

func (w *Writer) appendOne(rec Record) {
	now := w.clock()
	date := now.UTC().Format("2006-01-02")
	keyHash := rec.KeyHash
	if keyHash == "" {
		keyHash = ids.AllZeroKeyHash
	}
	hashShort := ids.KeyHashShort(keyHash)

	of, err := w.fileFor(date, hashShort)
	if err != nil {
		w.bumpJSONLFail()
		return
	}

	line, err := marshalLine(rec.Trace)
	if err != nil {
		w.bumpJSONLFail()
		return
	}

	// Pre-write offset — where this line begins. Stored in SQLite so
	// the read API can seek directly to it.
	offset, err := currentSize(of.f)
	if err != nil {
		w.bumpJSONLFail()
		return
	}
	if _, err := of.f.Write(line); err != nil {
		w.bumpJSONLFail()
		return
	}
	// Stamp lastWrite AFTER the successful Write — failed writes don't
	// keep the bucket alive in idleSweep's eyes. The clock is the
	// injectable one (so test fixtures can advance time deterministically
	// for idle-close coverage); same value already used for `date` above.
	of.lastWrite = now
	w.mu.Lock()
	w.stats.Appended++
	w.mu.Unlock()
	if w.counters != nil {
		w.counters.IncAppended()
		w.counters.IncAppendedByStatus(rec.Trace.Status)
		w.counters.AddBytes(int64(len(line)))
	}

	// Usage extraction: deterministic copy of named protocol fields, with no
	// synthesis. It runs after JSONL is on disk, so extraction misses never
	// block the writer. The bumped counters and Row fields are derived caches;
	// replaying parser.ExtractUsage over the JSONL regenerates the same values.
	// The single extractor call is reused below to populate the SQLite Row.
	usage := parser.ExtractUsage(rec.Trace)
	if w.counters != nil {
		if usage.PromptTokens != nil {
			w.counters.AddPromptTokens(*usage.PromptTokens)
		}
		if usage.CompletionTokens != nil {
			w.counters.AddCompletionTokens(*usage.CompletionTokens)
		}
		if usage.CachedTokens != nil {
			w.counters.AddCachedTokens(*usage.CachedTokens)
		}
		if usage.CacheCreationTokens != nil {
			w.counters.AddCacheCreationTokens(*usage.CacheCreationTokens)
		}
		if usage.ReasoningTokens != nil {
			w.counters.AddReasoningTokens(*usage.ReasoningTokens)
		}
	}

	// Client extraction: deterministic copy of named protocol/header fields; no
	// general-purpose UA parsing and no body sniffing. It runs after JSONL is on
	// disk and never blocks the writer. The derived Row.ClientKind /
	// ClientVersion are rebuildable by replaying ExtractClient over req.headers
	// from the JSONL. No per-kind counters: the SQLite columns alone are enough
	// to drive viewer/group-by until a concrete need forces more surface area.
	client := parser.ExtractClient(rec.Trace.Req.Headers)

	// Project-context extraction: deterministic copy of the project name parsed
	// from request-body system/instructions text. Same discipline as the client
	// and usage extractors above: runs after JSONL is on disk, never blocks the
	// writer, rebuildable from JSONL by replaying the same parser. No counter:
	// the SQLite column alone is the surface.
	project := parser.ExtractProjectContext(parser.ExtractSystemPrompt(rec.Trace))

	// Media extraction runs after JSONL is on disk, so any failure here doesn't
	// affect what was captured. The extractor itself logs WARN on per-file
	// errors and returns whatever it managed to write; we never block on it.
	//
	// JSONL is the source of truth; extracted media files are a derived copy +
	// cache for fast read / export bundling. The base64 stays inline in the
	// JSONL line that's already been flushed.
	//
	// Placed BEFORE the store == nil return so JSONL-only deployments
	// (used in tests + plain-file recording mode) still extract media —
	// the SQLite mirror is just a query index, not a precondition for
	// extraction.
	var mediaCount int
	if w.mediaExtractor != nil && w.mediaEnabled != nil && w.mediaEnabled.Load() {
		// Pass the writer's chosen bucket so media lands beside its JSONL
		// even when trace.TsStart and the writer's date differ across a
		// UTC midnight rotation. of.bucket is set by fileFor.
		files := w.mediaExtractor.Extract(rec.Trace, of.bucket)
		mediaCount = len(files)
		if w.counters != nil && mediaCount > 0 {
			w.counters.AddMediaFiles(int64(mediaCount))
		}
	}

	if w.store == nil {
		return
	}

	// SQLite mirror + session inference in one transaction (one fsync,
	// not two). Failure here doesn't roll back the JSONL — the JSONL is
	// already on disk and a startup rebuild will recover.
	row := sqlite.FromTrace(rec.Trace, keyHash, of.path, offset)
	row.MediaCount = mediaCount
	// Reuse the usage value extracted above so counters and the SQLite Row see
	// exactly the same numbers from the same parse. Nil fields stay nil in the
	// Row (distinct from a real zero) as required by the nullable column
	// contract.
	row.Model = usage.Model
	row.FinishReason = usage.FinishReason
	row.PromptTokens = usage.PromptTokens
	row.CompletionTokens = usage.CompletionTokens
	row.TotalTokens = usage.TotalTokens
	row.CachedTokens = usage.CachedTokens
	row.CacheCreationTokens = usage.CacheCreationTokens
	row.ReasoningTokens = usage.ReasoningTokens
	// Client kind/version are sourced from the same finalize-time parse as the
	// usage block; nil stays nil so field absence remains distinct from a
	// present empty value.
	row.ClientKind = client.Kind
	row.ClientVersion = client.Version
	// Project name from the same finalize-time parse.
	// Empty Name (zero ProjectContext) leaves row.ClientProject nil so
	// "no project signal" stays distinct from a real empty string.
	if project.Name != "" {
		name := project.Name
		row.ClientProject = &name
	}
	prefix, _ := session.Build(rec.Trace.Path, sessionPrefixBody(rec.Trace))
	sqlStart := time.Now()
	if err := w.store.AppendTrace(row, prefix); err != nil {
		w.bumpSQLiteFail()
		return
	}
	if w.counters != nil {
		w.counters.SQLiteHist.Observe(time.Since(sqlStart).Milliseconds())
	}
	w.mu.Lock()
	w.stats.Indexed++
	w.mu.Unlock()
	if w.counters != nil {
		w.counters.IncIndexed()
	}
}

// sessionPrefixBody picks the JSON body to feed session.Build. Returns
// nil when the request body wasn't parseable JSON (then the trace has
// no session prefix — landing as a self-root in SQLite).
func sessionPrefixBody(t trace.Trace) []byte {
	if len(t.Req.Body) == 0 {
		return nil
	}
	return []byte(t.Req.Body)
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

func (w *Writer) bumpJSONLFail() {
	w.mu.Lock()
	w.stats.DropJSONLFail++
	w.mu.Unlock()
	if w.counters != nil {
		w.counters.IncDropJSONLFail()
	}
}

func (w *Writer) bumpSQLiteFail() {
	w.mu.Lock()
	w.stats.DropSQLiteFail++
	w.mu.Unlock()
	if w.counters != nil {
		w.counters.IncDropSQLiteFail()
	}
}

// ---- per-file handles + rotation ----

type openFile struct {
	path   string
	date   string
	hash   string // keyhash[:8]
	f      *os.File
	bucket storage.FileID // canonical identity; passed to media.Extract
	lease  *storage.Lease // held for the lifetime of f; nil when coord disabled

	// lastWrite is stamped on every successful append (and on initial
	// fileFor open). idleSweep uses it to decide whether the handle
	// has been quiet long enough to close + release the lease.
	// Zero-valued handles are treated as "just opened" by fileFor's
	// initialization below; idleSweep computes `now - lastWrite`
	// which on a fresh handle is small, so nothing gets swept on the
	// first tick.
	lastWrite time.Time
}

func keyFor(date, hashShort string) string { return date + "/" + hashShort }

// fileFor returns an open file handle for the given (date, keyhash) pair,
// rotating closed files in the background as the date crosses midnight.
//
// Rotation policy: when a date != currentDate appears on the writer
// channel, we close that old handle (if open) and schedule a background
// gzip on the closed file's path. We do NOT close handles on every
// append — only when their date is no longer current for any key_hash
// observed since.
//
// v0.1.1 — lease arbitration with the storage coordinator:
//
//   - Lease MUST be acquired BEFORE os.OpenFile. Acquiring after creates
//     a TOCTOU window where eviction could observe refcount=0 and start
//     deleting the file while we hold the freshly-opened *os.File.
//   - If AcquireLease returns ErrFileBeingDeleted (eviction is mid-delete
//     on this bucket), fileFor surfaces the error so appendOne bumps
//     DropJSONLFail and routes to a new bucket on the next finalize.
//   - On rotation / shutdown, lease.Release() runs alongside f.Close().
//     Order doesn't matter — lease is a refcount, not a file lock.
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
			oldBucket := oldOf.bucket
			if oldOf.lease != nil {
				oldOf.lease.Release()
			}
			if err := oldOf.f.Close(); err != nil {
				_ = err // best-effort; the gzip below will still try.
			}
			w.gzWG.Add(1)
			coord := w.coord
			go func() {
				defer w.gzWG.Done()
				if err := compressInPlace(coord, oldBucket, oldPath); err != nil {
					slog.Warn("gzip rotation failed", "path", oldPath, "err", err)
				}
			}()
		}
	}

	// Bucket identity — load-bearing for media co-location AND for
	// lease arbitration. The DataDir on the FileID must match what
	// w.dataDir/<date>/<hashShort>.jsonl resolves to; we construct
	// FileID with the same dataDir the writer was given at New.
	bucket := storage.FileID{
		DataDir:  w.dataDir,
		Date:     date,
		KeyHash8: hashShort,
	}

	// Acquire lease BEFORE OpenFile — see the comment block on this
	// method. With nil coord the writer behaves identically to v0.1.0:
	// no lease tracked, the file just opens.
	var lease *storage.Lease
	if w.coord != nil {
		var err error
		lease, err = w.coord.AcquireLease(bucket)
		if err != nil {
			// ErrFileBeingDeleted is the expected race outcome —
			// surfaced upward without modification so callers can log
			// at a quieter level if they wish.
			return nil, fmt.Errorf("acquire lease for %s: %w", bucket.CanonicalPath(), err)
		}
	}

	// Open new handle. mkdir -p of `data/<date>/`.
	dir := filepath.Join(w.dataDir, date)
	// 0o700 / 0o600 — the JSONL line contains the full request body
	// including raw API keys (Authorization / x-api-key headers); the
	// owning process is the only legitimate reader. Adopters who need
	// broader read access (backup systems, ops tools) chmod themselves.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if lease != nil {
			lease.Release()
		}
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, hashShort+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		if lease != nil {
			lease.Release()
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	of := &openFile{
		path:      path,
		date:      date,
		hash:      hashShort,
		f:         f,
		bucket:    bucket,
		lease:     lease,
		lastWrite: w.clock(),
	}
	w.files[k] = of
	return of, nil
}

// compressInPlace writes <path>.gz from <path>, then removes <path>.
// If anything goes wrong the original .jsonl stays on disk so the next
// startup can recover.
//
// The bucket lease is reacquired around the gzip work so retention
// doesn't race with us: we just released the rotation-time lease in
// fileFor before launching this goroutine; without re-acquiring, an
// in-between eviction tick could see refcount=0 and start deleting the
// file mid-Open. With coord=nil (no retention), the lease block is a
// no-op and behavior matches v0.1.0.
func compressInPlace(coord *storage.Coordinator, bucket storage.FileID, path string) error {
	if coord != nil {
		lease, err := coord.AcquireLease(bucket)
		if err != nil {
			// ErrFileBeingDeleted — retention already removed this
			// bucket; nothing to gzip.
			return fmt.Errorf("acquire lease for gzip %s: %w", path, err)
		}
		defer lease.Release()
	}

	// Defer import of gzip; compress/gzip is stdlib so cheap.
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	gzPath := path + ".gz"
	dst, err := os.OpenFile(gzPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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
