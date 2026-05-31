package api

// mediaHandler implements GET /api/media/{trace_id}/{idx} per
// uiux-research/phase-k-media-contract.md §5.1.
//
// Backend PHILOSOPHY §6: filesystem is truth. The extractor wrote files
// at `<DataDir>/<date>/<keyhash8>/media/<trace_id>/<idx>.<ext>` at finalize
// time; this handler is a pure read of that layout. No fallback to the
// JSONL b64 — if the file isn't on disk (e.g. trace recorded with
// save_attachments=false), 404 is the honest answer.
//
// Date and keyhash8 are derived from the row's JSONLPath, not from
// TsStart, because JSONLPath is the literal bucket the extractor used;
// TsStart can disagree at UTC-day boundaries.

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xiayangzhang/api-log/internal/store/sqlite"
)

func mediaHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.PathValue("trace_id")
		idxStr := r.PathValue("idx")
		if traceID == "" || idxStr == "" {
			writeError(w, http.StatusBadRequest, "missing_param")
			return
		}
		// Validate idx is a non-negative integer; the actual filename
		// match uses idxStr (string) so we don't need the parsed int
		// later, just the validation pass.
		if n, err := strconv.Atoi(idxStr); err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_param",
				map[string]string{"param": "idx"})
			return
		}

		row, err := deps.Store.GetByID(traceID)
		if err != nil {
			if errors.Is(err, sqlite.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found",
					map[string]string{"detail": "no trace for trace_id=" + traceID})
				return
			}
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}

		date, keyhash8 := splitJSONLPathLocal(row.JSONLPath)
		if date == "" || keyhash8 == "" {
			writeError(w, http.StatusNotFound, "not_found",
				map[string]string{"detail": "trace has no resolvable media bucket"})
			return
		}

		mediaDir := filepath.Join(deps.DataDir, date, keyhash8, "media", traceID)

		// Look for a file named "<idx>.*" — extension is determined by
		// the extractor from MIME, not from any user-supplied name, so we
		// trust whatever the extractor wrote.
		entries, err := os.ReadDir(mediaDir)
		if err != nil {
			// Treat any read error (NotExist, permission, etc.) as 404 —
			// from the API's point of view, there's no media here. We do
			// not surface a 500 just because save_attachments was false
			// or the trace predates extraction.
			writeError(w, http.StatusNotFound, "not_found",
				map[string]string{"detail": "no extracted media for trace_id=" + traceID + ", idx=" + idxStr})
			return
		}

		prefix := idxStr + "."
		var match string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, prefix) {
				match = name
				break
			}
		}
		if match == "" {
			writeError(w, http.StatusNotFound, "not_found",
				map[string]string{"detail": "no extracted media for trace_id=" + traceID + ", idx=" + idxStr})
			return
		}

		fullPath := filepath.Join(mediaDir, match)
		f, err := os.Open(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "not_found",
					map[string]string{"detail": "media file disappeared between list and open"})
				return
			}
			writeError(w, http.StatusInternalServerError, "server_error",
				map[string]string{"detail": err.Error()})
			return
		}
		defer f.Close()

		stat, err := f.Stat()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error",
				map[string]string{"detail": err.Error()})
			return
		}

		// MIME from extension only — the file was written with an
		// extension chosen from MimeType at extraction time, so this
		// round-trips cleanly. Default to octet-stream for unknown.
		ext := filepath.Ext(match)
		ctype := mime.TypeByExtension(ext)
		if ctype == "" {
			ctype = "application/octet-stream"
		}

		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
		w.Header().Set("Cache-Control", "public, immutable")
		// WriteHeader(200) implicit on first Write via io.Copy.
		_, _ = io.Copy(w, f)
	})
}

// splitJSONLPathLocal mirrors exporter.splitJSONLPath (which is
// unexported). Writer writes `<dataDir>/<date>/<keyhash8>.jsonl[.gz]`;
// we want the date directory name and the keyhash8 base. If the path
// doesn't fit that shape we return zeros and let the caller 404.
func splitJSONLPathLocal(path string) (date, keyhash8 string) {
	if path == "" {
		return "", ""
	}
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".gz")
	base = strings.TrimSuffix(base, ".jsonl")
	keyhash8 = base
	parent := filepath.Base(filepath.Dir(path))
	date = parent
	if date == "." || date == "/" || keyhash8 == "" {
		return "", ""
	}
	return date, keyhash8
}
