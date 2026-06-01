package api

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
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
