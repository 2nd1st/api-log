// Package viewerhost fetches, verifies, caches, and serves the
// api-log-viewer single-page bundle at a local URL prefix.
//
// The backend SOURCE pins (via Config wired by package config) both
// the viewer version and the expected SHA-256 of dist.zip. ensureCache
// will refuse to write anything that does not match the configured
// hash — the trust model is "GitHub release asset hash matches what
// the binary was compiled to expect, or we serve 503." There is no
// auto-update, no floating-latest, no fallback URL.
//
// Operators can opt out (Enabled=false), point at a different repo
// or version (with a matching Sha256), or skip the fetch entirely by
// pointing LocalPath at an extracted dist tree (offline mode). If an
// operator overrides Repo or Version without supplying a matching
// Sha256, this package returns 503 with detail — the binary stays up
// so /api still serves.
package viewerhost

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Config is the data shape Agent 3 will wire from internal/config.
// All fields are required when Enabled=true and LocalPath="".
type Config struct {
	Enabled    bool   // default true; if false, Handler always 503s
	Repo       string // e.g. "2nd1st/api-log-viewer"
	Version    string // tag, e.g. "v0.1.0"
	Sha256     string // hex-encoded SHA-256 of dist.zip
	CacheDir   string // e.g. "<data_dir>/viewer-cache"
	LocalPath  string // if non-empty, skip fetch and serve from this dir
	PublicPath string // URL prefix, e.g. "/viewer"

	// ReleasesAPIBase points the dist.zip fetch at a non-GitHub
	// releases endpoint. Default empty → https://api.github.com. Set
	// to e.g. "http://gitea.homelab.lan/api/v1" to mirror viewer
	// releases on an internal Gitea (the JSON shape Gitea returns is
	// GitHub-compatible — `assets[].browser_download_url`).
	//
	// Operators who run their own artifact store can serve viewer
	// dists from there without forking the binary.
	ReleasesAPIBase string

	// ReleasesAuthToken is sent as `Authorization: token <value>` on
	// the release-metadata GET and the asset download. Empty means
	// "no auth" (public GitHub releases). Internal Gitea repos need
	// a token; the value is sensitive and should be sourced from env
	// or a secrets file (NOT committed to the YAML config).
	ReleasesAuthToken string
}

// Info is the snapshot reported via Host.Info() and embedded into
// /healthz output. JSON tags match what the README documents.
type Info struct {
	Enabled    bool   `json:"enabled"`
	Repo       string `json:"repo,omitempty"`
	Version    string `json:"version,omitempty"`
	Source     string `json:"source"` // "cache" | "fetched" | "local" | "disabled" | "error"
	Sha256     string `json:"sha256,omitempty"`
	Error      string `json:"error,omitempty"`
	PublicPath string `json:"public_path,omitempty"`
}

// Host is the runtime view of the viewer mount. Construct with New;
// query with Info; mount with Handler. It is safe for concurrent use
// — initState is set once during New, and the http.Handler returned
// by Handler is stateless beyond an immutable pointer back here.
type Host struct {
	cfg Config

	mu       sync.RWMutex
	distRoot string
	info     Info
	fileH    http.Handler // pre-wired StripPrefix + FileServer; nil if not ready
}

