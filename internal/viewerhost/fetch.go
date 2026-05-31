package viewerhost

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// githubAPIBase is the GitHub REST v3 base URL. Tests reassign this to
// point at a local httptest.Server. Keep it unexported so external
// consumers cannot rebind it.
var githubAPIBase = "https://api.github.com"

// httpClient is the fetch client. Tests may swap timeouts via the
// fetcher if needed; for normal operation 30s covers a slow GitHub
// + a slow CDN.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// fetchOutcome describes what ensureCache actually did so that the
// caller can populate Info.Source.
type fetchOutcome int

const (
	outcomeCacheHit fetchOutcome = iota
	outcomeFetched
)

// ensureCache makes sure a verified viewer dist tree exists on disk
// and returns the path to it. It is idempotent — repeat calls with
// the same Sha256 return the same directory without touching the
// network.
//
// Steps (the order matters for the no-partial-write guarantee):
//  1. Compute cache key from cfg.Sha256.
//  2. If <CacheDir>/<key>/dist/index.html exists, return cache hit.
//  3. GET <githubAPIBase>/repos/<Repo>/releases/tags/<Version>.
//  4. Find the "dist.zip" asset; download browser_download_url.
//  5. Stream the body into a temp file under CacheDir while hashing.
//  6. If sha256 mismatches cfg.Sha256, delete the temp file and
//     return an error WITHOUT creating <key>/dist/.
//  7. On match, extract the zip into <key>/dist/ with zip-slip
//     protection. Delete the temp file.
func ensureCache(ctx context.Context, cfg Config) (distRoot string, outcome fetchOutcome, err error) {
	if cfg.Sha256 == "" {
		return "", 0, errors.New("viewerhost: empty sha256")
	}
	if cfg.Repo == "" || cfg.Version == "" {
		return "", 0, errors.New("viewerhost: empty repo or version")
	}
	if cfg.CacheDir == "" {
		return "", 0, errors.New("viewerhost: empty cache dir")
	}

	key := cacheKey(cfg.Sha256)
	distRoot = filepath.Join(cfg.CacheDir, key, "dist")

	// (2) cache hit?
	if fi, err := os.Stat(filepath.Join(distRoot, "index.html")); err == nil && !fi.IsDir() {
		return distRoot, outcomeCacheHit, nil
	}

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return "", 0, fmt.Errorf("viewerhost: mkdir cache: %w", err)
	}

	// (3) resolve release.
	assetURL, err := resolveAssetURL(ctx, cfg.Repo, cfg.Version)
	if err != nil {
		return "", 0, err
	}

	// (4-5) download + hash into a temp file.
	tmpFile := filepath.Join(cfg.CacheDir, ".tmp-"+key+".zip")
	gotSum, err := downloadAndHash(ctx, assetURL, tmpFile)
	if err != nil {
		_ = os.Remove(tmpFile)
		return "", 0, err
	}

	wantSum := strings.ToLower(strings.TrimSpace(cfg.Sha256))
	if gotSum != wantSum {
		_ = os.Remove(tmpFile)
		return "", 0, fmt.Errorf("viewerhost: sha256 mismatch: got %s want %s", gotSum, wantSum)
	}

	// (7) extract.
	if err := extractZip(tmpFile, distRoot); err != nil {
		// Best-effort cleanup of any partial extract.
		_ = os.RemoveAll(filepath.Join(cfg.CacheDir, key))
		_ = os.Remove(tmpFile)
		return "", 0, err
	}
	_ = os.Remove(tmpFile)

	return distRoot, outcomeFetched, nil
}

