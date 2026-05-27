package api

import (
	"net/http"
	"strconv"
	"time"
)

// listSessions handles GET /api/sessions — aggregated rollup of
// traces by session_root_id, latest-activity first. Used by the
// viewer's "by session" toggle so an operator can see a stream of
// conversations rather than individual turns.
//
// Query params:
//   limit (default 100, max 500)
//   since (RFC3339; constraint on session's last activity timestamp)
func listSessions(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := 100
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				writeError(w, http.StatusBadRequest, "bad_param", map[string]string{"param": "limit"})
				return
			}
			limit = n
		}
		var since time.Time
		if v := q.Get("since"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_param", map[string]string{"param": "since"})
				return
			}
			since = t
		}

		sums, err := deps.Store.ListSessions(since, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}

		out := make([]map[string]any, 0, len(sums))
		for _, s := range sums {
			out = append(out, map[string]any{
				"session_root_id":    s.SessionRootID,
				"n_turns":            s.NTurns,
				"first_ts":           s.FirstTs.UTC().Format(time.RFC3339Nano),
				"last_ts":            s.LastTs.UTC().Format(time.RFC3339Nano),
				"last_path":          s.LastPath,
				"last_status":        s.LastStatus,
				"last_model":         s.LastModel,
				"distinct_key_count": s.DistinctKeyCount,
				"ok_count":           s.OkCount,
				"err_count":          s.ErrCount,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
	})
}
