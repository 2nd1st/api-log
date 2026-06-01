package capture

import (
	"fmt"
	"os"
	"path/filepath"
)

// TmpDir manages the data/tmp/ directory documented in ARCHITECTURE § 2.
// On startup the entire directory is wiped (orphans from a prior crash).
type TmpDir struct {
	Path string
}

// NewTmpDir creates and wipes the tmp directory under dataDir. Must be
// called exactly once at process startup, before any traces are captured.
func NewTmpDir(dataDir string) (*TmpDir, error) {
	path := filepath.Join(dataDir, "tmp")
	// Remove + recreate to ensure a clean slate (orphans from prior crash).
	if err := os.RemoveAll(path); err != nil {
		return nil, fmt.Errorf("wipe tmp dir: %w", err)
	}
	// 0o700: tmp/ carries in-flight bodies that include raw API keys
	// (Authorization headers) before the finalize step writes them to
	// the JSONL. Same-user readability is the threat model floor; the
	// owning process is the only legitimate reader.
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("create tmp dir: %w", err)
	}
	return &TmpDir{Path: path}, nil
}

// CreateTraceFiles opens two new files (req + resp) for the given trace ID.
// The caller closes them at finalize.
func (t *TmpDir) CreateTraceFiles(traceID string) (req, resp *os.File, err error) {
	reqPath := filepath.Join(t.Path, traceID+".req.bin")
	respPath := filepath.Join(t.Path, traceID+".resp.bin")

	// 0o600 for the same reason the tmp dir is 0o700 — the body bytes
	// here include raw API keys; only the owning process should be
	// able to read them.
	req, err = os.OpenFile(reqPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("create req tmp: %w", err)
	}
	resp, err = os.OpenFile(respPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = req.Close()
		_ = os.Remove(reqPath)
		return nil, nil, fmt.Errorf("create resp tmp: %w", err)
	}
	return req, resp, nil
}

// RemoveTraceFiles deletes a trace's tmp files. Called by the writer
// goroutine after the JSONL line and SQLite row are durably written.
// Errors are non-fatal: orphan tmp files will be cleaned at next startup.
func (t *TmpDir) RemoveTraceFiles(traceID string) {
	_ = os.Remove(filepath.Join(t.Path, traceID+".req.bin"))
	_ = os.Remove(filepath.Join(t.Path, traceID+".resp.bin"))
}