// cacheKey returns the first 16 hex chars of cfg.Sha256, normalized
// to lower case. 16 hex chars = 64 bits of collision resistance,
// plenty for "tell two viewer drops apart on disk" purposes. The
// ARCHITECTURE doc shows `<sha256-first-8>`; we use 16 because the
// extra entropy costs nothing.
func cacheKey(sha string) string {
	s := strings.ToLower(strings.TrimSpace(sha))
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

// resolveAssetURL hits the GitHub releases-by-tag API and returns the
// browser_download_url of the "dist.zip" asset.
func resolveAssetURL(ctx context.Context, repo, version string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/tags/%s",
		strings.TrimRight(githubAPIBase, "/"), repo, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("viewerhost: build release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "api-log/viewerhost")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("viewerhost: github release fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("viewerhost: github release fetch: 403 (rate limited or private repo) for %s", url)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("viewerhost: github release fetch: status %d for %s", resp.StatusCode, url)
	}

	var release struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("viewerhost: decode release json: %w", err)
	}
	for _, a := range release.Assets {
		if a.Name == "dist.zip" {
			if a.URL == "" {
				return "", fmt.Errorf("viewerhost: dist.zip asset has empty browser_download_url")
			}
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("viewerhost: release %s has no dist.zip asset", version)
}

// downloadAndHash streams url into dst and returns the lowercase hex
// sha256. The body is hashed in lockstep with the disk write so we
// never have to re-read the file. Caller is responsible for deleting
// dst on error.
func downloadAndHash(ctx context.Context, url, dst string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("viewerhost: build asset request: %w", err)
	}
	req.Header.Set("User-Agent", "api-log/viewerhost")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("viewerhost: asset download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("viewerhost: asset download: status %d for %s", resp.StatusCode, url)
	}

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("viewerhost: open tmp file: %w", err)
	}
	h := sha256.New()
	mw := io.MultiWriter(f, h)
	if _, err := io.Copy(mw, resp.Body); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("viewerhost: stream asset: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("viewerhost: close tmp file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractZip unpacks src into distRoot. Rejects entries with absolute
// paths, parent-directory escapes, or any path that, after Join +
// Clean, falls outside distRoot. Directory entries are mkdir'd.
//
// We strip a single leading "dist/" segment if every entry has one,
// so a zip that wraps its files in "dist/index.html" lands at
// "<distRoot>/index.html" rather than "<distRoot>/dist/index.html".
// This makes us tolerant of both "flat" and "dist-wrapped" release
// archives without forcing the upstream to commit to one shape.
func extractZip(src, distRoot string) error {
	rc, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("viewerhost: open zip: %w", err)
	}
	defer rc.Close()

	if err := os.MkdirAll(distRoot, 0o755); err != nil {
		return fmt.Errorf("viewerhost: mkdir dist root: %w", err)
	}
	cleanRoot := filepath.Clean(distRoot)

	// Detect "all entries share dist/ prefix" so we can flatten.
	stripDist := allHaveDistPrefix(rc.File)

	for _, zf := range rc.File {
		name := zf.Name
		if stripDist {
			name = strings.TrimPrefix(name, "dist/")
			if name == "" {
				continue
			}
		}
		// Defense against zip-slip:
		//   - absolute paths
		//   - any ".." anywhere
		//   - joined target outside cleanRoot
		if filepath.IsAbs(name) || strings.Contains(name, "..") {
			return fmt.Errorf("viewerhost: zip entry rejected (escape): %q", zf.Name)
		}
		target := filepath.Join(distRoot, name)
		cleanTarget := filepath.Clean(target)
		if cleanTarget != cleanRoot && !strings.HasPrefix(cleanTarget, cleanRoot+string(os.PathSeparator)) {
			return fmt.Errorf("viewerhost: zip entry rejected (outside root): %q", zf.Name)
		}

		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(cleanTarget, 0o755); err != nil {
				return fmt.Errorf("viewerhost: mkdir %q: %w", name, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(cleanTarget), 0o755); err != nil {
			return fmt.Errorf("viewerhost: mkdir parent of %q: %w", name, err)
		}
		if err := writeZipEntry(zf, cleanTarget); err != nil {
			return err
		}
	}
	return nil
}

func writeZipEntry(zf *zip.File, target string) error {
	in, err := zf.Open()
	if err != nil {
		return fmt.Errorf("viewerhost: open zip entry %q: %w", zf.Name, err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("viewerhost: create %q: %w", target, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("viewerhost: write %q: %w", target, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("viewerhost: close %q: %w", target, err)
	}
	return nil
}

func allHaveDistPrefix(files []*zip.File) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		if f.Name == "dist/" {
			continue
		}
		if !strings.HasPrefix(f.Name, "dist/") {
			return false
		}
	}
	return true
}
