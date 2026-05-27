package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned by GetByID when no row matches the given id.
var ErrNotFound = errors.New("trace not found")

// ListFilters bounds and filters /api/traces queries per ARCHITECTURE § 6.2.
// All fields are optional; zero values mean "no constraint".
type ListFilters struct {
	Since         time.Time // ts_start >= Since (zero = no bound)
	Until         time.Time // ts_start <  Until (zero = no bound)
	Status        *int      // exact match if set
	Model         string    // exact match if set
	KeyHashPrefix string    // accepts 8- or 16-char prefix; matches by LIKE
	SessionRootID string    // exact match if set
	Limit         int       // 1..500; <=0 means default 100

	// Cursor is opaque base64 of "<ts_start_ms>:<id>"; rows strictly older
	// than the cursor (ts_start DESC, id DESC ordering) are returned.
	// Empty = first page.
	CursorTsStart int64
	CursorID      string
}

// ListPage is the paginated result.
type ListPage struct {
	Rows            []Row
	NextCursorMs    int64  // 0 if no next page
	NextCursorID    string // empty if no next page
}

// List returns at most filters.Limit (default 100, max 500) rows
// ordered by (ts_start DESC, id DESC) that match the filters.
//
// Always orders by ts_start DESC for cursor stability. To page, pass
// the previous page's NextCursorMs / NextCursorID back as
// filters.CursorTsStart / filters.CursorID.
func (s *Store) List(filters ListFilters) (ListPage, error) {
	limit := filters.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

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
	if filters.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, filters.Model)
	}
	if filters.KeyHashPrefix != "" {
		conds = append(conds, "key_hash LIKE ?")
		args = append(args, filters.KeyHashPrefix+"%")
	}
	if filters.SessionRootID != "" {
		conds = append(conds, "session_root_id = ?")
		args = append(args, filters.SessionRootID)
	}
	if filters.CursorTsStart > 0 || filters.CursorID != "" {
		// Strict (ts_start, id) ordering — keyset pagination.
		conds = append(conds,
			"(ts_start < ? OR (ts_start = ? AND id < ?))")
		args = append(args,
			filters.CursorTsStart, filters.CursorTsStart, filters.CursorID)
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	q := fmt.Sprintf(`
		SELECT %s
		FROM traces
		%s
		ORDER BY ts_start DESC, id DESC
		LIMIT ?
	`, selectColumns, where)
	args = append(args, limit+1) // fetch one extra to detect "more available"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return ListPage{}, fmt.Errorf("list query: %w", err)
	}
	defer rows.Close()

	out := make([]Row, 0, limit)
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return ListPage{}, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return ListPage{}, err
	}

	page := ListPage{Rows: out}
	if len(out) > limit {
		// Trim the extra and record the cursor for the next page.
		last := out[limit-1]
		page.Rows = out[:limit]
		page.NextCursorMs = unixMs(last.TsStart)
		page.NextCursorID = last.ID
	}
	return page, nil
}

// GetByID returns the row with the given trace ID. Returns ErrNotFound
// when no such row exists.
func (s *Store) GetByID(id string) (Row, error) {
	q := fmt.Sprintf("SELECT %s FROM traces WHERE id = ?", selectColumns)
	row := s.db.QueryRow(q, id)
	r, err := scanRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Row{}, ErrNotFound
		}
		return Row{}, err
	}
	return r, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

const selectColumns = `
	id, ts_start, ts_end, client, method, path, upstream, status,
	disconnected, truncated_req, truncated_resp,
	model, stream, prompt_tokens, completion_tokens, total_tokens, finish_reason,
	key_hash, prefix_len, prefix_canonical_hash,
	parent_id, session_root_id,
	jsonl_path, jsonl_offset
`

func scanRow(rs rowScanner) (Row, error) {
	var (
		r              Row
		tsStartMs      int64
		tsEndMs        int64
		disc           int
		truncReq       int
		truncResp      int
		model          sql.NullString
		stream         sql.NullInt64
		promptTokens   sql.NullInt64
		completionToks sql.NullInt64
		totalTokens    sql.NullInt64
		finishReason   sql.NullString
		prefixLen      sql.NullInt64
		prefixHash     sql.NullString
		parentID       sql.NullString
	)
	err := rs.Scan(
		&r.ID, &tsStartMs, &tsEndMs, &r.Client, &r.Method, &r.Path, &r.Upstream, &r.Status,
		&disc, &truncReq, &truncResp,
		&model, &stream, &promptTokens, &completionToks, &totalTokens, &finishReason,
		&r.KeyHash, &prefixLen, &prefixHash,
		&parentID, &r.SessionRootID,
		&r.JSONLPath, &r.JSONLOffset,
	)
	if err != nil {
		return Row{}, err
	}
	r.TsStart = time.UnixMilli(tsStartMs).UTC()
	r.TsEnd = time.UnixMilli(tsEndMs).UTC()
	r.Disconnected = disc != 0
	r.TruncatedReq = truncReq != 0
	r.TruncatedResp = truncResp != 0
	if model.Valid {
		v := model.String
		r.Model = &v
	}
	if stream.Valid {
		b := stream.Int64 != 0
		r.Stream = &b
	}
	if promptTokens.Valid {
		v := promptTokens.Int64
		r.PromptTokens = &v
	}
	if completionToks.Valid {
		v := completionToks.Int64
		r.CompletionTokens = &v
	}
	if totalTokens.Valid {
		v := totalTokens.Int64
		r.TotalTokens = &v
	}
	if finishReason.Valid {
		v := finishReason.String
		r.FinishReason = &v
	}
	if prefixLen.Valid {
		v := int(prefixLen.Int64)
		r.PrefixLen = &v
	}
	if prefixHash.Valid {
		v := prefixHash.String
		r.PrefixCanonicalHash = &v
	}
	if parentID.Valid {
		v := parentID.String
		r.ParentID = &v
	}
	return r, nil
}
