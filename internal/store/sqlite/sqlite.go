// Package sqlite is the derived-cache store described in ARCHITECTURE § 4.
//
// The SQLite database mirrors JSONL columns and adds writer-computed
// columns (model, tokens, key_hash, prefix_canonical_hash, parent_id,
// session_root_id). Everything in here is fully rebuildable from
// data/**/*.jsonl{,.gz}; SQLite is a rebuildable derived cache.
//
// v0 uses modernc.org/sqlite (pure Go, no cgo). Slightly slower than
// mattn/go-sqlite3 but trivial to build / cross-compile.
package sqlite

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // driver registration
)

// Store is the writer-side handle into index.sqlite. One Writer holds
// one Store. Read-API handlers will get their own Store handles in M4
// (separate connections, WAL allows concurrent reads).
type Store struct {
	db *sql.DB
}

// Open opens or creates index.sqlite at path, runs migration, and
// applies the pragmas from ARCHITECTURE § 4.
func Open(path string) (*Store, error) {
	// modernc.org/sqlite DSN: see https://gitlab.com/cznic/sqlite — uses
	// the file path with optional query params after a `?`. We set the
	// pragmas via Exec after open so they apply uniformly.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}
	// WAL mode is enabled below at Open(); 8 conns lets the read API and
	// writer proceed in parallel. SQLite WAL serializes writes internally,
	// so we don't need a single-conn writer lock at the database/sql layer
	// — and capping at 1 forced every /api/traces read to queue behind the
	// writer's finalize transaction. Idle cap stays modest.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA temp_store=MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying *sql.DB.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// migrate creates the traces table + indexes if they don't exist.
//
// v0 ships exactly this schema; later versions add columns via additive
// migrations (schema is append-only for the lifetime). Breaking changes go
// through a new format key, not a column rename / drop.
func migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS traces (
  id              TEXT PRIMARY KEY,
  ts_start        INTEGER NOT NULL,
  ts_end          INTEGER NOT NULL,
  client          TEXT,
  method          TEXT,
  path            TEXT,
  upstream        TEXT,
  status          INTEGER,
  disconnected    INTEGER,
  truncated_req   INTEGER,
  truncated_resp  INTEGER,

  -- Protocol field copies
  model           TEXT,
  stream          INTEGER,
  prompt_tokens   INTEGER,
  completion_tokens INTEGER,
  total_tokens    INTEGER,
  finish_reason   TEXT,

  -- Deterministic encodings
  key_hash              TEXT NOT NULL,
  prefix_len            INTEGER,
  prefix_canonical_hash TEXT,

  -- Cross-trace structural algorithm output
  parent_id             TEXT,
  session_root_id       TEXT NOT NULL,

  -- JSONL location
  jsonl_path            TEXT NOT NULL,
  jsonl_offset          INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ts             ON traces(ts_start DESC);
CREATE INDEX IF NOT EXISTS idx_key_ts         ON traces(key_hash, ts_start DESC);
CREATE INDEX IF NOT EXISTS idx_session_root   ON traces(session_root_id);
CREATE INDEX IF NOT EXISTS idx_prefix_hash    ON traces(key_hash, prefix_canonical_hash);
`
	for _, stmt := range strings.Split(schema, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", firstLine(stmt), err)
		}
	}

	// Additive migration: media_count column.
	//
	// Schema is append-only: never rename or drop, only add.
	// ALTER TABLE ... ADD COLUMN has no IF NOT EXISTS in SQLite, so we run it
	// unconditionally and swallow the specific "duplicate column" error that
	// occurs on re-open. Any other error propagates.
	if _, err := db.Exec(`ALTER TABLE traces ADD COLUMN media_count INTEGER NOT NULL DEFAULT 0`); err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "duplicate column") && !strings.Contains(msg, "already exists") {
			return fmt.Errorf("alter add media_count: %w", err)
		}
	}

	// Additive migration: usage-extraction columns.
	//
	// Deterministic copies of named protocol usage fields: cache hits, cache
	// creation, and reasoning tokens. Nullable
	// (no DEFAULT) so an absent field stays absent rather than being conflated
	// with a real zero. Follows the same idempotent-ALTER pattern as media_count.
	for _, alter := range []string{
		"ALTER TABLE traces ADD COLUMN cached_tokens INTEGER",
		"ALTER TABLE traces ADD COLUMN cache_creation_tokens INTEGER",
		"ALTER TABLE traces ADD COLUMN reasoning_tokens INTEGER",
	} {
		if _, err := db.Exec(alter); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "duplicate column") && !strings.Contains(msg, "already exists") {
				return fmt.Errorf("alter add: %w", err)
			}
		}
	}

	// Additive migration: client-identity columns.
	//
	// Deterministic copies of named request-header fields emitted by
	// ExtractClient at finalize time. Nullable TEXT so absence stays absent. The
	// idempotent ALTER pattern matches media_count and the usage columns above.
	for _, alter := range []string{
		"ALTER TABLE traces ADD COLUMN client_kind TEXT",
		"ALTER TABLE traces ADD COLUMN client_version TEXT",
	} {
		if _, err := db.Exec(alter); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "duplicate column") && !strings.Contains(msg, "already exists") {
				return fmt.Errorf("alter add: %w", err)
			}
		}
	}

	// Additive migration: project-context column.
	//
	// Deterministic copy of the project name parsed from request-body
	// system/instructions text. Nullable TEXT so a trace with no project signal
	// stays NULL (distinct from a real empty string). Mirror of the viewer's
	// promptSource.ts so the derived column matches what the UI used to compute
	// at render time. Idempotent ALTER pattern matches the client, usage, and
	// media_count blocks above.
	if _, err := db.Exec("ALTER TABLE traces ADD COLUMN client_project TEXT"); err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "duplicate column") && !strings.Contains(msg, "already exists") {
			return fmt.Errorf("alter add client_project: %w", err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// unixMs returns the Unix millisecond value of t in UTC. SQLite stores
// timestamps as INTEGER ms for cheap range queries on ts_start.
func unixMs(t time.Time) int64 {
	return t.UTC().UnixNano() / int64(time.Millisecond)
}
