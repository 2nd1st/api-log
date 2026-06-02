// Package exporter builds the /api/export zip. It ships JSONL bytes straight
// from disk into a streaming zip with no transformation; SQLite only selects
// which JSONL lines to include.
//
// The zip layout follows docs/specs/phase-i-export-contract.md and
// docs/specs/phase-k-media-contract.md:
//
//	data/<YYYY-MM-DD>/<keyhash8>.jsonl          (all rows on disk matched)
//	data/<YYYY-MM-DD>/<keyhash8>.partial.jsonl  (subset matched)
//	media/<trace_id>/<idx>.<ext>                 (extracted attachments, if any)
//	agent/CLAUDE.md
//	agent/jq-cheatsheet.md
//	README.md
package exporter

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/2nd1st/api-log/internal/storage"
	"github.com/2nd1st/api-log/internal/store/sqlite"
)

//go:embed templates/CLAUDE.md templates/jq-cheatsheet.md
var templatesFS embed.FS

// fileGroup carries the rows that share one source JSONL file. The
// exporter buckets rows by JSONLPath and turns each bucket into one zip
// entry, with the name picking complete-vs-partial based on whether
// every line in the source file is in the match set.
type fileGroup struct {
	path       string           // absolute or relative jsonl path on disk
	offsets    map[int64]string // jsonl_offset -> trace.ID
	earliestTs time.Time
	date       string // YYYY-MM-DD parsed from JSONLPath dir name
	keyhash    string // keyhash8 parsed from JSONLPath base
}

// mediaRef is the slim per-row record the streaming exporter carries
// from Phase 1 (cursor) to Phase 2 (zip writes). It MUST stay small —
// the full Row would balloon memory on 100k-row exports — so it holds
// only what writeMediaForTrace actually needs to locate the on-disk
// subtree and stamp the zip entry's modified time.
type mediaRef struct {
	traceID string
	date    string // YYYY-MM-DD bucket directory
	keyhash string // keyhash8 bucket directory
	tsStart time.Time
}

