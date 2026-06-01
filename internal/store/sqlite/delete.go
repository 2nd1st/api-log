package sqlite

import (
	"context"
	"fmt"
)

// DeleteByJSONLPath removes every traces row whose jsonl_path column
// equals canonicalPath. Returns the affected row count.
//
// Called by the storage coordinator's eviction sequence (see
// internal/storage/eviction.go.deleteIfIdle) after the on-disk JSONL +
// media subtree have been removed. Backed by idx_jsonl_path so the
// DELETE plan is a single index seek even with millions of rows.
//
// Context-aware: a canceled ctx surfaces as an error from ExecContext
// and the caller (deleteIfIdle) returns it up the stack to the monitor
// tick, which logs and continues. Zero rows affected is NOT an error —
// it's the "already cleaned up by a previous partial-failure tick"
// case; deleteIfIdle's defensive-delete model assumes this can happen.
func (s *Store) DeleteByJSONLPath(ctx context.Context, canonicalPath string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM traces WHERE jsonl_path = ?`, canonicalPath)
	if err != nil {
		return 0, fmt.Errorf("delete by jsonl_path %q: %w", canonicalPath, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// ExecContext's driver promises RowsAffected for modernc.org/sqlite,
		// but be defensive — surface the rare driver error rather than
		// silently returning 0.
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// ListDistinctJSONLPaths returns every distinct jsonl_path stored in
// the traces table, ordered ASC for deterministic test output. With
// idx_jsonl_path SQLite can satisfy the DISTINCT scan from the index
// alone — no table-row visits.
//
// Used by the storage coordinator's reconcileOrphans path to find rows
// referencing files that no longer exist on disk (post-crash gap
// between os.Remove + DELETE). Returned slice is safe for the caller
// to mutate; nothing else in the package shares it.
func (s *Store) ListDistinctJSONLPaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT jsonl_path FROM traces ORDER BY jsonl_path`)
	if err != nil {
		return nil, fmt.Errorf("list distinct jsonl_path: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan jsonl_path: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
