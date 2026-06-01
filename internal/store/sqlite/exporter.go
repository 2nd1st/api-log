package sqlite

import (
	"context"
	"fmt"
	"strings"
)

// CountMatching returns the number of rows that match filters, capped
// at hardCap+1. The subquery pattern (`SELECT COUNT(*) FROM (SELECT 1
// ... LIMIT ?)`) lets SQLite stop counting once the cap is reached
// rather than scanning every matching row — important for the export
// pre-flight check on multi-million-row stores.
//
// Pass hardCap = 0 (or any non-positive value) to count without a cap.
// Callers (api/export.go) use the result to short-circuit oversized
// exports with a 413 BEFORE any zip bytes hit the wire. Returns the
// raw count; callers compare against their own cap.
//
// Reuses buildListConds so the predicate stays in lockstep with List
// and StreamMatching — adding a new ListFilters field only requires
// editing buildListConds.
func (s *Store) CountMatching(ctx context.Context, filters ListFilters, hardCap int) (int, error) {
	conds, args := buildListConds(filters)
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	limitClause := ""
	if hardCap > 0 {
		// hardCap+1 so callers can detect "more than hardCap" without
		// false equality at the boundary. Caller compares: cnt > hardCap.
		limitClause = "LIMIT ?"
		args = append(args, hardCap+1)
	}

	q := fmt.Sprintf(`
		SELECT COUNT(*) FROM (
			SELECT 1 FROM traces
			%s
			%s
		)
	`, where, limitClause)

	var n int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count-matching query: %w", err)
	}
	return n, nil
}

// StreamMatching iterates every row matching filters in the same order
// AllMatching returns them (ts_start ASC, id ASC), invoking visit on
// each row. If visit returns an error, iteration stops and that error
// is returned to the caller. Used by the streaming export path so the
// exporter never has to hold the full result set in memory.
//
// One database connection is borrowed via db.Conn(ctx) for the entire
// cursor lifetime — the WAL snapshot stays consistent across all
// visit calls without blocking the writer. The conn is released on
// return so the standard pool can serve subsequent queries.
//
// No internal LIMIT: callers gate oversize exports via CountMatching
// BEFORE calling StreamMatching. The cursor itself is unbounded so
// adopters opting into `?all=1` see every row.
//
// Context propagation: a canceled ctx surfaces as the visit boundary
// (next QueryContext call returns ctx.Err()) — partial-write callers
// (the zip exporter) recover from that mid-stream.
func (s *Store) StreamMatching(ctx context.Context, filters ListFilters, visit func(Row) error) error {
	conds, args := buildListConds(filters)
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	q := fmt.Sprintf(`
		SELECT %s
		FROM traces
		%s
		ORDER BY ts_start ASC, id ASC
	`, selectColumns, where)

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("borrow stream conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("stream query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, err := scanRow(rows)
		if err != nil {
			return err
		}
		if err := visit(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// AllMatching returns every row matching filters, ordered chronologically
// (ts_start ASC, id ASC for a deterministic tiebreak).
//
// Unlike List, this method ignores filters.Limit (the per-page cap) and
// any cursor fields, and orders ASC so callers (the exporter) can stream
// results into a reproducible zip. The hardCap argument optionally bounds
// the total rows fetched; pass 0 (or any non-positive value) for
// unlimited — the export path uses this. The cap parameter is kept on the
// signature for future callers that want a guard (rate-limit, preview).
//
// The WHERE-clause is built from the same fields as List (status, status
// bucket, model, path/prefix, key_hash prefix, session_root_id, since,
// until) — keep them in sync if List grows new filters.
func (s *Store) AllMatching(filters ListFilters, hardCap int) ([]Row, error) {
	conds, args := buildListConds(filters)
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	limitClause := ""
	if hardCap > 0 {
		limitClause = "LIMIT ?"
		args = append(args, hardCap)
	}

	q := fmt.Sprintf(`
		SELECT %s
		FROM traces
		%s
		ORDER BY ts_start ASC, id ASC
		%s
	`, selectColumns, where, limitClause)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("all-matching query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Row, 0, 128)
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// buildListConds returns the non-cursor WHERE conditions and bound args
// for a ListFilters. Extracted so List and AllMatching share one
// definition; cursor logic stays in List.
func buildListConds(filters ListFilters) ([]string, []any) {
	var (
		conds []string
		args  []any
	)
	if !filters.Since.IsZero() {
		conds = append(conds, "ts_start >= ?")
		args = append(args, unixMs(filters.Since))
	}
	if !filters.Until.IsZero() {
		conds = append(conds, "ts_start < ?")
		args = append(args, unixMs(filters.Until))
	}
	if filters.Status != nil {
		conds = append(conds, "status = ?")
		args = append(args, *filters.Status)
	}
	if filters.StatusBucket >= 2 && filters.StatusBucket <= 5 {
		lo := filters.StatusBucket * 100
		conds = append(conds, "status >= ? AND status < ?")
		args = append(args, lo, lo+100)
	}
	if filters.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, filters.Model)
	}
	if filters.Path != "" {
		conds = append(conds, "path = ?")
		args = append(args, filters.Path)
	} else if filters.PathPrefix != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(filters.PathPrefix)
		conds = append(conds, `path LIKE ? ESCAPE '\'`)
		args = append(args, esc+"%")
	}
	if filters.KeyHashPrefix != "" {
		conds = append(conds, "key_hash LIKE ?")
		args = append(args, filters.KeyHashPrefix+"%")
	}
	if filters.SessionRootID != "" {
		conds = append(conds, "session_root_id = ?")
		args = append(args, filters.SessionRootID)
	}
	if filters.Project != "" {
		// Exact match — operators pass a known project name (no glob),
		// so a column equality keeps the SQLite plan trivial. Empty
		// filter (the default) skips this branch entirely.
		conds = append(conds, "client_project = ?")
		args = append(args, filters.Project)
	}
	return conds, args
}
