package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2nd1st/api-log/internal/counters"
	"github.com/2nd1st/api-log/internal/runtime"
	"github.com/2nd1st/api-log/internal/storage"
	"github.com/2nd1st/api-log/internal/store/sqlite"
)

// newRetentionServer builds an api server wired with a real
// storage.Coordinator + sqlite store + fresh data dir — exercises the
// full PUT/persist/GET round-trip without depending on writer/proxy
// surface area.
func newRetentionServer(t *testing.T) (*httptest.Server, *storage.Coordinator, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	coord, err := storage.New(storage.Config{DataDir: dir, WarnAtPercent: 80}, store, counters.New())
	if err != nil {
		t.Fatal(err)
	}

	mux := NewMux(Deps{
		Store:        store,
		Counters:     counters.New(),
		AdminToken:   "tok-test",
		Version:      "test",
		DataDir:      dir,
		StorageCoord: coord,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, coord, dir
}

func TestGetRetentionDefault(t *testing.T) {
	srv, _, _ := newRetentionServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/config/retention", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got retentionConfigJSON
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.MaxBytes != 0 || got.MaxAgeDays != 0 {
		t.Errorf("default thresholds non-zero: %+v", got)
	}
	if got.Source != "yaml" {
		t.Errorf("source = %q, want yaml (no override)", got.Source)
	}
}

func TestPutRetentionPersistsAndApplies(t *testing.T) {
	srv, coord, dir := newRetentionServer(t)

	body := `{"max_bytes": 5000000000, "max_age_days": 14, "warn_at_percent": 75}`
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/config/retention",
		bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer tok-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}

	// Coord state immediately reflects new thresholds.
	s := coord.Status()
	if s.MaxBytes != 5_000_000_000 || s.MaxAgeDays != 14 {
		t.Errorf("coord.Status post-PUT = %+v, want MaxBytes=5e9 MaxAgeDays=14", s)
	}

	// Override file persisted with both knobs + warn percent.
	ov, err := runtime.LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Retention == nil {
		t.Fatal("retention override missing from disk")
	}
	if ov.Retention.MaxBytes == nil || *ov.Retention.MaxBytes != 5_000_000_000 {
		t.Errorf("persisted max_bytes = %v, want 5e9", ov.Retention.MaxBytes)
	}
	if ov.Retention.MaxAgeDays == nil || *ov.Retention.MaxAgeDays != 14 {
		t.Errorf("persisted max_age_days = %v, want 14", ov.Retention.MaxAgeDays)
	}
	if ov.Retention.WarnAtPercent == nil || *ov.Retention.WarnAtPercent != 75 {
		t.Errorf("persisted warn_at_percent = %v, want 75", ov.Retention.WarnAtPercent)
	}

	// Source flips to "override" on the next GET.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/config/retention", nil)
	req2.Header.Set("Authorization", "Bearer tok-test")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var got retentionConfigJSON
	_ = json.NewDecoder(resp2.Body).Decode(&got)
	if got.Source != "override" {
		t.Errorf("source = %q, want override", got.Source)
	}
}

func TestPutRetentionBothKnobsZeroDisables(t *testing.T) {
	srv, coord, _ := newRetentionServer(t)
	// First enable.
	first := `{"max_bytes": 1000000, "max_age_days": 1}`
	resp, err := http.DefaultClient.Do(authedPut(srv, first))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if coord.Status().MaxBytes == 0 {
		t.Fatal("first PUT did not enable retention")
	}

	// Now disable.
	off := `{"max_bytes": 0, "max_age_days": 0}`
	resp2, err := http.DefaultClient.Do(authedPut(srv, off))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("disable PUT status = %d", resp2.StatusCode)
	}
	s := coord.Status()
	if s.State != "disabled" {
		t.Errorf("post-disable State = %q, want disabled", s.State)
	}
}

func TestPutRetentionRejectsInvalid(t *testing.T) {
	srv, _, _ := newRetentionServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"negative max_bytes", `{"max_bytes": -1}`},
		{"negative max_age_days", `{"max_age_days": -1}`},
		{"warn over 100", `{"warn_at_percent": 150}`},
		{"unknown field", `{"unrelated": 1}`},
		{"non-json", `not even json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.DefaultClient.Do(authedPut(srv, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, body = %s; want 400", resp.StatusCode, body)
			}
		})
	}
}

func TestRetentionEndpoint503WhenCoordMissing(t *testing.T) {
	dir := t.TempDir()
	store, _ := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	t.Cleanup(func() { _ = store.Close() })

	mux := NewMux(Deps{
		Store:      store,
		Counters:   counters.New(),
		AdminToken: "tok-test",
		Version:    "test",
		DataDir:    dir,
		// StorageCoord intentionally nil.
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/config/retention", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("GET status = %d, body = %s; want 503", resp.StatusCode, raw)
	}
}

func authedPut(srv *httptest.Server, body string) *http.Request {
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/config/retention",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-test")
	req.Header.Set("Content-Type", "application/json")
	return req
}
