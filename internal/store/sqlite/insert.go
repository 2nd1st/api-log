package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leoyun/api-log/internal/session"
	"github.com/leoyun/api-log/internal/trace"
)

// Row is the Go-side projection of one traces row. Session-inference
// columns (PrefixLen, PrefixCanonicalHash, ParentID, SessionRootID)
// are filled by AppendTrace, not by the caller — pass them empty.
type Row struct {
	ID            string
	TsStart       time.Time
	TsEnd         time.Time
	Client        string
	Method        string
	Path          string
	Upstream      string
	Status        int

	Disconnected  bool
	TruncatedReq  bool
	TruncatedResp bool

	// Protocol field copies (may be NULL if absent). Populated from
	// trace.Trace by the writer; pass nil for unknown.
	Model            *string
	Stream           *bool
	PromptTokens     *int64
	CompletionTokens *int64
	TotalTokens      *int64
	FinishReason     *string

	// Identifiers.
	KeyHash string

	// JSONL location.
	JSONLPath   string
	JSONLOffset int64
}

// FromTrace builds a Row from a trace.Trace plus the writer's known
// extras. Session columns are left to AppendTrace; derived columns
// (model, tokens, finish_reason) are left nil — caller fills them.
func FromTrace(t trace.Trace, keyHash, jsonlPath string, jsonlOffset int64) Row {
	return Row{
		ID:            t.ID,
		TsStart:       t.TsStart,
		TsEnd:         t.TsEnd,
		Client:        t.Client,
		Method:        t.Method,
		Path:          t.Path,
		Upstream:      t.Upstream,
		Status:        t.Status,
		Disconnected:  t.Disconnected,
		TruncatedReq:  t.TruncatedReq,
		TruncatedResp: t.TruncatedResp,
		KeyHash:       keyHash,
		JSONLPath:     jsonlPath,
		JSONLOffset:   jsonlOffset,
	}
}

// AppendTrace inserts a row and runs session inference atomically.
//
// sessionPrefix is the result of session.Build(path, req.body). Pass
// nil if the trace has no session concept (embeddings, image gen, etc.);
// the row's session_root_id defaults to id (self-root) and parent_id
// stays NULL.
//
// Everything happens in one transaction: this gives us one fsync per
// trace (instead of two for INSERT + UPDATE) and atomic session-inference
// reads. On commit failure the row is not inserted; the caller's JSONL
// line is still on disk and will be picked up by startup rebuild.
func (s *Store) AppendTrace(r Row, sessionPrefix []json.RawMessage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	// Session inference inside the transaction.
	var (
		prefixLen           *int
		prefixCanonicalHash *string
		parentID            *string
		sessionRootID       = r.ID // default: self-root
	)
	if len(sessionPrefix) > 0 {
		plen := len(sessionPrefix)
		prefixLen = &plen
		h := session.HashHex(sessionPrefix)
		prefixCanonicalHash = &h

		strict := session.StrictPrefixHashes(sessionPrefix)
		if len(strict) > 0 {
			pid, root, _, ok, err := findParentTx(tx, r.KeyHash, strict)
			if err != nil {
				return err
			}
			if ok {
				parentID = &pid
				sessionRootID = root
			}
		}
	}

	const q = `INSERT OR REPLACE INTO traces (
		id, ts_start, ts_end, client, method, path, upstream, status,
		disconnected, truncated_req, truncated_resp,
		model, stream, prompt_tokens, completion_tokens, total_tokens, finish_reason,
		key_hash, prefix_len, prefix_canonical_hash,
		parent_id, session_root_id,
		jsonl_path, jsonl_offset
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?,
		?, ?, ?, ?, ?, ?,
		?, ?, ?,
		?, ?,
		?, ?
	)`
	_, err = tx.Exec(q,
		r.ID, unixMs(r.TsStart), unixMs(r.TsEnd), r.Client, r.Method, r.Path, r.Upstream, r.Status,
		boolToInt(r.Disconnected), boolToInt(r.TruncatedReq), boolToInt(r.TruncatedResp),
		nullStr(r.Model), nullBoolToInt(r.Stream), nullInt64(r.PromptTokens), nullInt64(r.CompletionTokens), nullInt64(r.TotalTokens), nullStr(r.FinishReason),
		r.KeyHash, nullInt(prefixLen), nullStr(prefixCanonicalHash),
		nullStr(parentID), sessionRootID,
		r.JSONLPath, r.JSONLOffset,
	)
	if err != nil {
		return fmt.Errorf("insert %s: %w", r.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", r.ID, err)
	}
	rollback = false
	return nil
}

// findParentTx runs the IN-clause query inside an existing transaction.
// Same semantics as Store.FindParent but reads see writes within the tx.
func findParentTx(tx *sql.Tx, keyHash string, strictHashes []string) (parentID, sessionRootID string, prefixLen int, ok bool, err error) {
	if len(strictHashes) == 0 {
		return "", "", 0, false, nil
	}
	placeholders := strings.Repeat("?,", len(strictHashes))
	placeholders = placeholders[:len(placeholders)-1]
	q := fmt.Sprintf(`
		SELECT id, session_root_id, prefix_len
		FROM traces
		WHERE key_hash = ?
		  AND prefix_canonical_hash IN (%s)
		ORDER BY prefix_len DESC, ts_start DESC
		LIMIT 1
	`, placeholders)

	args := make([]any, 0, len(strictHashes)+1)
	args = append(args, keyHash)
	for _, h := range strictHashes {
		args = append(args, h)
	}

	var pid, root string
	var plen int
	if err := tx.QueryRow(q, args...).Scan(&pid, &root, &plen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", 0, false, nil
		}
		return "", "", 0, false, fmt.Errorf("find parent: %w", err)
	}
	return pid, root, plen, true, nil
}

// CountRows is used by the startup rebuild path to detect a "fresh" index.
func (s *Store) CountRows() (int64, error) {
	var n int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM traces").Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ---- helpers for nullable column binding ----

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullBoolToInt(b *bool) any {
	if b == nil {
		return nil
	}
	return boolToInt(*b)
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// keep the database/sql import live (helper inline used elsewhere).
var _ = sql.ErrNoRows
