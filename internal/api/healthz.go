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

