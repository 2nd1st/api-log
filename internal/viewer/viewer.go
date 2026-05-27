// Package viewer serves the embedded single-file UI at /viewer/. The
// HTML lives in static/; it is one file, no build step, no CDN, no
// framework — by design, per the project's restraint principle. Future
// expansions (analytics, skill-usage views) belong in a separate richer
// frontend project; this one is the bundled, zero-friction default.
package viewer

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Handler returns an http.Handler that serves the viewer at the
// given URL prefix. The prefix should NOT end in a slash; callers
// register it as `mux.Handle(prefix+"/", viewer.Handler(prefix))`.
//
// The handler is intentionally unauthenticated — the HTML must load
// to ask the user for their admin token. All subsequent /api/* calls
// go through the auth middleware as usual.
func Handler(prefix string) http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Compile-time embed guarantees this directory exists; if it
		// doesn't, panic at startup is the right failure mode.
		panic(err)
	}
	fs := http.FileServer(http.FS(sub))
	return http.StripPrefix(prefix, fs)
}
