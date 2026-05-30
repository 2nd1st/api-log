// Package api implements the read API surface from ARCHITECTURE § 6.
//
// Core endpoints:
//   GET /                       — JSON pointer to the viewer project
//                                 (binary ships no embedded HTML)
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
	pluginv2 "github.com/leoyun/api-log/internal/plugin/v2"
	"github.com/leoyun/api-log/internal/store/sqlite"
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

	// PluginV2Reg is the live v2 hook-plugin registry. PUT/DELETE
	// /api/config/plugins handlers call PluginV2Reg.Reload after a
	// successful SaveOverride so the next request sees the new config
	// without a process restart (W4.2). Nil-safe: handlers that test
	// the persistence layer in isolation can pass Deps without it; the
	// handler then skips the Reload step and behaves as a pure
	// persist-only endpoint.
	PluginV2Reg *pluginv2.Registry

	// YAMLPlugins is the pre-override plugin instance list compiled from
	// config.yaml at process start. The hot-reload path needs it so
	// Reload can merge against YAML defaults the same way startup did
	// — passing the merged effective list would double-apply the
	// override on each Reload. In v0 the YAML side carries no v2
	// instances, so main.go passes nil; the merge semantics in
	// pluginv2.Load treat that correctly.
	YAMLPlugins []pluginv2.InstanceConfig
}

// NewMux returns an http.Handler ready to mount on the API listener.
// /api/* and /healthz require the admin bearer; GET / is unauthenticated
// so a reverse proxy or operator dashboard can probe liveness without
// holding a token.
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

	// Root returns a small JSON pointer to the out-of-tree viewer
	// project. The binary itself ships no HTML; operators point a
	// reverse proxy (caddy, nginx, etc.) at the api-log-viewer build
	// output and mount api-log under /api on the same origin.
	mux.HandleFunc("GET /{$}", rootPointer)

	return mux
}

// rootPointer answers GET / with a static JSON document pointing at
// the out-of-tree viewer project and naming the live mount paths on
// this binary. It is intentionally tiny and unauthenticated: a 200 OK
// from `curl http://host/` is the cheapest "is api-log up?" probe an
// operator has, and it gives a new adopter a single link to the UI
// repo without needing to read the README first.
func rootPointer(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"viewer":  "https://github.com/leoyun/api-log-viewer",
		"api":     "/api",
		"healthz": "/healthz",
		"docs":    "see PHILOSOPHY.md and ARCHITECTURE.md in this repo",
	})
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
