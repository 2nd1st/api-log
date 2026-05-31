package viewerhost

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// helper: build an in-memory zip from name->content pairs.
func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := io.WriteString(f, content); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func hexSha256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fakeGitHub spins up an httptest.Server that serves both the
// release-by-tag JSON endpoint and the asset bytes. It increments
// hits so tests can assert "did we hit the network at all?".
type fakeGitHub struct {
	srv      *httptest.Server
	hits     int64
	apiHits  int64
	zipBytes []byte
}

func newFakeGitHub(t *testing.T, repo, version string, zipBytes []byte) *fakeGitHub {
	t.Helper()
	fg := &fakeGitHub{zipBytes: zipBytes}
	mux := http.NewServeMux()
	apiPath := "/repos/" + repo + "/releases/tags/" + version
	assetPath := "/download/" + version + "/dist.zip"

	mux.HandleFunc(apiPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&fg.apiHits, 1)
		atomic.AddInt64(&fg.hits, 1)
		// browser_download_url must point back at this same server so
		// the asset GET hits us too — that's what makes
		// "did we hit the network?" assertions work.
		downloadURL := fg.srv.URL + assetPath
		body := map[string]any{
			"assets": []map[string]string{
				{"name": "dist.zip", "browser_download_url": downloadURL},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc(assetPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&fg.hits, 1)
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(fg.zipBytes)
	})
	fg.srv = httptest.NewServer(mux)
	return fg
}

// withFakeGitHub swaps the package-level base URL and restores it
// on test end.
func withFakeGitHub(t *testing.T, fg *fakeGitHub) {
	t.Helper()
	prev := githubAPIBase
	githubAPIBase = fg.srv.URL
	t.Cleanup(func() {
		githubAPIBase = prev
		fg.srv.Close()
	})
}

