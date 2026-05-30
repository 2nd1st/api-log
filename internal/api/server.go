// Package api implements the read API surface from ARCHITECTURE § 6.
//
// Four endpoints:
//   GET /                       — JSON pointer to the viewer project
//   GET /healthz                — liveness + in-memory counter snapshot
//   GET /api/traces             — list, SQLite-backed, paginated
//   GET /api/traces/:id         — detail, SQLite + JSONL seek
//   GET /api/traces/:id/replay  — M5 (placeholder for now)
package api

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/leoyun/api-log/internal/counters"
	"github.com/leoyun/api-log/internal/store/sqlite"
	"github.com/leoyun/api-log/internal/viewer"
)

// Deps is the bag of process-wide handles the API handlers need.
type Deps struct {
	Store      *sqlite.Store
	Counters   *counters.Counters
	AdminToken string
	Version    string
	StartedAt  time.Time

	// DataDir is the storage root that JSONL paths in SQLite rows are
	// relative to. /api/export reads files from here to stream into the
	// zip; other handlers seek by absolute JSONLPath and don't need it.
	DataDir string

	// Phase K — media extraction toggle. Atomic so PUT /api/config/media
	// can flip it from a different goroutine while the writer reads it
	// per-trace. Nil = extraction disabled (treat as off).
	MediaEnabled *atomic.Bool

	// PluginTypes is the catalogue provider for GET /api/plugins/types
	// (plugin-b-c-spec §8.5). main.go injects this from the
	// builtin-plugin registry; W3 keeps it nil-safe so the handler
	// returns an empty list before main.go is wired and during tests
	// that do not exercise the catalogue. Returning the slice fresh on
	// each call (rather than caching a pointer) lets future hot-swap
	// scenarios surface a changing catalogue without touching Deps.
	PluginTypes func() []PluginTypeDescriptor
}

// NewMux returns an http.Handler ready to mount on the API listener.
// All /api/* routes require the admin bearer; /healthz and / are
// authenticated too (operator dashboards need the token anyway).
func NewMux(deps Deps) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /healthz", authMW(deps.AdminToken, healthz(deps)))
	mux.Handle("GET /api/traces", authMW(deps.AdminToken, listTraces(deps)))
	mux.Handle("GET /api/traces/{id}", authMW(deps.AdminToken, getTrace(deps)))
	mux.Handle("GET /api/traces/{id}/replay", authMW(deps.AdminToken, replayHandler(deps)))
	mux.Handle("GET /api/sessions", authMW(deps.AdminToken, listSessions(deps)))
	mux.Handle("GET /api/export", authMW(deps.AdminToken, exportHandler(deps)))

	// Phase K media endpoints.
	mux.Handle("GET /api/media/{trace_id}/{idx}", authMW(deps.AdminToken, mediaHandler(deps)))
	mux.Handle("GET /api/config/media", authMW(deps.AdminToken, getConfigMedia(deps)))
	mux.Handle("PUT /api/config/media", authMW(deps.AdminToken, putConfigMedia(deps)))

	// Plugin runtime-config endpoints (plugin-b-c-spec §8.5).
	mux.Handle("GET /api/plugins/types", authMW(deps.AdminToken, listPluginTypes(deps)))
	mux.Handle("GET /api/config/plugins", authMW(deps.AdminToken, getConfigPlugins(deps)))
	mux.Handle("PUT /api/config/plugins", authMW(deps.AdminToken, putConfigPlugins(deps)))
	mux.Handle("DELETE /api/config/plugins", authMW(deps.AdminToken, deleteConfigPlugins(deps)))
	mux.Handle("PUT /api/config/plugins/{id}", authMW(deps.AdminToken, putConfigPluginInstance(deps)))

	// Embedded viewer. Intentionally NOT behind authMW — the page has
	// to load to prompt the user for their token. All AJAX calls the
	// page makes hit the authed /api/* routes above.
	mux.Handle("GET /viewer/", viewer.Handler("/viewer"))

	// Root redirects to the viewer; old JSON pointer (M4) is gone now
	// that there's a real UI to send people to.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/viewer/", http.StatusFound)
	})

	return mux
}

// --- helpers used across handlers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string, extras ...map[string]string) {
	body := map[string]string{"error": code}
	for _, m := range extras {
		for k, v := range m {
			body[k] = v
		}
	}
	writeJSON(w, status, body)
}
