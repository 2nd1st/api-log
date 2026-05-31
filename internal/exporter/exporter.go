// Package exporter builds the /api/export zip per PHILOSOPHY § 4
// (compose, don't absorb) — it ships JSONL bytes straight from disk into
// a streaming zip, no transformation. The SQLite store is consulted only
// to decide *which* JSONL lines to ship (filesystem is truth; SQLite is
// the derived index, principle 6).
//
// The zip layout matches uiux-research/phase-i-export-contract.md and
// uiux-research/phase-k-media-contract.md § 9:
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
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xiayangzhang/api-log/internal/store/sqlite"
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

// WriteZip streams a zip archive of every row matched by filters into w.
//
// dataDir is the storage root that JSONL paths in the SQLite rows are
// relative to (or absolute; we accept either — the row's JSONLPath is
// the authoritative reader-side path).
//
// limit optionally bounds how many rows we read from SQLite. Pass 0 (or
// any non-positive value) to export every matching row — there is no
// fixed upper bound; the export streams so memory is not the bottleneck
// and SQLite handles 100k+ rows comfortably.
//
// The returned error means either the SQLite read failed (zip not yet
// started) or a disk read failed mid-stream (zip may be partial). The
// caller is responsible for setting HTTP headers BEFORE calling — once
// bytes start flowing to w we can no longer set a status code.
func WriteZip(w io.Writer, store *sqlite.Store, dataDir string, filters sqlite.ListFilters, limit int) error {
	rows, err := store.AllMatching(filters, limit)
	if err != nil {
		return fmt.Errorf("query matching rows: %w", err)
	}

	zw := zip.NewWriter(w)
	defer zw.Close()

	// Group rows by their JSONLPath. Each group will become one zip entry.
	// Track match-set as a set of jsonl_offset values so the per-file pass
	// can decide complete-vs-partial without re-parsing JSON.
	groups := make(map[string]*fileGroup)
	for _, r := range rows {
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
	}

	// Deterministic emission order: by date then keyhash. Keeps two
	// identical exports byte-identical.
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

	// Track range for the README.
	var (
		minTs time.Time
		maxTs time.Time
	)
	for _, r := range rows {
		if minTs.IsZero() || r.TsStart.Before(minTs) {
			minTs = r.TsStart
		}
		if r.TsStart.After(maxTs) {
			maxTs = r.TsStart
		}
	}

	for _, k := range keys {
		g := groups[k]
		if err := writeGroupEntry(zw, g); err != nil {
			return fmt.Errorf("write entry for %s: %w", g.path, err)
		}
	}

	// Bundle extracted media files alongside the JSONL (phase-k contract
	// § 9). Per backend PHILOSOPHY § 1 the extractor runs at finalize time
	// and the files on disk are the source of truth here — exporter only
	// copies. Failure to read a media dir is silent on purpose: traces
	// recorded with save_attachments=false legitimately have no dir, and a
	// missing dir must not abort the export of the JSONL that *is* there.
	// Iterate rows in deterministic order (sorted trace ID) so two
	// identical exports stay byte-identical.
	rowsSorted := make([]sqlite.Row, len(rows))
	copy(rowsSorted, rows)
	sort.Slice(rowsSorted, func(i, j int) bool {
		return rowsSorted[i].ID < rowsSorted[j].ID
	})
	mediaIncluded := 0
	for _, r := range rowsSorted {
		n, err := writeMediaForTrace(zw, dataDir, r)
		if err != nil {
			return fmt.Errorf("bundle media for %s: %w", r.ID, err)
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

	// README — human-readable summary.
	if err := writeReadme(zw, len(rows), mediaIncluded, filters, minTs, maxTs, time.Now().UTC()); err != nil {
		return err
	}
	return nil
}

// writeMediaForTrace copies every file under
// `${dataDir}/${date}/${keyhash8}/media/${trace_id}/` into the zip at
// `media/${trace_id}/${name}`. Returns the number of files added.
//
// date and keyhash8 are derived from the row's JSONLPath via the same
// rule writeGroupEntry uses, so the exporter never invents a path that
// doesn't match what the writer chose at finalize time.
//
// Silently returns (0, nil) when the directory is missing or empty —
// traces recorded with media.save_attachments=false legitimately have
// no extracted files and the export must remain useful for them.
func writeMediaForTrace(zw *zip.Writer, dataDir string, r sqlite.Row) (int, error) {
	date, keyhash8 := splitJSONLPath(dataDir, r.JSONLPath)
	mediaDir := filepath.Join(dataDir, date, keyhash8, "media", r.ID)
	entries, err := os.ReadDir(mediaDir)
	if err != nil {
		// Directory missing (most traces) or unreadable — skip silently.
		// PHILOSOPHY § 2: extraction is best-effort, not load-bearing.
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
		dstName := filepath.ToSlash(filepath.Join("media", r.ID, e.Name()))
		if err := copyFileToZip(zw, srcPath, dstName, r.TsStart); err != nil {
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
		defer gz.Close()
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
	if len(lines) == 0 {
		return "(none — all traces)"
	}
	return strings.Join(lines, "\n")
}