// New constructs a Host. It performs init (cache check / extract /
// local path stat) synchronously but never returns nil — on failure
// the Host reports source="error" via Info() and the Handler returns
// 503 with detail. Failures here do not bring down the binary.
func New(ctx context.Context, cfg Config) *Host {
	h := &Host{cfg: cfg}
	h.info = Info{
		Enabled:    cfg.Enabled,
		Repo:       cfg.Repo,
		Version:    cfg.Version,
		Sha256:     cfg.Sha256,
		PublicPath: cfg.PublicPath,
	}

	if !cfg.Enabled {
		h.info.Source = "disabled"
		return h
	}

	if cfg.LocalPath != "" {
		root := filepath.Clean(cfg.LocalPath)
		if !looksLikeDistRoot(root) {
			h.info.Source = "error"
			h.info.Error = "local_path missing index.html: " + root
			slog.Warn("viewerhost: local_path missing index.html",
				"path", root)
			return h
		}
		h.distRoot = root
		h.info.Source = "local"
		h.fileH = buildFileHandler(cfg.PublicPath, root)
		slog.Info("viewerhost: serving from local path",
			"path", root, "public_path", cfg.PublicPath)
		return h
	}

	// Operator-override sanity: if Repo or Version is empty here, we
	// were misconfigured. Pinned defaults SHOULD have been supplied
	// upstream; this is the last-resort signal.
	if cfg.Repo == "" || cfg.Version == "" || cfg.Sha256 == "" {
		h.info.Source = "error"
		h.info.Error = "missing repo, version, or sha256"
		slog.Warn("viewerhost: missing pinning",
			"repo", cfg.Repo, "version", cfg.Version,
			"sha256_set", cfg.Sha256 != "")
		return h
	}

	root, outcome, err := ensureCache(ctx, cfg)
	if err != nil {
		h.info.Source = "error"
		h.info.Error = err.Error()
		// sha256 mismatches and missing assets are ERROR; transport
		// blips are WARN. ensureCache wraps both as plain errors, so
		// classify by substring once — better than letting both look
		// the same in operator logs.
		if strings.Contains(err.Error(), "sha256 mismatch") {
			slog.Error("viewerhost: sha256 mismatch — refusing to serve",
				"err", err.Error(),
				"repo", cfg.Repo, "version", cfg.Version,
				"want_sha256", cfg.Sha256)
		} else {
			slog.Warn("viewerhost: cache init failed",
				"err", err.Error(),
				"repo", cfg.Repo, "version", cfg.Version,
				"cache_dir", cfg.CacheDir)
		}
		return h
	}

	h.distRoot = root
	h.fileH = buildFileHandler(cfg.PublicPath, root)
	switch outcome {
	case outcomeCacheHit:
		h.info.Source = "cache"
		slog.Info("viewerhost: cache hit",
			"repo", cfg.Repo, "version", cfg.Version,
			"dist_root", root, "public_path", cfg.PublicPath)
	case outcomeFetched:
		h.info.Source = "fetched"
		slog.Info("viewerhost: fetched and extracted",
			"repo", cfg.Repo, "version", cfg.Version,
			"dist_root", root, "public_path", cfg.PublicPath)
	}
	return h
}

// Info returns a snapshot of the current Host state. Safe to call
// from any goroutine.
func (h *Host) Info() Info {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.info
}

// Handler returns an http.Handler that serves the viewer at
// cfg.PublicPath/*. On any failure mode it returns 503 with a JSON
// body shaped like {"error":"...","detail":"..."} — matching the
// rest of api-log.
func (h *Host) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		info := h.info
		fileH := h.fileH
		h.mu.RUnlock()

		switch info.Source {
		case "disabled":
			writeViewerError(w, "disabled", "")
			return
		case "local", "cache", "fetched":
			if fileH != nil {
				fileH.ServeHTTP(w, r)
				return
			}
			// Defensive: state says ready but handler is nil.
			writeViewerError(w, "viewer_unavailable", "handler not initialized")
			return
		default:
			writeViewerError(w, "viewer_unavailable", info.Error)
			return
		}
	})
}

// buildFileHandler wraps an http.FileServer with the right prefix
// strip. PublicPath="/viewer" means a request for "/viewer/" reaches
// the FileServer as "/", and "/viewer/assets/x" reaches it as
// "/assets/x". We Clean the prefix so trailing slashes don't surprise
// http.StripPrefix.
func buildFileHandler(publicPath, distRoot string) http.Handler {
	prefix := strings.TrimRight(publicPath, "/")
	fs := http.FileServer(http.Dir(distRoot))
	if prefix == "" {
		return fs
	}
	return http.StripPrefix(prefix, fs)
}

// looksLikeDistRoot returns true if the path contains an index.html
// at its root. We deliberately do not require any specific set of
// other files — the SPA shape varies by build pipeline.
func looksLikeDistRoot(root string) bool {
	fi, err := os.Stat(filepath.Join(root, "index.html"))
	return err == nil && !fi.IsDir()
}

// writeViewerError emits the standard 503 envelope. Disabled mode
// omits "detail" so the body is the documented {"error":"disabled"}.
func writeViewerError(w http.ResponseWriter, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	body := map[string]string{"error": code}
	if detail != "" {
		body["detail"] = detail
	}
	_ = json.NewEncoder(w).Encode(body)
}
