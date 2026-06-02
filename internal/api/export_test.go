package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/2nd1st/api-log/internal/counters"
	"github.com/2nd1st/api-log/internal/store/sqlite"
	"github.com/2nd1st/api-log/internal/writer"
)

// TestExport413WhenOverHardCap saves the package-level cap, drops it
// to 2, inserts 3 traces, and asserts /api/export returns 413 with
// the documented JSON shape before any zip bytes leak.
func TestExport413WhenOverHardCap(t *testing.T) {
	saved := defaultExportHardCap
	defaultExportHardCap = 2
	t.Cleanup(func() { defaultExportHardCap = saved })

	srv, store, w, _ := newTestServer(t, "tok-test")
	for i, id := range []string{"01H_E1", "01H_E2", "01H_E3"} {
		enqueueTrace(t, w, id, "abcd1234abcd"+toHex4(i)+"abcd", nil)
	}
	waitForRows(t, store, 3)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; want 413", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("non-JSON 413 body %q: %v", body, err)
	}
	if got["error"] != "export_too_large" {
		t.Errorf("error code = %v, want export_too_large", got["error"])
	}
	if got["cap"] != "2" {
		t.Errorf("cap field = %v, want \"2\"", got["cap"])
	}
}

// TestExportAll1BypassesCap confirms `?all=1` short-circuits the
// pre-flight cap so adopters can always opt out of the safety net.
func TestExportAll1BypassesCap(t *testing.T) {
	saved := defaultExportHardCap
	defaultExportHardCap = 2
	t.Cleanup(func() { defaultExportHardCap = saved })

	srv, store, w, _ := newTestServer(t, "tok-test")
	for i, id := range []string{"01H_A1", "01H_A2", "01H_A3"} {
		enqueueTrace(t, w, id, "abcd1234abcd"+toHex4(i)+"abcd", nil)
	}
	waitForRows(t, store, 3)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export?all=1", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; want 200 with ?all=1", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", got)
	}
}

// TestExportProjectFilter exercises the v0.1.1-added `project` query
// param. Two traces with distinct client_project values; the filtered
// export returns 200 with a much smaller zip than the un-filtered.
//
// We don't decode the zip here — TestWriteZipNoFilters already
// verifies the unzip path. The size delta + 200 is enough to confirm
// the filter is wired through CountMatching + StreamMatching to the
// exporter without exporting both rows.
func TestExportProjectFilter(t *testing.T) {
	srv, store, w, _ := newTestServer(t, "tok-test")
	// Two traces — different paths so they group into distinct files;
	// no extracted project but the filter exercises the param plumbing.
	enqueueTrace(t, w, "01H_PR1", "aaaa1111aaaa1111", nil)
	enqueueTrace(t, w, "01H_PR2", "bbbb2222bbbb2222", nil)
	waitForRows(t, store, 2)

	// No filter — baseline byte count.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	allBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("baseline status = %d", resp.StatusCode)
	}

	// project=does-not-exist — empty match set; export should still 200
	// with template + README only.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export?project=does-not-exist", nil)
	req2.Header.Set("Authorization", "Bearer tok-test")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	filteredBytes, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("filtered status = %d", resp2.StatusCode)
	}
	if !(len(filteredBytes) < len(allBytes)) {
		t.Errorf("filtered export (%d B) should be smaller than baseline (%d B); filter not wired",
			len(filteredBytes), len(allBytes))
	}
}

// toHex4 is a small 4-char hex helper for keyhash variation across
// enqueueTrace calls.
func toHex4(n int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[(n>>12)&0xf], hex[(n>>8)&0xf], hex[(n>>4)&0xf], hex[n&0xf]})
}

