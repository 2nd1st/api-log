package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileInfo describes one trace-storage bucket — its on-disk size,
// modification time, and media subtree size. Produced by walkAndStat
// and consumed by status computation, retention eviction, and the
// orphan reconciliation tick.
//
// FSPath is informational: when both `.jsonl` and `.jsonl.gz` exist
// transiently during rotation, walkAndStat records the latest form
// seen. Callers that need the live filesystem form at action time
// (e.g. eviction's os.Remove) should call FileID.FSExists() rather
// than trusting a possibly-stale FSPath.
type FileInfo struct {
	FileID    FileID
	FSPath    string    // most recently scanned on-disk form
	SizeBytes int64     // total across .jsonl + .jsonl.gz (sums during rotation)
	ModTime   time.Time // latest mtime across forms; zero if no JSONL on disk
	MediaSize int64     // recursive bytes under MediaSubtree()
}

// Inventory is the snapshot walkAndStat returns. Files is unsorted —
// callers (eviction sorts by mtime ASC, status sums) decide their
// own ordering.
type Inventory struct {
	Files      []FileInfo
	TotalBytes int64 // sum of SizeBytes + MediaSize over Files (the retention-managed footprint)
}

// walkAndStat walks dataDir and returns an Inventory of trace
// buckets. Costs one stat() per file via DirEntry.Info(). The walk
// respects ctx cancellation — checked at every entry.
//
// Excluded from the inventory (do NOT count against retention):
//   - top-level files: admin_token, index.sqlite, runtime_overrides.json
//   - SQLite sidecars: *-wal, *-shm anywhere
//   - the entire tmp/ directory (in-flight finalize state)
//   - paths that don't match `<date>/<keyhash8>.jsonl{,.gz}` or
//     `<date>/<keyhash8>/media/...`
//
// The rationale for excluding the SQLite + tmp surfaces is documented
// in §2 of data-volume-roadmap.md: max_bytes governs the
// retention-managed footprint (JSONL + media), not total disk usage.
// Adopters who want the total can `du -sb data/` separately.
func walkAndStat(ctx context.Context, dataDir string) (Inventory, error) {
	if dataDir == "" {
		return Inventory{}, errors.New("walkAndStat: empty dataDir")
	}
	clean := filepath.Clean(dataDir)

	// Stat the root explicitly — filepath.WalkDir silently passes the
	// error to the walkFn (where we skip it), which would swallow a
	// missing / unreadable dataDir into an empty inventory. That's a
	// config bug operators should see at startup, not lose to silent
	// success.
	if st, err := os.Stat(clean); err != nil {
		return Inventory{}, fmt.Errorf("walkAndStat stat %s: %w", clean, err)
	} else if !st.IsDir() {
		return Inventory{}, fmt.Errorf("walkAndStat: %s is not a directory", clean)
	}

	byFileID := map[FileID]*FileInfo{}

	err := filepath.WalkDir(clean, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// Permission / transient errors on a single entry shouldn't
			// abort the whole walk — return SkipDir if it's a directory,
			// or just continue past the entry.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(clean, path)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}

		// Skip the tmp/ subtree entirely.
		if d.IsDir() {
			if rel == "tmp" {
				return fs.SkipDir
			}
			return nil
		}

		// Skip the non-data files we know about at any depth.
		base := filepath.Base(rel)
		switch base {
		case "admin_token", "index.sqlite", "runtime_overrides.json":
			return nil
		}
		if strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm") {
			return nil
		}

		// JSONL or .jsonl.gz file: <date>/<keyhash8>.jsonl{,.gz}
		if strings.HasSuffix(base, ".jsonl") || strings.HasSuffix(base, ".jsonl.gz") {
			fid, err := FileIDFromPath(clean, path)
			if err != nil {
				// Not our shape (e.g. someone dropped a stray .jsonl).
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			fi := getOrInitFileInfo(byFileID, fid)
			fi.SizeBytes += info.Size()
			if mt := info.ModTime(); mt.After(fi.ModTime) {
				fi.ModTime = mt
			}
			fi.FSPath = path
			return nil
		}

		// Media file: <date>/<keyhash8>/media/<trace_id>/<idx>.<ext>
		// Path parts: [date, keyhash8, "media", trace_id, file.ext]
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) >= 4 && parts[2] == "media" {
			date := parts[0]
			keyHash := parts[1]
			if !dateRe.MatchString(date) || !keyHashRe.MatchString(keyHash) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			fid := FileID{DataDir: clean, Date: date, KeyHash8: keyHash}
			fi := getOrInitFileInfo(byFileID, fid)
			fi.MediaSize += info.Size()
			return nil
		}

		// Anything else under data/ — silently ignore.
		return nil
	})
	if err != nil {
		return Inventory{}, fmt.Errorf("walkAndStat %s: %w", clean, err)
	}

	out := Inventory{Files: make([]FileInfo, 0, len(byFileID))}
	for _, fi := range byFileID {
		out.Files = append(out.Files, *fi)
		out.TotalBytes += fi.SizeBytes + fi.MediaSize
	}
	return out, nil
}

func getOrInitFileInfo(m map[FileID]*FileInfo, fid FileID) *FileInfo {
	if fi, ok := m[fid]; ok {
		return fi
	}
	fi := &FileInfo{FileID: fid}
	m[fid] = fi
	return fi
}
