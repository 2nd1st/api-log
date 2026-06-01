package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/2nd1st/api-log/internal/exporter"
	"github.com/2nd1st/api-log/internal/store/sqlite"
)

// exportHandler implements GET /api/export; see
// docs/specs/phase-i-export-contract.md.
//
// Filters share the /api/traces vocabulary (status, model, key_hash,
// session_root_id, since, until, path) — we parse them with the same
// pipeline. The limit param is unbounded (defaults to unlimited when
// absent) — export streams, memory is not the bottleneck, and SQLite
// handles 100k+ rows comfortably. The body is a streaming zip; once the
// first byte is on the wire we can no longer set a status code, so all
// validation runs before any zip data is written.
func exportHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filters, limit, errCode, errField := parseExportFilters(r)
		if errCode != "" {
			writeError(w, http.StatusBadRequest, errCode, map[string]string{"param": errField})
			return
		}

		// Stage response headers but defer WriteHeader(200) to the first
		// actual zip byte. If WriteZip fails *before* writing anything
		// (e.g. SQLite query error), nothing has gone on the wire and we
		// can still surface the contract's documented 500 export_failed.
		// Filename uses a colon-free UTC stamp ("YYYYMMDDTHHMMSS.mmmZ")
		// to stay portable across the filesystems people unzip into
		// (Windows in particular rejects colons in filenames).
		now := time.Now().UTC()
		filename := "api-log-export-" + now.Format("20060102T150405.000Z") + ".zip"
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		w.Header().Set("Cache-Control", "no-store")

		lw := &lazyHeaderWriter{w: w}
		if err := exporter.WriteZip(lw, deps.Store, deps.DataDir, filters, limit, deps.StorageCoord); err != nil {
			if !lw.wrote {
				// Nothing on the wire yet — clean 500.
				writeError(w, http.StatusInternalServerError, "export_failed",
					map[string]string{"detail": err.Error()})
				return
			}
			// Mid-stream failure: zip is partial. Headers committed; log
			// and let the client see a truncated body.
			slog.Error("export stream failed mid-zip", "err", err)
			return
		}
		// Empty / success path: WriteZip always writes at least
		// agent/CLAUDE.md, agent/jq-cheatsheet.md, and README.md, so
		// lw.wrote is true and the 200 already fired through Write().
	})
}

// parseExportFilters mirrors parseListFilters but does not impose an
// upper bound on limit (the export streams; the previous 5000-row cap
// was an arbitrary safety guard and has been removed). A missing or
// zero limit means "every matching row". Cursor is rejected —
// /api/export is a one-shot endpoint, not paginated.
func parseExportFilters(r *http.Request) (sqlite.ListFilters, int, string, string) {
	q := r.URL.Query()
	f := sqlite.ListFilters{}

	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, 0, "bad_param", "since"
		}
		f.Since = t
	}
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, 0, "bad_param", "until"
		}
		f.Until = t
	}
	if v := q.Get("status"); v != "" {
		if len(v) == 3 && (v[1] == 'x' || v[1] == 'X') && (v[2] == 'x' || v[2] == 'X') {
			switch v[0] {
			case '2', '4', '5':
				f.StatusBucket = int(v[0] - '0')
			default:
				return f, 0, "bad_param", "status"
			}
		} else {
			n, err := strconv.Atoi(v)
			if err != nil {
				return f, 0, "bad_param", "status"
			}
			f.Status = &n
		}
	}
	if v := q.Get("model"); v != "" {
		f.Model = v
	}
	if v := q.Get("path"); v != "" {
		if strings.HasSuffix(v, "*") {
			f.PathPrefix = strings.TrimSuffix(v, "*")
		} else {
			f.Path = v
		}
	}
	if v := q.Get("key_hash"); v != "" {
		if len(v) != 8 && len(v) != 16 {
			return f, 0, "bad_param", "key_hash"
		}
		f.KeyHashPrefix = v
	}
	if v := q.Get("session_root_id"); v != "" {
		f.SessionRootID = v
	}

	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return f, 0, "bad_param", "limit"
		}
		limit = n
	}
	return f, limit, "", ""
}

// lazyHeaderWriter delays http.ResponseWriter.WriteHeader(200) until the
// first Write call. This lets the export handler send a clean 500 when
// the zip generator errors before producing any bytes (typical: SQLite
// read failure) without forcing the success path to call WriteHeader
// explicitly.
type lazyHeaderWriter struct {
	w     http.ResponseWriter
	wrote bool
}

func (l *lazyHeaderWriter) Write(p []byte) (int, error) {
	if !l.wrote {
		l.w.WriteHeader(http.StatusOK)
		l.wrote = true
	}
	return l.w.Write(p)
}
