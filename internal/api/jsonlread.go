package api

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// readJSONLLine reads one JSONL line at the given uncompressed offset
// from a .jsonl or .jsonl.gz file, per ARCHITECTURE § 6.3.
//
// For plain .jsonl: os.Open + Seek(offset) + bufio.ReadBytes('\n').
// For .jsonl.gz: gzip.NewReader + io.CopyN(io.Discard, gz, offset) to
// advance the uncompressed stream to the line, then ReadBytes('\n').
// gzip preserves uncompressed offsets so the offset stored at write
// time stays valid post-rotation.
//
// If `path` does not exist as-is, we try path+".gz" (handles the case
// where the SQLite row was written before rotation but the file has
// since been gzipped). If neither exists, returns os.ErrNotExist.
func readJSONLLine(path string, offset int64) (json.RawMessage, error) {
	if strings.HasSuffix(path, ".gz") {
		return readJSONLFromGz(path, offset)
	}

	if _, err := os.Stat(path); err == nil {
		return readJSONLFromPlain(path, offset)
	}

	// Plain file missing — try gzipped sibling.
	gzPath := path + ".gz"
	if _, err := os.Stat(gzPath); err == nil {
		return readJSONLFromGz(gzPath, offset)
	}
	return nil, fmt.Errorf("neither %s nor %s.gz exists", path, path)
}

func readJSONLFromPlain(path string, offset int64) (json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek to %d: %w", offset, err)
	}
	return readOneLine(f)
}

func readJSONLFromGz(path string, offset int64) (json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	if offset > 0 {
		// Advance the uncompressed stream. Use a single big chunk size
		// to amortize CopyN's per-loop overhead.
		if _, err := io.CopyN(io.Discard, gz, offset); err != nil {
			return nil, fmt.Errorf("advance to offset %d in %s: %w", offset, path, err)
		}
	}
	return readOneLine(gz)
}

func readOneLine(r io.Reader) (json.RawMessage, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// Strip trailing newline.
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) == 0 {
		return nil, errors.New("empty line at offset")
	}
	return json.RawMessage(line), nil
}
