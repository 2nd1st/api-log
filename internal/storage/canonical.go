// Package storage owns the coordination layer between writer,
// exporter, retention, and the read API for on-disk JSONL files.
//
// The package introduces three load-bearing abstractions:
//
//   - FileID — canonical identity of a (date, key_hash) bucket.
//     The same trace file can exist on disk as either `<keyhash>.jsonl`
//     (active or post-rotation but pre-compression) or `<keyhash>.jsonl.gz`
//     (post-compression). The SQLite jsonl_path column ALWAYS stores
//     the canonical `.jsonl` form; FileID is the in-process value
//     that maps between the two without baking the extension into
//     callers.
//
//   - Lease (lease.go) — refcount-based hold preventing eviction
//     from removing a file while a reader or writer still has it open.
//
//   - Coordinator (coordinator.go) — the always-on monitor that owns
//     inventory, status, leases, and (when knobs are set) eviction.
//
// This file declares the FileID type and the parsing helpers. The
// types declared here have no goroutines, no I/O lifecycle, and no
// shared state — they are pure value types.
package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// FileID identifies one trace-storage bucket: the per-(date, key_hash)
// file the writer rotates into. A FileID has a deterministic mapping
// to one canonical SQLite path and to either or both filesystem forms
// (`.jsonl` plain, `.jsonl.gz` compressed). It is the single source
// of identity across writer / exporter / retention / read API.
type FileID struct {
	// DataDir is the absolute root configured via storage.data_dir.
	// Kept on FileID so MediaSubtree, CanonicalPath, and FSExists
	// don't need it threaded as a separate argument.
	DataDir string

	// Date is the UTC date the writer used at bucket-creation time,
	// formatted as YYYY-MM-DD. Source-of-truth is the JSONL file's
	// parent directory name, not the trace's TsStart (those can
	// differ when finalize crosses a day boundary).
	Date string

	// KeyHash8 is the first 8 hex characters of the SHA-256 of the
	// client's Authorization / x-api-key, computed by
	// internal/ids.KeyHashShort. 8 hex chars ≈ 32 bits, sufficient
	// to keep per-tenant files separate at every realistic scale
	// (collision probability is ~1.2e-5 at 1000 tenants).
	KeyHash8 string
}

// dateRe matches YYYY-MM-DD; the writer guarantees this exact format
// via t.TsStart.UTC().Format("2006-01-02"), so a stricter check than
// "non-empty" rejects accidentally-mangled paths early.
var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// keyHashRe matches exactly 8 lowercase hex characters.
// ids.KeyHashShort always emits lowercase hex of length 8.
var keyHashRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// CanonicalPath returns the canonical filesystem form of this bucket
// — the `.jsonl` (uncompressed) path. SQLite's jsonl_path column
// always stores this exact string. The actual on-disk file may be
// the `.jsonl` form (active) or the `.jsonl.gz` form (compressed),
// or both transiently during rotation; use FSExists to determine
// the current on-disk reality.
func (f FileID) CanonicalPath() string {
	return filepath.Join(f.DataDir, f.Date, f.KeyHash8+".jsonl")
}

// gzPath is an internal helper returning the compressed-form path.
// Exposed only via FSExists so callers don't need to know about the
// `.gz` suffix anywhere else.
func (f FileID) gzPath() string {
	return f.CanonicalPath() + ".gz"
}

// MediaSubtree returns the absolute path to this bucket's media
// directory: `<DataDir>/<Date>/<KeyHash8>/media/`. Extraction in
// internal/media writes files under this tree as
// `media/<trace_id>/<idx>.<ext>`; retention deletes the whole
// subtree alongside the JSONL file.
func (f FileID) MediaSubtree() string {
	return filepath.Join(f.DataDir, f.Date, f.KeyHash8, "media")
}

// FSExists returns the actual on-disk form of this bucket, if any.
//
// Returns:
//   - (path, nil) when exactly one form exists (either `.jsonl` or
//     `.jsonl.gz`). Plain wins over `.gz` when both exist transiently
//     during rotation: compressInPlace writes `.gz` first, then
//     removes the plain file, so seeing both means rotation is
//     mid-step and the plain form is still authoritative.
//   - ("", nil) when neither form exists. This is the "already gone"
//     signal that callers like deleteIfIdle should treat as success.
//   - ("", err) when stat returns an error OTHER than fs.ErrNotExist.
//     Callers MUST distinguish — treating permission / I/O errors as
//     "gone" would silently delete SQLite rows for files we just
//     couldn't read.
func (f FileID) FSExists() (string, error) {
	plain := f.CanonicalPath()
	if _, err := os.Stat(plain); err == nil {
		return plain, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", plain, err)
	}
	gz := f.gzPath()
	if _, err := os.Stat(gz); err == nil {
		return gz, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", gz, err)
	}
	return "", nil
}

// FileIDFromPath parses a canonical jsonl_path (the SQLite form) back
// into a FileID. The path may be the plain `.jsonl` form OR the
// compressed `.jsonl.gz` form — both produce the same FileID because
// the suffix is metadata, not identity.
//
// Returns an error when:
//   - path doesn't sit under dataDir (a defense against accidentally
//     calling this with a path from a different deployment)
//   - the relative path doesn't match `<date>/<keyhash8>.jsonl{,.gz}`
//   - date doesn't parse as YYYY-MM-DD
//   - keyhash isn't 8 lowercase hex characters
//
// Used by:
//   - retention/reconcile when reading SQLite jsonl_path rows
//   - the background gzip worker (compressInPlace) to acquire a lease
//   - tests + tooling that need to round-trip a path
func FileIDFromPath(dataDir, path string) (FileID, error) {
	if dataDir == "" {
		return FileID{}, errors.New("FileIDFromPath: empty dataDir")
	}
	// Clean both sides so trailing slashes / dot segments don't make
	// the prefix check fail spuriously.
	cleanData := filepath.Clean(dataDir)
	cleanPath := filepath.Clean(path)
	rel, err := filepath.Rel(cleanData, cleanPath)
	if err != nil {
		return FileID{}, fmt.Errorf("FileIDFromPath: rel %s vs %s: %w", path, dataDir, err)
	}
	// filepath.Rel returns "../..." when path isn't under dataDir;
	// reject those.
	if len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return FileID{}, fmt.Errorf("FileIDFromPath: path %s not under dataDir %s", path, dataDir)
	}
	// Expect exactly two path components: <date>/<keyhash8>.jsonl{,.gz}
	dir, base := filepath.Split(rel)
	date := filepath.Clean(dir) // strips trailing slash
	if !dateRe.MatchString(date) {
		return FileID{}, fmt.Errorf("FileIDFromPath: date segment %q does not match YYYY-MM-DD", date)
	}
	// Strip the suffix. Order matters: .jsonl.gz first, then .jsonl.
	keyHash := base
	switch {
	case len(base) > len(".jsonl.gz") && base[len(base)-len(".jsonl.gz"):] == ".jsonl.gz":
		keyHash = base[:len(base)-len(".jsonl.gz")]
	case len(base) > len(".jsonl") && base[len(base)-len(".jsonl"):] == ".jsonl":
		keyHash = base[:len(base)-len(".jsonl")]
	default:
		return FileID{}, fmt.Errorf("FileIDFromPath: base %q has no .jsonl or .jsonl.gz suffix", base)
	}
	if !keyHashRe.MatchString(keyHash) {
		return FileID{}, fmt.Errorf("FileIDFromPath: key_hash segment %q is not 8 lowercase hex characters", keyHash)
	}
	return FileID{
		DataDir:  cleanData,
		Date:     date,
		KeyHash8: keyHash,
	}, nil
}