// WriteZip streams a zip archive of every row matched by filters into w.
//
// Two-phase pipeline (v0.1.1):
//
//  1. Cursor (one SQLite conn borrowed via StreamMatching) walks rows,
//     builds the per-file groups (jsonl_offset → trace ID) and the
//     slim mediaRef slice. Incremental minTs / maxTs / matched count.
//     No zip bytes are written here — if the SQLite query errors, the
//     caller can still return a clean HTTP status.
//  2. After the cursor closes (conn released back to the pool), the
//     zip is built. zip.NewWriter is registered with a level-1 Deflate
//     compressor — adopters trade ~5% size for ~3× wall-clock on 100k+
//     row exports (the workload that motivated streaming in the first
//     place). Each (date, keyhash) group acquires a storage lease in
//     an inner-func scope, writes its entry, and releases on iteration
//     exit; the naive top-of-loop defer would pin every bucket.
//
// dataDir is the storage root that JSONL paths in the SQLite rows are
// relative to (or absolute; we accept either — the row's JSONLPath is
// the authoritative reader-side path).
//
// coord (v0.1.1) is the storage coordinator. nil disables lease
// arbitration: the exporter reads JSONL files without notifying
// retention — fine for tests / no-retention deployments. Non-nil:
// groups whose bucket is mid-eviction (ErrFileBeingDeleted) are
// skipped with a WARN log; the rest of the zip still streams cleanly.
//
// ctx is propagated into StreamMatching (cursor abort on cancel) and
// honored at every group + media iteration. Cancel mid-export leaves
// the zip well-formed up to the cancel point — adopters see a
// truncated body, never a corrupted central directory.
//
// No internal hardCap: callers gate oversize exports via
// CountMatching BEFORE calling WriteZip. The cursor walks every
// matching row; pre-flight is the contract.
//
// The returned error means either the SQLite read failed (zip not yet
// started) or a disk / zip write failed mid-stream (zip may be
// partial — zw.Close() is still called via defer to write the central
// directory of whatever was emitted, but downstream callers should
// treat the archive as suspect).
func WriteZip(ctx context.Context, w io.Writer, store *sqlite.Store, dataDir string, filters sqlite.ListFilters, coord *storage.Coordinator) error {
	// Phase 1 — cursor. Build groups + media refs without materializing
	// the full result set in memory.
	groups := make(map[string]*fileGroup)
	var mediaRefs []mediaRef
	var (
		minTs        time.Time
		maxTs        time.Time
		matchedCount int
	)
	streamErr := store.StreamMatching(ctx, filters, func(r sqlite.Row) error {
		matchedCount++
		g, ok := groups[r.JSONLPath]
		if !ok {
			date, keyhash := splitJSONLPath(dataDir, r.JSONLPath)
			g = &fileGroup{
				path:       r.JSONLPath,
				offsets:    make(map[int64]string),
				earliestTs: r.TsStart,
				date:       date,
				keyhash:    keyhash,
			}
			groups[r.JSONLPath] = g
		}
		g.offsets[r.JSONLOffset] = r.ID
		if r.TsStart.Before(g.earliestTs) {
			g.earliestTs = r.TsStart
		}

		// Media ref — slim copy so we can locate the per-trace subtree
		// in Phase 2 after the full Row has been GC'd.
		mediaRefs = append(mediaRefs, mediaRef{
			traceID: r.ID,
			date:    g.date,
			keyhash: g.keyhash,
			tsStart: r.TsStart,
		})

		if minTs.IsZero() || r.TsStart.Before(minTs) {
			minTs = r.TsStart
		}
		if r.TsStart.After(maxTs) {
			maxTs = r.TsStart
		}
		return nil
	})
	if streamErr != nil {
		return fmt.Errorf("query matching rows: %w", streamErr)
	}

	// Phase 2 — zip. The cursor + its conn are now released; the zip
	// build below is conn-free filesystem + memory work.
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()
	// Level-1 Deflate (BestSpeed). Must be installed BEFORE any entry
	// is created. Defaults to zip.Deflate which is gzip's level 6 —
	// fine for tiny archives, prohibitive at 100k entries. Operators
	// trade ~5% size for ~3× wall-clock; the zip stays compatible with
	// every standard unzip implementation.
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})

	// Deterministic emission order: by date then keyhash. Keeps two
	// identical exports byte-identical (modulo Deflate-1 nondeterminism
	// in some platform combinations — content is byte-identical, zip
	// framing may differ).
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		gi, gj := groups[keys[i]], groups[keys[j]]
		if gi.date != gj.date {
			return gi.date < gj.date
		}
		if gi.keyhash != gj.keyhash {
			return gi.keyhash < gj.keyhash
		}
		return keys[i] < keys[j]
	})

	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		g := groups[k]
		err := func() error {
			if coord != nil {
				fid, ferr := storage.FileIDFromPath(dataDir, g.path)
				if ferr != nil {
					// Path doesn't match the canonical writer shape —
					// rare, but if it happens just read without a lease;
					// retention won't touch it either.
					return writeGroupEntry(zw, g)
				}
				lease, lerr := coord.AcquireLease(fid)
				if lerr != nil {
					if errors.Is(lerr, storage.ErrFileBeingDeleted) {
						slog.Warn("export: group skipped, bucket being deleted",
							"date", fid.Date, "key_hash8", fid.KeyHash8)
						return nil
					}
					return fmt.Errorf("acquire lease for %s: %w", g.path, lerr)
				}
				defer lease.Release()
			}
			return writeGroupEntry(zw, g)
		}()
		if err != nil {
			return fmt.Errorf("write entry for %s: %w", g.path, err)
		}
	}

	// Bundle extracted media files alongside the JSONL. The extractor runs
	// at finalize time, and files on disk are the source of truth here;
	// exporter only copies. Read errors are silent on purpose: traces
	// recorded with save_attachments=false legitimately have no dir, and a
	// missing dir must not abort the export of the JSONL that *is* there.
	// Iterate refs in deterministic order (sorted trace ID) so two
	// identical exports stay byte-identical.
	sort.Slice(mediaRefs, func(i, j int) bool {
		return mediaRefs[i].traceID < mediaRefs[j].traceID
	})
	mediaIncluded := 0
	for _, mr := range mediaRefs {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := writeMediaForRef(zw, dataDir, mr)
		if err != nil {
			return fmt.Errorf("bundle media for %s: %w", mr.traceID, err)
		}
		mediaIncluded += n
	}

	// Embed agent/ templates (always present, even on empty exports per
	// contract § Empty Export).
	if err := copyEmbed(zw, "templates/CLAUDE.md", "agent/CLAUDE.md"); err != nil {
		return err
	}
	if err := copyEmbed(zw, "templates/jq-cheatsheet.md", "agent/jq-cheatsheet.md"); err != nil {
		return err
	}

	// README — human-readable summary. Use the explicit cursor counter
	// (matchedCount) rather than re-traversing the cursor data — no row
	// slice is kept around in the streaming model.
	if err := writeReadme(zw, matchedCount, mediaIncluded, filters, minTs, maxTs, time.Now().UTC()); err != nil {
		return err
	}

	// Explicit Close — the deferred Close is best-effort; surface real
	// errors to the caller so the API handler logs them.
	return zw.Close()
}

