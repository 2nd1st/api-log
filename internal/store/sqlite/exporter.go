package sqlite

import (
	"fmt"
	"strings"
)

// AllMatching returns every row matching filters, ordered chronologically
// (ts_start ASC, id ASC for a deterministic tiebreak), with a hard cap.
//
// Unlike List, this method ignores filters.Limit (the per-page cap) and
// any cursor fields, and orders ASC so callers (the exporter) can stream
// results into a reproducible zip. The hardCap argument bounds the total
// rows fetched; pass 5000 for the /api/export safety cap.
//
// The WHERE-clause is built from the same fields as List (status, status
// bucket, model, path/prefix, key_hash prefix, session_root_id, since,
// until) — keep them in sync if List grows new filters.
func (s *Store) AllMatching(filters ListFilters, hardCap int) ([]Row, error) {
	if hardCap <= 0 {
		hardCap = 5000
	}

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
		LIMIT ?
	`, selectColumns, where)
	args = append(args, hardCap)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("all-matching query: %w", err)
	}
	defer rows.Close()

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
	return conds, args
}
