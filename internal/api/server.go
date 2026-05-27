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
	"time"

	"github.com/leoyun/api-log/internal/counters"
	"github.com/leoyun/api-log/internal/store/sqlite"
)

// Deps is the bag of process-wide handles the API handlers need.
type Deps struct {
	Store      *sqlite.Store
	Counters   *counters.Counters
	AdminToken string
	Version    string
	StartedAt  time.Time
}

// NewMux returns an http.Handler ready to mount on the API listener.
// All /api/* routes require the admin bearer; /healthz and / are
// authenticated too (operator dashboards need the token anyway).
func NewMux(deps Deps) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /healthz", authMW(deps.AdminToken, healthz(deps)))
	mux.Handle("GET /api/traces", authMW(deps.AdminToken, listTraces(deps)))
	mux.Handle("GET /api/traces/{id}", authMW(deps.AdminToken, getTrace(deps)))
	mux.Handle("GET /api/traces/{id}/replay", authMW(deps.AdminToken, replayPlaceholder()))
	mux.Handle("GET /{$}", authMW(deps.AdminToken, rootPointer(deps)))

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
