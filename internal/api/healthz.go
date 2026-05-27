package api

import (
	"net/http"
	"time"
)

// healthz returns 200 with version + uptime + counter snapshot per
// ARCHITECTURE § 6.5. Does NOT check disk or SQLite — only HTTP-path
// liveness and counter exposure.
func healthz(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"status":         "ok",
			"ts":             time.Now().UTC().Format(time.RFC3339Nano),
			"version":        deps.Version,
			"uptime_seconds": int64(time.Since(deps.StartedAt).Seconds()),
			"counters":       deps.Counters.Snapshot(),
		}
		writeJSON(w, http.StatusOK, body)
	})
}

// rootPointer returns the JSON pointer to the separate viewer project
// per ARCHITECTURE § 6.6. v0 ships no embedded HTML.
func rootPointer(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"name":    "api-log",
			"viewer":  "https://github.com/leoyun/api-log-viewer",
			"version": deps.Version,
		})
	})
}

// replayPlaceholder is the M5 endpoint stub. Returns 501 Not Implemented
// so an early viewer integrating with v0 sees a clear "not yet" rather
// than a 404 that could mean "endpoint moved".
func replayPlaceholder() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			map[string]string{"detail": "/replay lands in M5"})
	})
}
