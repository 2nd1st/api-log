package capture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewTmpDirWipesOrphans(t *testing.T) {
	dataDir := t.TempDir()

	// Plant an orphan from a prior run.
	tmpPath := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(tmpPath, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(tmpPath, "01HZZZZ.req.bin")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}

	td, err := NewTmpDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan file should be wiped, stat err = %v", err)
	}
	if _, err := os.Stat(td.Path); err != nil {
		t.Errorf("tmp dir should exist after NewTmpDir, err = %v", err)
	}
}

func TestCreateAndRemoveTraceFiles(t *testing.T) {
	td, err := NewTmpDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	traceID := "01HX7K8MS"

	req, resp, err := td.CreateTraceFiles(traceID)
	if err != nil {
		t.Fatal(err)
	}
	_ = req.Close()
	_ = resp.Close()

	for _, suffix := range []string{".req.bin", ".resp.bin"} {
		p := filepath.Join(td.Path, traceID+suffix)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}

	td.RemoveTraceFiles(traceID)

	for _, suffix := range []string{".req.bin", ".resp.bin"} {
		p := filepath.Join(td.Path, traceID+suffix)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, stat err = %v", p, err)
		}
	}
}