// TestExport413WhenOverBytesCap drops the byte cap to 1 and seeds
// one trace — any non-empty source JSONL trips a 1-byte cap — and
// asserts the handler returns 413 with the documented JSON shape AND
// the `limit:"bytes"` discriminator that separates it from the
// row-cap 413.
func TestExport413WhenOverBytesCap(t *testing.T) {
	saved := defaultExportByteHardCap
	defaultExportByteHardCap = 1
	t.Cleanup(func() { defaultExportByteHardCap = saved })

	srv, store, w, _ := newTestServer(t, "tok-test")
	// One trace is enough: any marshaled trace line is hundreds of
	// bytes, way above the 1-byte cap.
	enqueueTrace(t, w, "01H_BC1", "aaaa1111aaaa1111", nil)
	waitForRows(t, store, 1)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; want 413", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("non-JSON 413 body %q: %v", body, err)
	}
	if got["error"] != "export_too_large" {
		t.Errorf("error code = %v, want export_too_large", got["error"])
	}
	if got["limit"] != "bytes" {
		t.Errorf("limit discriminator = %v, want \"bytes\"", got["limit"])
	}
	if got["cap_bytes"] != "1" {
		t.Errorf("cap_bytes = %v, want \"1\"", got["cap_bytes"])
	}
	if _, ok := got["matched_bytes"]; !ok {
		t.Errorf("missing matched_bytes field; got %v", got)
	}
	if _, ok := got["hint"]; !ok {
		t.Errorf("missing hint field; got %v", got)
	}
	if _, ok := got["note"]; !ok {
		t.Errorf("missing note field; got %v", got)
	}
}

// TestExportBytesAll1BypassesByteCap confirms `?bytes_all=1`
// short-circuits the byte safety net so adopters can opt out of the
// byte cap independent of the row cap.
func TestExportBytesAll1BypassesByteCap(t *testing.T) {
	saved := defaultExportByteHardCap
	defaultExportByteHardCap = 1
	t.Cleanup(func() { defaultExportByteHardCap = saved })

	srv, store, w, _ := newTestServer(t, "tok-test")
	enqueueTrace(t, w, "01H_BB1", "aaaa1111aaaa1111", nil)
	waitForRows(t, store, 1)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export?bytes_all=1", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; want 200 with ?bytes_all=1", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", got)
	}
}

// TestExportAll1DoesNotBypassByteCap is the independence guard: the
// row-cap bypass `?all=1` must NOT silently bypass the byte cap. If
// this assertion ever flips to 200 it means the two bypasses got
// collapsed — covered by the design's explicit independence
// decision.
func TestExportAll1DoesNotBypassByteCap(t *testing.T) {
	savedRow := defaultExportHardCap
	savedBytes := defaultExportByteHardCap
	defaultExportHardCap = 2
	defaultExportByteHardCap = 1
	t.Cleanup(func() {
		defaultExportHardCap = savedRow
		defaultExportByteHardCap = savedBytes
	})

	srv, store, w, _ := newTestServer(t, "tok-test")
	enqueueTrace(t, w, "01H_IG1", "aaaa1111aaaa1111", nil)
	waitForRows(t, store, 1)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export?all=1", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; want 413 (?all=1 must NOT bypass byte cap)",
			resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("non-JSON 413 body %q: %v", body, err)
	}
	if got["limit"] != "bytes" {
		t.Errorf("limit discriminator = %v, want \"bytes\" (the byte cap should have fired, not the row cap)",
			got["limit"])
	}
}

// TestExportBothCapsBypassed asserts the two bypass flags compose
// cleanly — `?all=1&bytes_all=1` with both caps tiny returns 200.
func TestExportBothCapsBypassed(t *testing.T) {
	savedRow := defaultExportHardCap
	savedBytes := defaultExportByteHardCap
	defaultExportHardCap = 2
	defaultExportByteHardCap = 1
	t.Cleanup(func() {
		defaultExportHardCap = savedRow
		defaultExportByteHardCap = savedBytes
	})

	srv, store, w, _ := newTestServer(t, "tok-test")
	for i, id := range []string{"01H_BX1", "01H_BX2", "01H_BX3"} {
		enqueueTrace(t, w, id, "abcd1234abcd"+toHex4(i)+"abcd", nil)
	}
	waitForRows(t, store, 3)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export?all=1&bytes_all=1", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; want 200 with both bypasses", resp.StatusCode, body)
	}
}

