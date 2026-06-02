package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/2nd1st/api-log/internal/exporter"
	"github.com/2nd1st/api-log/internal/store/sqlite"
)

// defaultExportHardCap bounds an un-`?all=1` export to a row count
// the operator can hold in working memory + a single zip's RAM. 50000
// rows is the value the v0.1.1 design (uiux-research/data-volume-
// roadmap.md §B3) settled on after a four-reviewer pass; raise it via
// `?all=1` for "I really do want everything" exports.
//
// var (not const) so tests can save / restore the cap without
// inserting tens of thousands of rows just to exercise the 413 path.
// Production callers never mutate it.
var defaultExportHardCap = 50000

// defaultExportByteHardCap bounds an un-`?bytes_all=1` export by the
// sum of source JSONL file sizes for the matched groups, measured
// after Phase 1 (one os.Stat per group, not per row) and before any
// zip bytes are written. 2 GiB matches the conservative posture of
// the 50k-row default — fits in working memory, survives default
// upload limits on most reverse proxies, and keeps the failure mode
// loud-and-early rather than swap-thrashing the operator's laptop
// mid-download.
//
// Independent of the row cap: `?all=1` does NOT bypass this; pass
// `?bytes_all=1` to opt out of the byte cap specifically. Each cap
// names what it bypasses — see /api/export contract.
//
// var (not const) so tests can save / restore the cap without
// seeding 2 GiB of JSONL just to exercise the 413 path. Production
// callers never mutate it. Wiring to YAML / env (precedent:
// Storage.MaxBodyBytes, `APILOG_API_EXPORT_BYTE_HARDCAP`) is a
// post-v0.1.2 follow-up.
var defaultExportByteHardCap int64 = 2 << 30

// exportHandler implements GET /api/export; see
// docs/specs/phase-i-export-contract.md.
//
// Pipeline (v0.1.2, streaming):
//
//  1. Parse filters (status/model/path/key_hash/session_root_id/
//     since/until/project).
//  2. Pre-flight CountMatching against hardCap (50000 default,
//     `?all=1` disables). If the count exceeds the cap, return 413
//     BEFORE any zip bytes hit the wire — adopters get a clean JSON
//     error pointing at the cap-bypass flag.
//  3. Stage Content-Type / Disposition / Cache-Control headers but
//     keep WriteHeader(200) lazy until the first actual zip byte.
//     Pre-flight or query errors before any byte → clean JSON 500.
//  4. exporter.WriteZip streams the zip; r.Context() flows through so
//     a closed connection aborts the cursor + stops the zip. The byte
//     cap (2 GiB default, `?bytes_all=1` disables) is enforced INSIDE
//     WriteZip after Phase 1 (cursor) but before any zip byte —
//     *exporter.ByteCapExceededError comes back as a typed error and
//     this handler maps it to 413 `export_too_large` with
//     `limit:"bytes"`. The byte cap is independent of `?all=1`.
func exportHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filters, errCode, errField := parseExportFilters(r)
		if errCode != "" {
			writeError(w, http.StatusBadRequest, errCode, map[string]string{"param": errField})
			return
		}

		hardCap := defaultExportHardCap
		if r.URL.Query().Get("all") == "1" {
			hardCap = 0 // unlimited
		}

		// Byte cap (v0.1.2). Independent of the row cap and its
		// `?all=1` bypass — opting out of the row safety net does
		// NOT opt out of the byte safety net, and vice versa.
		byteCap := defaultExportByteHardCap
		if r.URL.Query().Get("bytes_all") == "1" {
			byteCap = 0 // unlimited
		}

		// Pre-flight count. CountMatching with hardCap > 0 stops at
		// hardCap+1 — if it returns > hardCap we know the export
		// would exceed the cap without scanning the rest of the
		// table.
		ctx := r.Context()
		if hardCap > 0 {
			cnt, err := deps.Store.CountMatching(ctx, filters, hardCap)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "count_failed",
					map[string]string{"detail": err.Error()})
				return
			}
			if cnt > hardCap {
				writeError(w, http.StatusRequestEntityTooLarge, "export_too_large",
					map[string]string{
						"matched": ">" + strconv.Itoa(hardCap),
						"cap":     strconv.Itoa(hardCap),
						"hint":    "narrow filters or pass ?all=1",
					})
				return
			}
		}

		// Stage response headers but defer WriteHeader(200) to the first
		// actual zip byte. If WriteZip fails *before* writing anything
		// (e.g. SQLite cursor error), nothing has gone on the wire and
		// we can still surface the contract's documented 500.
		// Filename uses a colon-free UTC stamp ("YYYYMMDDTHHMMSS.mmmZ")
		// to stay portable across the filesystems people unzip into
		// (Windows in particular rejects colons in filenames).
		now := time.Now().UTC()
		filename := "api-log-export-" + now.Format("20060102T150405.000Z") + ".zip"
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		w.Header().Set("Cache-Control", "no-store")

		lw := &lazyHeaderWriter{w: w}
		if err := exporter.WriteZip(ctx, lw, deps.Store, deps.DataDir, filters, byteCap, deps.StorageCoord); err != nil {
			if !lw.wrote {
				// Byte-cap pre-flight failed BEFORE any zip bytes — map
				// to 413 with the same `export_too_large` code as the
				// row cap, discriminated by limit:"bytes". Must precede
				// the generic 500 mapping below so the typed error is
				// not swallowed by `export_failed`.
				var bcErr *exporter.ByteCapExceededError
				if errors.As(err, &bcErr) {
					writeError(w, http.StatusRequestEntityTooLarge, "export_too_large",
						map[string]string{
							"limit":         "bytes",
							"matched_bytes": strconv.FormatInt(bcErr.Total, 10),
							"cap_bytes":     strconv.FormatInt(bcErr.Cap, 10),
							"hint":          "narrow filters to exclude whole days/keys, or pass ?bytes_all=1",
							"note":          "estimate is sum of source JSONL sizes; actual zip is smaller due to Deflate, and gzipped days are counted at compressed size",
						})
					return
				}
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

// parseExportFilters mirrors parseListFilters and surfaces the same
// vocabulary plus the v0.1.1-added `project` filter (matched against
// the client_project column populated by parser.ExtractProjectContext
// at finalize). Cursor is rejected — /api/export is a one-shot
// endpoint, not paginated. The old `limit` query param is also
// rejected: pre-flight CountMatching + `?all=1` replace it.
func parseExportFilters(r *http.Request) (sqlite.ListFilters, string, string) {
	q := r.URL.Query()
	f := sqlite.ListFilters{}

	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, "bad_param", "since"
		}
		f.Since = t
	}
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, "bad_param", "until"
		}
		f.Until = t
	}
	if v := q.Get("status"); v != "" {
		if len(v) == 3 && (v[1] == 'x' || v[1] == 'X') && (v[2] == 'x' || v[2] == 'X') {
			switch v[0] {
			case '2', '4', '5':
				f.StatusBucket = int(v[0] - '0')
			default:
				return f, "bad_param", "status"
			}
		} else {
			n, err := strconv.Atoi(v)
			if err != nil {
				return f, "bad_param", "status"
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
			return f, "bad_param", "key_hash"
		}
		f.KeyHashPrefix = v
	}
	if v := q.Get("session_root_id"); v != "" {
		f.SessionRootID = v
	}
	if v := q.Get("project"); v != "" {
		f.Project = v
	}
	return f, "", ""
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
