// Package api implements the read API surface from ARCHITECTURE § 6.
//
// Core endpoints:
//
//	GET /                       — JSON pointer to the viewer project
//	                              (binary ships no embedded HTML)
//	GET /healthz                — liveness + in-memory counter snapshot
//	GET /api/traces             — list, SQLite-backed, paginated
//	GET /api/traces/:id         — detail, SQLite + JSONL seek
//	GET /api/traces/:id/replay  — replay recorded SSE events
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/2nd1st/api-log/internal/counters"
	pluginv2 "github.com/2nd1st/api-log/internal/plugin/v2"
	"github.com/2nd1st/api-log/internal/storage"
	"github.com/2nd1st/api-log/internal/store/sqlite"
	"github.com/2nd1st/api-log/internal/viewerhost"
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

	// ExportByteHardCap overrides the default 2 GiB byte cap on
	// /api/export (v0.1.2). Set via APILOG_API_EXPORT_BYTE_HARDCAP
	// env or `api.export_byte_hardcap` YAML. 0 (zero value) keeps the
	// package default; a positive value replaces it; explicit -1 (or
	// any negative) disables the cap entirely (equivalent to
	// `?bytes_all=1` being the permanent state).
	ExportByteHardCap int64

	// StorageCoord (v0.1.1) is the storage coordinator. Nil-safe: the
	// export handler skips lease arbitration when coord is nil — fine
	// for tests / no-retention deployments. When non-nil, exporter
	// acquires a lease per (date, keyhash) bucket before reading the
	// JSONL so retention can't delete the file mid-stream.
	StorageCoord *storage.Coordinator

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

	// ViewerHost is the optional hosted-viewer surface. When non-nil,
	// NewMux registers `<ViewerPublicPath>/` to its handler OUTSIDE
	// the auth middleware (the viewer is an unauthenticated browser
	// load; the read API the viewer subsequently calls is still
	// bearer-gated). Nil-safe: tests and integrations that don't need
	// the viewer pass it as zero; the route stays unregistered and
	// healthz omits the `viewer` field.
	ViewerHost *viewerhost.Host

	// ViewerPublicPath is the URL prefix the viewer is mounted under.
	// Carried on Deps (instead of read off ViewerHost) so the route
	// prefix tracks the operator's APILOG_VIEWER_PUBLIC_PATH override
	// without coupling internal/api to viewerhost's accessor surface.
	// Empty defaults to "/viewer" at registration time.
	ViewerPublicPath string
}

// NewMux returns an http.Handler ready to mount on the API listener.
// /api/* requires the admin bearer; /healthz and GET / are unauthenticated
// so k8s liveness probes, alertmanager, and reverse-proxy health checks
// (none of which carry bearer tokens) can probe the binary cheaply.
// The whole mux is wrapped in recoverMW so a handler panic returns 500
// instead of taking down the proxy listener that shares this process.
func NewMux(deps Deps) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /healthz", healthz(deps))
	mux.Handle("GET /api/traces", authMW(deps.AdminToken, listTraces(deps)))
	mux.Handle("GET /api/traces/{id}", authMW(deps.AdminToken, getTrace(deps)))
	mux.Handle("GET /api/traces/{id}/replay", authMW(deps.AdminToken, replayHandler(deps)))
	mux.Handle("GET /api/sessions", authMW(deps.AdminToken, listSessions(deps)))
	mux.Handle("GET /api/export", authMW(deps.AdminToken, exportHandler(deps)))

	// Phase K media endpoints.
	mux.Handle("GET /api/media/{trace_id}/{idx}", authMW(deps.AdminToken, mediaHandler(deps)))
	mux.Handle("GET /api/config/media", authMW(deps.AdminToken, getConfigMedia(deps)))
	mux.Handle("PUT /api/config/media", authMW(deps.AdminToken, putConfigMedia(deps)))
	mux.Handle("GET /api/config/retention", authMW(deps.AdminToken, getConfigRetention(deps)))
	mux.Handle("PUT /api/config/retention", authMW(deps.AdminToken, putConfigRetention(deps)))

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

	// Hosted viewer (optional). Registered OUTSIDE authMW: the viewer
	// is a browser asset load, and the read API it later calls is
	// still bearer-gated. The viewerhost.Host handler is responsible
	// for stripping the prefix and serving the cached `dist/` tree
	// (or returning 503 if fetch / verify failed). Default mount
	// `/viewer/` matches ViewerConfig.PublicPath; an operator override
	// flows through ViewerPublicPath on Deps so this stays in sync.
	if deps.ViewerHost != nil {
		prefix := deps.ViewerPublicPath
		if prefix == "" {
			prefix = "/viewer"
		}
		mux.Handle("GET "+prefix+"/", deps.ViewerHost.Handler())
	}

	return recoverMW(mux)
}

// recoverMW catches handler panics so a nil-deref in a single request
// cannot kill the binary — which would also kill the proxy listener
// running on the same process. Logs the panic + stack with slog and
// returns 500 server_panic to the client. The proxy listener runs on
// a separate http.Server and is untouched by this middleware.
func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("api handler panic",
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "server_panic")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// rootPointer answers GET / with a static JSON document pointing at
// the out-of-tree viewer project and naming the live mount paths on
// this binary. It is intentionally tiny and unauthenticated: a 200 OK
// from `curl http://host/` is the cheapest "is api-log up?" probe an
// operator has, and it gives a new adopter a single link to the UI
// repo without needing to read the README first.
func rootPointer(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"viewer":  "https://github.com/2nd1st/api-log-viewer",
		"api":     "/api",
		"healthz": "/healthz",
		"docs":    "see ARCHITECTURE.md in this repo",
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