func TestEnsureCache_HappyPath(t *testing.T) {
	zipBytes := buildZip(t, map[string]string{
		"index.html":     "<html>hi</html>",
		"assets/app.js":  "console.log(1)",
	})
	sum := hexSha256(zipBytes)
	fg := newFakeGitHub(t, "2nd1st/api-log-viewer", "v0.1.0", zipBytes)
	withFakeGitHub(t, fg)

	cfg := Config{
		Enabled:  true,
		Repo:     "2nd1st/api-log-viewer",
		Version:  "v0.1.0",
		Sha256:   sum,
		CacheDir: t.TempDir(),
	}
	root, outcome, err := ensureCache(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureCache: %v", err)
	}
	if outcome != outcomeFetched {
		t.Fatalf("first call outcome = %v, want fetched", outcome)
	}
	if _, err := os.Stat(filepath.Join(root, "index.html")); err != nil {
		t.Fatalf("expected index.html at root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "assets", "app.js")); err != nil {
		t.Fatalf("expected assets/app.js: %v", err)
	}
}

func TestEnsureCache_ShaMismatch_NoCacheWritten(t *testing.T) {
	zipBytes := buildZip(t, map[string]string{
		"index.html": "<html>hi</html>",
	})
	// Wrong sha on purpose.
	wrong := strings.Repeat("a", 64)
	fg := newFakeGitHub(t, "2nd1st/api-log-viewer", "v0.1.0", zipBytes)
	withFakeGitHub(t, fg)

	cacheDir := t.TempDir()
	cfg := Config{
		Enabled:  true,
		Repo:     "2nd1st/api-log-viewer",
		Version:  "v0.1.0",
		Sha256:   wrong,
		CacheDir: cacheDir,
	}
	_, _, err := ensureCache(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected sha256 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("error %q does not mention sha256 mismatch", err.Error())
	}
	// No dist directory should exist.
	key := cacheKey(wrong)
	if _, err := os.Stat(filepath.Join(cacheDir, key, "dist")); !os.IsNotExist(err) {
		t.Fatalf("expected no dist dir on mismatch, stat err = %v", err)
	}
	// No tmp file should linger.
	if _, err := os.Stat(filepath.Join(cacheDir, ".tmp-"+key+".zip")); !os.IsNotExist(err) {
		t.Fatalf("expected no tmp zip, stat err = %v", err)
	}
}

func TestEnsureCache_ZipSlipRejected(t *testing.T) {
	// Build a zip that tries to escape via ../evil.txt.
	zipBytes := buildZip(t, map[string]string{
		"../evil.txt": "pwned",
		"index.html":  "<html>hi</html>",
	})
	sum := hexSha256(zipBytes)
	fg := newFakeGitHub(t, "2nd1st/api-log-viewer", "v0.1.0", zipBytes)
	withFakeGitHub(t, fg)

	cacheDir := t.TempDir()
	cfg := Config{
		Enabled:  true,
		Repo:     "2nd1st/api-log-viewer",
		Version:  "v0.1.0",
		Sha256:   sum,
		CacheDir: cacheDir,
	}
	_, _, err := ensureCache(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected zip-slip rejection, got nil")
	}
	// The evil file must NOT have been written anywhere under cacheDir
	// or its parent.
	parent := filepath.Dir(cacheDir)
	if _, err := os.Stat(filepath.Join(parent, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("evil.txt leaked outside cache dir: stat err = %v", err)
	}
}

func TestEnsureCache_CacheHitNoNetwork(t *testing.T) {
	zipBytes := buildZip(t, map[string]string{
		"index.html": "<html>hi</html>",
	})
	sum := hexSha256(zipBytes)
	fg := newFakeGitHub(t, "2nd1st/api-log-viewer", "v0.1.0", zipBytes)
	withFakeGitHub(t, fg)

	cacheDir := t.TempDir()
	cfg := Config{
		Enabled:  true,
		Repo:     "2nd1st/api-log-viewer",
		Version:  "v0.1.0",
		Sha256:   sum,
		CacheDir: cacheDir,
	}
	// First call populates the cache and hits the network.
	if _, _, err := ensureCache(context.Background(), cfg); err != nil {
		t.Fatalf("first ensureCache: %v", err)
	}
	hitsAfterFirst := atomic.LoadInt64(&fg.hits)
	if hitsAfterFirst == 0 {
		t.Fatal("expected first call to hit the network at least once")
	}

	// Second call must serve from cache without touching the network.
	// To make that assertion airtight, swap the base URL to a server
	// that fails any request.
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected network call on cache hit: %s %s", r.Method, r.URL.Path)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer fail.Close()
	prev := githubAPIBase
	githubAPIBase = fail.URL
	defer func() { githubAPIBase = prev }()

	root, outcome, err := ensureCache(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second ensureCache: %v", err)
	}
	if outcome != outcomeCacheHit {
		t.Fatalf("second call outcome = %v, want cache hit", outcome)
	}
	if _, err := os.Stat(filepath.Join(root, "index.html")); err != nil {
		t.Fatalf("expected index.html at root: %v", err)
	}
}

func TestEnsureCache_NoDistZipAsset(t *testing.T) {
	// Release exists but has no dist.zip.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/2nd1st/api-log-viewer/releases/tags/v0.1.0",
		func(w http.ResponseWriter, r *http.Request) {
			body := map[string]any{
				"assets": []map[string]string{
					{"name": "source.tar.gz", "browser_download_url": "https://example.invalid/x"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	prev := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = prev }()

	cfg := Config{
		Enabled:  true,
		Repo:     "2nd1st/api-log-viewer",
		Version:  "v0.1.0",
		Sha256:   strings.Repeat("a", 64),
		CacheDir: t.TempDir(),
	}
	_, _, err := ensureCache(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "no dist.zip asset") {
		t.Fatalf("expected no-dist.zip error, got %v", err)
	}
}

func TestExtractZip_FlattensDistPrefix(t *testing.T) {
	zipBytes := buildZip(t, map[string]string{
		"dist/index.html":    "<html>hi</html>",
		"dist/assets/app.js": "x",
	})
	tmpZip := filepath.Join(t.TempDir(), "in.zip")
	if err := os.WriteFile(tmpZip, zipBytes, 0o644); err != nil {
		t.Fatalf("write tmp zip: %v", err)
	}
	distRoot := filepath.Join(t.TempDir(), "dist")
	if err := extractZip(tmpZip, distRoot); err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(distRoot, "index.html")); err != nil {
		t.Fatalf("expected flattened index.html: %v", err)
	}
	if _, err := os.Stat(filepath.Join(distRoot, "assets", "app.js")); err != nil {
		t.Fatalf("expected flattened assets/app.js: %v", err)
	}
}