// writeMediaForRef copies every file under
// `${dataDir}/${date}/${keyhash8}/media/${trace_id}/` into the zip at
// `media/${trace_id}/${name}`. Returns the number of files added.
//
// The mediaRef carries date + keyhash from Phase 1 — the full Row has
// already been GC'd by the time we get here, so the slim ref is
// load-bearing for memory in big exports.
//
// Silently returns (0, nil) when the directory is missing or empty —
// traces recorded with media.save_attachments=false legitimately have
// no extracted files and the export must remain useful for them.
func writeMediaForRef(zw *zip.Writer, dataDir string, mr mediaRef) (int, error) {
	mediaDir := filepath.Join(dataDir, mr.date, mr.keyhash, "media", mr.traceID)
	entries, err := os.ReadDir(mediaDir)
	if err != nil {
		// Directory missing (most traces) or unreadable — skip silently.
		// Extraction is best-effort, not load-bearing.
		return 0, nil
	}
	// Deterministic order — os.ReadDir sorts by name on most platforms
	// but the docs do not guarantee it across the board, so we sort
	// explicitly.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	added := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		srcPath := filepath.Join(mediaDir, e.Name())
		dstName := filepath.ToSlash(filepath.Join("media", mr.traceID, e.Name()))
		if err := copyFileToZip(zw, srcPath, dstName, mr.tsStart); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

// copyFileToZip streams srcPath into the zip at dstName. mtime is used
// for the zip entry's Modified field so two exports of the same media
// produce byte-identical archives (zip stores mtime in the header).
func copyFileToZip(zw *zip.Writer, srcPath, dstName string, mtime time.Time) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	hdr := &zip.FileHeader{
		Name:     dstName,
		Method:   zip.Deflate,
		Modified: mtime,
	}
	zf, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(zf, src)
	return err
}

// writeGroupEntry reads the source JSONL file (plain or .gz fallback),
// walks it line-by-line tracking byte offsets so we can detect which
// lines match the row set, then writes one zip entry containing only the
// matching lines. The entry name picks .jsonl vs .partial.jsonl based on
// whether every line was matched.
func writeGroupEntry(zw *zip.Writer, g *fileGroup) error {
	src, plain, err := openJSONL(g.path)
	if err != nil {
		return err
	}
	defer src.Close()

	r := io.Reader(src)
	if !plain {
		gz, err := gzip.NewReader(src)
		if err != nil {
			return fmt.Errorf("open gzip %s: %w", g.path, err)
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	br := bufio.NewReaderSize(r, 64*1024)

	// First pass into a buffer: walk every line, track uncompressed offset,
	// keep matching lines.
	var matched bytes.Buffer
	var (
		offset    int64
		total     int
		matchHits int
	)
	for {
		line, err := br.ReadBytes('\n')
		lineLen := int64(len(line))
		if lineLen > 0 {
			total++
			if _, ok := g.offsets[offset]; ok {
				matched.Write(line)
				matchHits++
			}
			offset += lineLen
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", g.path, err)
		}
	}

	// Decide entry name. If every line in the source file is in our match
	// set, the export carries the complete file; otherwise it's partial.
	suffix := ".jsonl"
	if matchHits < total {
		suffix = ".partial.jsonl"
	}
	// If the match set has rows whose offset didn't show up in the file
	// (e.g. file truncated since SQLite was written), still write what we
	// have but flag as partial.
	if matchHits < len(g.offsets) {
		suffix = ".partial.jsonl"
	}
	name := fmt.Sprintf("data/%s/%s%s", g.date, g.keyhash, suffix)

	hdr := &zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: g.earliestTs,
	}
	zf, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = zf.Write(matched.Bytes())
	return err
}

// openJSONL opens path if it exists, else tries path+".gz". Returns the
// open file plus a flag indicating whether to read it as plain JSONL
// (true) or via gzip (false).
//
// We deliberately do not import internal/api/jsonlread because exporter
// must not depend on api; the logic is small enough to duplicate.
func openJSONL(path string) (*os.File, bool, error) {
	if strings.HasSuffix(path, ".gz") {
		f, err := os.Open(path)
		return f, false, err
	}
	if f, err := os.Open(path); err == nil {
		return f, true, nil
	}
	f, err := os.Open(path + ".gz")
	if err != nil {
		return nil, false, fmt.Errorf("neither %s nor %s.gz exists: %w", path, path, err)
	}
	return f, false, nil
}

// splitJSONLPath pulls the date directory and keyhash short out of a
// JSONLPath. Writer writes <dataDir>/<date>/<keyhash8>.jsonl[.gz]; if the
// path doesn't fit that shape (e.g. a rebuild dropped it elsewhere) we
// fall back to filesystem-meaningful fragments so the zip stays valid.
func splitJSONLPath(dataDir, path string) (date, keyhash string) {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".gz")
	base = strings.TrimSuffix(base, ".jsonl")
	keyhash = base

	parent := filepath.Base(filepath.Dir(path))
	date = parent

	_ = dataDir // dataDir is reserved for future absolute-path resolution.
	return date, keyhash
}

