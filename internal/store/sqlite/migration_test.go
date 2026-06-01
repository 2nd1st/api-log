package sqlite

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMigrationCreatesJSONLPathIndex asserts the v0.1.1 additive
// idx_jsonl_path index lands at Open() time so the storage
// coordinator's reconcileOrphans and DeleteByJSONLPath queries use an
// index seek instead of a table scan.
func TestMigrationCreatesJSONLPathIndex(t *testing.T) {
	s := openTestStore(t)

	rows, err := s.db.Query(`
		SELECT name, tbl_name, sql
		FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_jsonl_path'
	`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		t.Fatal("idx_jsonl_path not present after migrate")
	}
	var name, tbl, sql string
	if err := rows.Scan(&name, &tbl, &sql); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if tbl != "traces" {
		t.Errorf("idx_jsonl_path on table %q, want traces", tbl)
	}
	if !strings.Contains(sql, "jsonl_path") {
		t.Errorf("idx_jsonl_path SQL does not mention jsonl_path: %q", sql)
	}
}

// TestMigrationIdempotentOnExistingDB explicitly reopens a populated
// store to confirm the v0.1.1 index migration plays well with prior
// data. CREATE INDEX IF NOT EXISTS is idempotent, but exercising it
// catches accidental regressions if someone replaces it with a bare
// CREATE.
func TestMigrationIdempotentOnExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v0.1.0-style.sqlite")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Insert a row through the normal path so the table has real data
	// across the migrate-rerun boundary.
	r := mkRow("01H_pre_migrate", time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC))
	r.JSONLPath = "data/2026-05-30/deadbeef.jsonl"
	if err := s1.AppendTrace(r, nil); err != nil {
		t.Fatalf("AppendTrace pre-migrate: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open after row inserted: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	n, err := s2.CountRows()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("row count post-reopen = %d, want 1", n)
	}
}

// TestDSNPragmasApplied verifies the DSN-embedded pragmas took effect
// for every connection in the pool. Without DSN pragmas, only the
// first-Exec connection would have busy_timeout set; the rest would
// surface SQLITE_BUSY under parallel load. We don't observe the pool
// directly but a quick query through the standard handle suffices to
// confirm WAL mode + busy_timeout are present on the conn that
// executes Query.
func TestDSNPragmasApplied(t *testing.T) {
	s := openTestStore(t)

	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var foreignKeys int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}
}