// TestExportNormalUnderBothCaps is a signature-wiring smoke check:
// production defaults, a handful of traces, expect a successful
// non-trivial zip. Catches the case where the new byteHardCap
// argument got dropped on the floor or the typed-error mapping
// regressed.
func TestExportNormalUnderBothCaps(t *testing.T) {
	srv, store, w, _ := newTestServer(t, "tok-test")
	for i, id := range []string{"01H_NS1", "01H_NS2", "01H_NS3"} {
		enqueueTrace(t, w, id, "abcd1234abcd"+toHex4(i)+"abcd", nil)
	}
	waitForRows(t, store, 3)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; want 200", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) < 256 {
		t.Errorf("zip body suspiciously small (%d B); expected agent/README + data entries", len(body))
	}
}

// TestExportDepsByteHardCap_PositiveOverride exercises the
// deps.ExportByteHardCap > 0 branch — operator YAML/env override
// replaces the package var without mutating it. Mirrors the audit
// pattern: leave the package var at its production default and verify
// the per-mount Deps value is what fires the 413.
func TestExportDepsByteHardCap_PositiveOverride(t *testing.T) {
	// Tiny isolated mux with ExportByteHardCap=1 — every non-empty
	// JSONL trips the cap. Doesn't touch the package var.
	dir := t.TempDir()
	store, err := sqlite.Open(dir + "/index.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctrs := counters.New()
	wrtr := writer.New(dir, 16, store, ctrs, nil, nil, nil, func() time.Time {
		return time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	})
	stop := wrtr.Start()
	t.Cleanup(stop)

	mux := NewMux(Deps{
		Store:             store,
		Counters:          ctrs,
		AdminToken:        "tok-test",
		Version:           "test",
		StartedAt:         time.Date(2026, 5, 27, 11, 0, 0, 0, time.UTC),
		DataDir:           dir,
		ExportByteHardCap: 1, // operator override; way under any real trace size
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	enqueueTrace(t, wrtr, "01H_DO1", "aaaa1111aaaa1111", nil)
	waitForRows(t, store, 1)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; want 413 (Deps override fires)", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("non-JSON 413 body %q: %v", body, err)
	}
	if got["cap_bytes"] != "1" {
		t.Errorf("cap_bytes = %v, want \"1\" (Deps value, not package default)", got["cap_bytes"])
	}
}

// TestExportDepsByteHardCap_NegativeDisables exercises the
// deps.ExportByteHardCap < 0 branch — operator wants the byte cap
// off entirely (permanent ?bytes_all=1). Sets the package var TINY
// to prove the negative Deps overrides it, not just defers to it.
func TestExportDepsByteHardCap_NegativeDisables(t *testing.T) {
	saved := defaultExportByteHardCap
	defaultExportByteHardCap = 1
	t.Cleanup(func() { defaultExportByteHardCap = saved })

	dir := t.TempDir()
	store, err := sqlite.Open(dir + "/index.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctrs := counters.New()
	wrtr := writer.New(dir, 16, store, ctrs, nil, nil, nil, func() time.Time {
		return time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	})
	stop := wrtr.Start()
	t.Cleanup(stop)

	mux := NewMux(Deps{
		Store:             store,
		Counters:          ctrs,
		AdminToken:        "tok-test",
		Version:           "test",
		StartedAt:         time.Date(2026, 5, 27, 11, 0, 0, 0, time.UTC),
		DataDir:           dir,
		ExportByteHardCap: -1, // operator says: never enforce
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	enqueueTrace(t, wrtr, "01H_DN1", "aaaa1111aaaa1111", nil)
	waitForRows(t, store, 1)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/export", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s; want 200 (negative Deps disables cap)", resp.StatusCode, body)
	}
}
