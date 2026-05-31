package viewerhost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLocalDist materializes a fake viewer dist tree for the
// local-path test path.
func writeLocalDist(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"),
		[]byte("<html>local</html>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "foo.js"),
		[]byte("console.log('foo')"), 0o644); err != nil {
		t.Fatalf("write foo.js: %v", err)
	}
	return root
}

func TestHost_Disabled(t *testing.T) {
	h := New(context.Background(), Config{
		Enabled:    false,
		PublicPath: "/viewer",
	})
	if got := h.Info().Source; got != "disabled" {
		t.Fatalf("Info.Source = %q, want disabled", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/viewer/", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "disabled" {
		t.Fatalf("body.error = %q, want disabled", body["error"])
	}
	if _, ok := body["detail"]; ok {
		t.Fatalf("disabled body should omit detail, got %v", body)
	}
}

func TestHost_LocalPath_ServesFiles(t *testing.T) {
	root := writeLocalDist(t)
	h := New(context.Background(), Config{
		Enabled:    true,
		LocalPath:  root,
		PublicPath: "/viewer",
	})
	if got := h.Info().Source; got != "local" {
		t.Fatalf("Info.Source = %q, want local", got)
	}

	// Request /viewer/ → should get index.html.
	{
		req := httptest.NewRequest(http.MethodGet, "/viewer/", nil)
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("/viewer/ status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "local") {
			t.Fatalf("/viewer/ body = %q, want index.html content", w.Body.String())
		}
	}

	// Request /viewer/assets/foo.js → asset.
	{
		req := httptest.NewRequest(http.MethodGet, "/viewer/assets/foo.js", nil)
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("/viewer/assets/foo.js status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "console.log") {
			t.Fatalf("asset body = %q, want JS", w.Body.String())
		}
	}
}

func TestHost_LocalPath_Missing(t *testing.T) {
	// Point at an empty directory — no index.html.
	root := t.TempDir()
	h := New(context.Background(), Config{
		Enabled:    true,
		LocalPath:  root,
		PublicPath: "/viewer",
	})
	if got := h.Info().Source; got != "error" {
		t.Fatalf("Info.Source = %q, want error", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/viewer/", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "viewer_unavailable" {
		t.Fatalf("body.error = %q, want viewer_unavailable", body["error"])
	}
	if body["detail"] == "" {
		t.Fatal("expected detail to be populated for error mode")
	}
}

func TestHost_InitError_ShaMismatch(t *testing.T) {
	// Wire a real GitHub-like server that serves a dist.zip, then ask
	// the host to verify against the wrong sha. New() must record an
	// error and Handler() must 503 with viewer_unavailable + detail.
	zipBytes := buildZip(t, map[string]string{
		"index.html": "<html>x</html>",
	})
	fg := newFakeGitHub(t, "2nd1st/api-log-viewer", "v0.1.0", zipBytes)
	withFakeGitHub(t, fg)

	wrong := strings.Repeat("b", 64)
	h := New(context.Background(), Config{
		Enabled:    true,
		Repo:       "2nd1st/api-log-viewer",
		Version:    "v0.1.0",
		Sha256:     wrong,
		CacheDir:   t.TempDir(),
		PublicPath: "/viewer",
	})

	info := h.Info()
	if info.Source != "error" {
		t.Fatalf("Info.Source = %q, want error", info.Source)
	}
	if !strings.Contains(info.Error, "sha256 mismatch") {
		t.Fatalf("Info.Error = %q, want sha256 mismatch substring", info.Error)
	}

	req := httptest.NewRequest(http.MethodGet, "/viewer/", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "viewer_unavailable" {
		t.Fatalf("body.error = %q, want viewer_unavailable", body["error"])
	}
	if !strings.Contains(body["detail"], "sha256 mismatch") {
		t.Fatalf("body.detail = %q, want sha256 mismatch substring", body["detail"])
	}
}

func TestHost_InitError_MissingPinning(t *testing.T) {
	h := New(context.Background(), Config{
		Enabled:    true,
		Repo:       "",
		Version:    "",
		Sha256:     "",
		CacheDir:   t.TempDir(),
		PublicPath: "/viewer",
	})
	if got := h.Info().Source; got != "error" {
		t.Fatalf("Info.Source = %q, want error", got)
	}
	if !strings.Contains(h.Info().Error, "missing") {
		t.Fatalf("Info.Error = %q, want 'missing' substring", h.Info().Error)
	}
}

func TestHost_FetchedFromFakeGitHub(t *testing.T) {
	zipBytes := buildZip(t, map[string]string{
		"index.html":    "<html>fetched</html>",
		"assets/app.js": "let x=1",
	})
	sum := hexSha256(zipBytes)
	fg := newFakeGitHub(t, "2nd1st/api-log-viewer", "v0.1.0", zipBytes)
	withFakeGitHub(t, fg)

	h := New(context.Background(), Config{
		Enabled:    true,
		Repo:       "2nd1st/api-log-viewer",
		Version:    "v0.1.0",
		Sha256:     sum,
		CacheDir:   t.TempDir(),
		PublicPath: "/viewer",
	})
	if got := h.Info().Source; got != "fetched" {
		t.Fatalf("Info.Source = %q, want fetched", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/viewer/assets/app.js", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(got), "let x=1") {
		t.Fatalf("asset body = %q", string(got))
	}
}