func copyEmbed(zw *zip.Writer, embedPath, zipName string) error {
	data, err := templatesFS.ReadFile(embedPath)
	if err != nil {
		return fmt.Errorf("read embed %s: %w", embedPath, err)
	}
	zf, err := zw.Create(zipName)
	if err != nil {
		return err
	}
	_, err = zf.Write(data)
	return err
}

func writeReadme(zw *zip.Writer, count, mediaCount int, filters sqlite.ListFilters, minTs, maxTs, now time.Time) error {
	zf, err := zw.Create("README.md")
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# api-log export\n\n")
	fmt.Fprintf(&b, "Generated at: %s\n\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "## Summary\n\n")
	fmt.Fprintf(&b, "- Traces matched: %d\n", count)
	if mediaCount > 0 {
		fmt.Fprintf(&b, "- Media files bundled: %d\n", mediaCount)
	}
	if count > 0 {
		fmt.Fprintf(&b, "- Time range: %s -> %s\n", minTs.Format(time.RFC3339), maxTs.Format(time.RFC3339))
	} else {
		fmt.Fprintf(&b, "- Time range: (no traces matched)\n")
	}
	fmt.Fprintf(&b, "\n## Filters applied\n\n")
	fmt.Fprintf(&b, "%s\n", summarizeFilters(filters))
	fmt.Fprintf(&b, "\n## Layout\n\n")
	fmt.Fprintf(&b, "- `data/<date>/<keyhash8>.jsonl` — every line in that day's file matched the filter.\n")
	fmt.Fprintf(&b, "- `data/<date>/<keyhash8>.partial.jsonl` — only the matching subset of that day's file is included.\n")
	if mediaCount > 0 {
		fmt.Fprintf(&b, "- `media/<trace_id>/<idx>.<ext>` — attachments (images, PDFs, audio) extracted from the matching traces; names match the per-trace ordinal recorded in SQLite.\n")
	}
	fmt.Fprintf(&b, "- `agent/CLAUDE.md` — instructions for an offline agent analyzing this export.\n")
	fmt.Fprintf(&b, "- `agent/jq-cheatsheet.md` — copy-paste jq recipes for common questions.\n")
	fmt.Fprintf(&b, "\n## Quick start\n\n")
	fmt.Fprintf(&b, "Each line in `data/**/*.jsonl` is one HTTP transaction. Inspect with jq:\n\n")
	fmt.Fprintf(&b, "    jq 'select(.status >= 500)' data/**/*.jsonl\n\n")
	fmt.Fprintf(&b, "See `agent/CLAUDE.md` for the full schema and `agent/jq-cheatsheet.md` for more recipes.\n")

	_, err = zf.Write([]byte(b.String()))
	return err
}

// summarizeFilters returns a human-readable bullet list of the filter set
// for the README. Zero-valued fields are skipped so the empty-filter case
// shows "(none — all traces up to safety cap)".
func summarizeFilters(f sqlite.ListFilters) string {
	var lines []string
	if !f.Since.IsZero() {
		lines = append(lines, "- since: "+f.Since.UTC().Format(time.RFC3339))
	}
	if !f.Until.IsZero() {
		lines = append(lines, "- until: "+f.Until.UTC().Format(time.RFC3339))
	}
	if f.Status != nil {
		lines = append(lines, fmt.Sprintf("- status: %d", *f.Status))
	}
	if f.StatusBucket > 0 {
		lines = append(lines, fmt.Sprintf("- status_bucket: %dxx", f.StatusBucket))
	}
	if f.Model != "" {
		lines = append(lines, "- model: "+f.Model)
	}
	if f.Path != "" {
		lines = append(lines, "- path: "+f.Path)
	}
	if f.PathPrefix != "" {
		lines = append(lines, "- path_prefix: "+f.PathPrefix+"*")
	}
	if f.KeyHashPrefix != "" {
		lines = append(lines, "- key_hash: "+f.KeyHashPrefix)
	}
	if f.SessionRootID != "" {
		lines = append(lines, "- session_root_id: "+f.SessionRootID)
	}
	if f.Project != "" {
		lines = append(lines, "- project: "+f.Project)
	}
	if len(lines) == 0 {
		return "(none — all traces)"
	}
	return strings.Join(lines, "\n")
}
