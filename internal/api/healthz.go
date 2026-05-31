package api

import (
	"net/http"
	"time"
)

// healthz returns 200 with version + uptime + counter snapshot per
// ARCHITECTURE § 6.5. Does NOT check disk or SQLite — only HTTP-path
// liveness and counter exposure.
//
// When ViewerHost is wired in, the payload gains a top-level `viewer`
// key carrying the host's Info() snapshot (enabled, repo, version,
// source, sha256, error, public_path). The key is omitted when
// ViewerHost is nil so tests + integrations that don't enable the
// viewer see a payload shape unchanged from pre-hosted-viewer builds.
func healthz(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"status":         "ok",
			"ts":             time.Now().UTC().Format(time.RFC3339Nano),
			"version":        deps.Version,
			"uptime_seconds": int64(time.Since(deps.StartedAt).Seconds()),
			"counters":       deps.Counters.Snapshot(),
		}
		if deps.ViewerHost != nil {
			body["viewer"] = deps.ViewerHost.Info()
		}
		writeJSON(w, http.StatusOK, body)
	})
}
