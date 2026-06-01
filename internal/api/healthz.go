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
//
// When StorageCoord is wired in (v0.1.1), the payload gains a top-level
// `storage` key carrying coord.Status() — DataDirBytes, retention
// thresholds + computed UsagePct + State, last-eviction stats. The key
// is omitted when StorageCoord is nil (tests + JSONL-only setups).
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
		if deps.StorageCoord != nil {
			body["storage"] = storageStatusJSON(deps.StorageCoord.Status())
		}
		writeJSON(w, http.StatusOK, body)
	})
}

// storageStatusJSON projects coord.Status() into a stable JSON shape.
// Done here (not on storage.Status via tags) so the storage package
// stays free of HTTP-surface concerns and the JSON field names can
// evolve without ripping through call sites.
func storageStatusJSON(s storageStatusView) map[string]any {
	out := map[string]any{
		"data_dir_bytes":   s.DataDirBytes,
		"max_bytes":        s.MaxBytes,
		"max_age_days":     s.MaxAgeDays,
		"usage_pct":        s.UsagePct,
		"state":            s.State,
		"engine_running":   s.EngineRunning,
		"eviction_cap_hit": s.EvictionCapHit,
	}
	if !s.LastEvictionTs.IsZero() {
		out["last_eviction_ts"] = s.LastEvictionTs.Format(time.RFC3339)
		out["last_evicted_bytes"] = s.LastEvictedBytes
	}
	return out
}

// storageStatusView is the subset of storage.Status this package
// actually marshals. Defined as a type alias-style local struct so
// healthz_test.go can build fixtures without importing storage.
type storageStatusView = struct {
	DataDirBytes     int64
	MaxBytes         int64
	MaxAgeDays       int
	UsagePct         int
	State            string
	LastEvictionTs   time.Time
	LastEvictedBytes int64
	EvictionCapHit   bool
	NextEvictionEst  time.Time
	EngineRunning    bool
}
