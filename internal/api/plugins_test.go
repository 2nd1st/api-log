package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leoyun/api-log/internal/counters"
	pluginv2 "github.com/leoyun/api-log/internal/plugin/v2"
	"github.com/leoyun/api-log/internal/runtime"
	"github.com/leoyun/api-log/internal/store/sqlite"
)

// newPluginsTestServer is a focused harness for the four plugin
// endpoints. It does NOT spin up a writer goroutine — the plugin API
// is independent of trace ingest and the extra moving parts would just
// add flake. DataDir is a per-test TempDir; PluginTypes is injectable
// per test.
func newPluginsTestServer(t *testing.T, token string, types func() []PluginTypeDescriptor) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mux := NewMux(Deps{
		Store:       store,
		Counters:    counters.New(),
		AdminToken:  token,
		Version:     "test",
		StartedAt:   time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
		DataDir:     dir,
		PluginTypes: types,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, dir
}

// doJSON performs an authenticated request and decodes the JSON body.
// It is intentionally not generic over the response type — tests
// decode into map[string]any so they can assert on the precise wire
// shape (presence/absence of fields) without re-litigating the struct
// definitions.
func doJSON(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) == 0 {
		return resp.StatusCode, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s %s: %v\nraw=%s", method, url, err, raw)
	}
	return resp.StatusCode, decoded
}

// --- GET /api/plugins/types ---

func TestPluginTypes_AuthRequired(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	resp, err := http.Get(srv.URL + "/api/plugins/types")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPluginTypes_EmptyWhenNoProvider(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	status, body := doJSON(t, "GET", srv.URL+"/api/plugins/types", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%+v", status, body)
	}
	types, ok := body["types"].([]any)
	if !ok {
		t.Fatalf("types missing or not an array: %+v", body)
	}
	if len(types) != 0 {
		t.Errorf("len(types) = %d, want 0", len(types))
	}
}

func TestPluginTypes_RendersProviderEntries(t *testing.T) {
	provider := func() []PluginTypeDescriptor {
		return []PluginTypeDescriptor{
			{
				Type:        "text-replace",
				Description: "Literal substring replace, both hooks.",
				ConfigSchema: PluginConfigSchema{
					Fields: []PluginConfigField{
						{Name: "routes", Label: "Routes", Type: "string_array", Required: false},
					},
				},
			},
		}
	}
	srv, _ := newPluginsTestServer(t, "tok", provider)
	status, body := doJSON(t, "GET", srv.URL+"/api/plugins/types", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	types := body["types"].([]any)
	if len(types) != 1 {
		t.Fatalf("len(types) = %d, want 1", len(types))
	}
	got := types[0].(map[string]any)
	if got["type"] != "text-replace" {
		t.Errorf("type = %v, want text-replace", got["type"])
	}
	schema, ok := got["config_schema"].(map[string]any)
	if !ok {
		t.Fatalf("config_schema missing: %+v", got)
	}
	fields := schema["fields"].([]any)
	if len(fields) != 1 {
		t.Errorf("len(fields) = %d, want 1", len(fields))
	}
}

// --- GET /api/config/plugins ---

func TestGetConfigPlugins_AuthRequired(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	resp, err := http.Get(srv.URL + "/api/config/plugins")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestGetConfigPlugins_NoOverrideIsYAMLSource(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	status, body := doJSON(t, "GET", srv.URL+"/api/config/plugins", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body["source"] != "yaml" {
		t.Errorf("source = %v, want yaml", body["source"])
	}
	inst, ok := body["instances"].([]any)
	if !ok || len(inst) != 0 {
		t.Errorf("instances = %+v, want empty array", body["instances"])
	}
}

func TestGetConfigPlugins_OverridePresent(t *testing.T) {
	srv, dir := newPluginsTestServer(t, "tok", nil)
	mustSaveOverrideList(t, dir, []runtime.PluginInstanceOverride{
		{Type: "text-replace", ID: "wm-public", Enabled: boolPtr(true),
			Config: map[string]any{"routes": []any{"/v1/*"}}},
	})
	status, body := doJSON(t, "GET", srv.URL+"/api/config/plugins", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body["source"] != "override" {
		t.Errorf("source = %v, want override", body["source"])
	}
	inst := body["instances"].([]any)
	if len(inst) != 1 {
		t.Fatalf("len(instances) = %d, want 1", len(inst))
	}
	first := inst[0].(map[string]any)
	if first["id"] != "wm-public" || first["type"] != "text-replace" || first["enabled"] != true {
		t.Errorf("instance = %+v, mismatch", first)
	}
}

// --- PUT /api/config/plugins ---

func TestPutConfigPlugins_AuthRequired(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	req, _ := http.NewRequest("PUT", srv.URL+"/api/config/plugins",
		bytes.NewReader([]byte(`{"instances":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPutConfigPlugins_MissingFieldRejected(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	// Body without "instances" field — distinct from "instances": [].
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%+v", status, body)
	}
	if body["error"] != "missing_field" {
		t.Errorf("error = %v, want missing_field", body["error"])
	}
}

func TestPutConfigPlugins_EmptyListPersistsAsAllOff(t *testing.T) {
	// Spec §3.3.2: an explicit empty list is "all plugins off" — the
	// override MUST persist as a non-nil PluginsOverride with an empty
	// slice so reload reads it back as override (not as "no override
	// → fall through to YAML").
	srv, dir := newPluginsTestServer(t, "tok", nil)
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{"instances": []any{}})
	if status != http.StatusOK {
		t.Fatalf("status = %d; body=%+v", status, body)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v", body["ok"])
	}
	if errs, ok := body["errors"].([]any); !ok || len(errs) != 0 {
		t.Errorf("errors = %+v, want empty array", body["errors"])
	}

	// Round-trip via the persistence layer to confirm non-nil empty.
	ov, err := runtime.LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Plugins == nil {
		t.Fatal("empty PUT collapsed to nil Plugins; want non-nil empty")
	}
	if len(ov.Plugins.Instances) != 0 {
		t.Errorf("len(Instances) = %d, want 0", len(ov.Plugins.Instances))
	}

	// And GET reports source=override after an empty PUT.
	status, body = doJSON(t, "GET", srv.URL+"/api/config/plugins", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d", status)
	}
	if body["source"] != "override" {
		t.Errorf("source after empty PUT = %v, want override", body["source"])
	}
}

func TestPutConfigPlugins_FullReplaceRoundTrip(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	payload := map[string]any{
		"instances": []map[string]any{
			{
				"type":    "text-replace",
				"id":      "wm-public",
				"enabled": true,
				"config":  map[string]any{"routes": []string{"/v1/*"}},
			},
			{
				"type":    "text-append",
				"id":      "policy-footer",
				"enabled": false,
				"config":  map[string]any{"down": map[string]any{"suffix": "x"}},
			},
		},
	}
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok", payload)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body=%+v", status, body)
	}
	gotInstances := body["instances"].([]any)
	if len(gotInstances) != 2 {
		t.Fatalf("len(instances) = %d, want 2", len(gotInstances))
	}

	// Subsequent GET should return identical list with source=override.
	status, body = doJSON(t, "GET", srv.URL+"/api/config/plugins", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d", status)
	}
	if body["source"] != "override" {
		t.Errorf("source = %v, want override", body["source"])
	}
	inst := body["instances"].([]any)
	if len(inst) != 2 {
		t.Fatalf("len(instances) = %d, want 2", len(inst))
	}
	second := inst[1].(map[string]any)
	if second["id"] != "policy-footer" || second["enabled"] != false {
		t.Errorf("second instance shape unexpected: %+v", second)
	}
}

func TestPutConfigPlugins_RejectsMissingType(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{
			"instances": []map[string]any{
				{"id": "no-type", "enabled": true},
			},
		})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%+v", status, body)
	}
	if body["error"] != "bad_instance" {
		t.Errorf("error = %v, want bad_instance", body["error"])
	}
}

func TestPutConfigPlugins_RejectsMissingID(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{
			"instances": []map[string]any{
				{"type": "text-replace", "enabled": true},
			},
		})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%+v", status, body)
	}
	if body["error"] != "bad_instance" {
		t.Errorf("error = %v, want bad_instance", body["error"])
	}
}

func TestPutConfigPlugins_RejectsDuplicateID(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{
			"instances": []map[string]any{
				{"type": "text-replace", "id": "same", "enabled": true},
				{"type": "text-append", "id": "same", "enabled": true},
			},
		})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%+v", status, body)
	}
	if body["error"] != "duplicate_id" {
		t.Errorf("error = %v, want duplicate_id", body["error"])
	}
}

func TestPutConfigPlugins_RejectsUnknownField(t *testing.T) {
	// DisallowUnknownFields is on; sending a typo is a client bug we
	// want to surface, not silently swallow.
	srv, _ := newPluginsTestServer(t, "tok", nil)
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{"instancesss": []any{}})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%+v", status, body)
	}
	if body["error"] != "bad_body" {
		t.Errorf("error = %v, want bad_body", body["error"])
	}
}

func TestPutConfigPlugins_PreservesSiblingOverrides(t *testing.T) {
	// runtime.SaveOverride is a read-modify-write; a plugin PUT must
	// not clobber a previously-saved media override.
	srv, dir := newPluginsTestServer(t, "tok", nil)
	if err := runtime.SaveOverride(dir, func(o *runtime.Overrides) {
		v := true
		o.Media.SaveAttachments = &v
	}); err != nil {
		t.Fatal(err)
	}
	status, _ := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{"instances": []any{}})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	ov, err := runtime.LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Media.SaveAttachments == nil || *ov.Media.SaveAttachments != true {
		t.Errorf("media override clobbered: %+v", ov.Media.SaveAttachments)
	}
}

// --- DELETE /api/config/plugins ---

func TestDeleteConfigPlugins_AuthRequired(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/config/plugins", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestDeleteConfigPlugins_RevertsToYAML(t *testing.T) {
	// Seed an override via PUT, then DELETE clears it. GET afterward
	// reports source="yaml" with an empty instance list.
	srv, dir := newPluginsTestServer(t, "tok", nil)
	if _, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{
			"instances": []map[string]any{
				{"type": "text-replace", "id": "wm", "enabled": true, "config": map[string]any{}},
			},
		}); body["ok"] != true {
		t.Fatalf("seed PUT failed: %+v", body)
	}

	status, body := doJSON(t, "DELETE", srv.URL+"/api/config/plugins", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE status = %d, body=%+v", status, body)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["source"] != "yaml" {
		t.Errorf("source = %v, want yaml", body["source"])
	}

	// GET reports yaml + empty list.
	status, body = doJSON(t, "GET", srv.URL+"/api/config/plugins", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d", status)
	}
	if body["source"] != "yaml" {
		t.Errorf("post-DELETE source = %v, want yaml", body["source"])
	}
	inst, ok := body["instances"].([]any)
	if !ok || len(inst) != 0 {
		t.Errorf("instances = %+v, want empty array", body["instances"])
	}

	// Persistence layer reports nil Plugins.
	ov, err := runtime.LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Plugins != nil {
		t.Errorf("disk Plugins after DELETE = %+v, want nil", ov.Plugins)
	}
}

func TestDeleteConfigPlugins_IdempotentWhenNoOverride(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	status, body := doJSON(t, "DELETE", srv.URL+"/api/config/plugins", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE on virgin store should be 200; got %d body=%+v", status, body)
	}
	if body["source"] != "yaml" {
		t.Errorf("source = %v, want yaml", body["source"])
	}
}

func TestDeleteConfigPlugins_PreservesSiblingOverrides(t *testing.T) {
	srv, dir := newPluginsTestServer(t, "tok", nil)
	if err := runtime.SaveOverride(dir, func(o *runtime.Overrides) {
		v := true
		o.Media.SaveAttachments = &v
	}); err != nil {
		t.Fatal(err)
	}
	if status, _ := doJSON(t, "DELETE", srv.URL+"/api/config/plugins", "tok", nil); status != http.StatusOK {
		t.Fatalf("DELETE status = %d", status)
	}
	ov, err := runtime.LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Media.SaveAttachments == nil || *ov.Media.SaveAttachments != true {
		t.Errorf("media override clobbered by DELETE: %+v", ov.Media.SaveAttachments)
	}
}

// --- PUT /api/config/plugins/{id} ---

func TestPutPluginInstance_AuthRequired(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	req, _ := http.NewRequest("PUT", srv.URL+"/api/config/plugins/foo",
		bytes.NewReader([]byte(`{"enabled":false}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPutPluginInstance_NoOverrideIs404(t *testing.T) {
	srv, _ := newPluginsTestServer(t, "tok", nil)
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins/anything", "tok",
		map[string]any{"enabled": false})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%+v", status, body)
	}
}

func TestPutPluginInstance_UnknownIDIs404(t *testing.T) {
	srv, dir := newPluginsTestServer(t, "tok", nil)
	mustSaveOverrideList(t, dir, []runtime.PluginInstanceOverride{
		{Type: "text-replace", ID: "wm-public", Enabled: boolPtr(true),
			Config: map[string]any{}},
	})
	status, _ := doJSON(t, "PUT", srv.URL+"/api/config/plugins/nope", "tok",
		map[string]any{"enabled": false})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestPutPluginInstance_TogglesEnabledOnly(t *testing.T) {
	srv, dir := newPluginsTestServer(t, "tok", nil)
	mustSaveOverrideList(t, dir, []runtime.PluginInstanceOverride{
		{Type: "text-replace", ID: "wm-public", Enabled: boolPtr(true),
			Config: map[string]any{"routes": []any{"/v1/*"}}},
	})
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins/wm-public", "tok",
		map[string]any{"enabled": false})
	if status != http.StatusOK {
		t.Fatalf("status = %d; body=%+v", status, body)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v", body["ok"])
	}
	inst := body["instance"].(map[string]any)
	if inst["enabled"] != false {
		t.Errorf("enabled = %v, want false", inst["enabled"])
	}
	// Config preserved.
	cfg := inst["config"].(map[string]any)
	routes, ok := cfg["routes"].([]any)
	if !ok || len(routes) != 1 || routes[0] != "/v1/*" {
		t.Errorf("config not preserved: %+v", cfg)
	}

	// Reload and check the disk shape.
	ov, err := runtime.LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Plugins == nil || len(ov.Plugins.Instances) != 1 {
		t.Fatalf("ov.Plugins shape: %+v", ov.Plugins)
	}
	persisted := ov.Plugins.Instances[0]
	if persisted.Enabled == nil || *persisted.Enabled != false {
		t.Errorf("disk enabled = %+v, want false", persisted.Enabled)
	}
}

func TestPutPluginInstance_UpdatesConfigOnly(t *testing.T) {
	srv, dir := newPluginsTestServer(t, "tok", nil)
	mustSaveOverrideList(t, dir, []runtime.PluginInstanceOverride{
		{Type: "text-replace", ID: "wm-public", Enabled: boolPtr(true),
			Config: map[string]any{"routes": []any{"/v1/*"}}},
	})
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins/wm-public", "tok",
		map[string]any{"config": map[string]any{"routes": []any{"/v1/messages"}}})
	if status != http.StatusOK {
		t.Fatalf("status = %d; body=%+v", status, body)
	}
	inst := body["instance"].(map[string]any)
	if inst["enabled"] != true {
		t.Errorf("enabled = %v, want true (preserved)", inst["enabled"])
	}
	cfg := inst["config"].(map[string]any)
	routes := cfg["routes"].([]any)
	if len(routes) != 1 || routes[0] != "/v1/messages" {
		t.Errorf("config not updated: %+v", cfg)
	}
}

func TestPutPluginInstance_RejectsEmptyType(t *testing.T) {
	srv, dir := newPluginsTestServer(t, "tok", nil)
	mustSaveOverrideList(t, dir, []runtime.PluginInstanceOverride{
		{Type: "text-replace", ID: "wm-public", Enabled: boolPtr(true)},
	})
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins/wm-public", "tok",
		map[string]any{"type": ""})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%+v", status, body)
	}
	if body["error"] != "bad_body" {
		t.Errorf("error = %v, want bad_body", body["error"])
	}
}

func TestPutPluginInstance_RejectsUnknownField(t *testing.T) {
	srv, dir := newPluginsTestServer(t, "tok", nil)
	mustSaveOverrideList(t, dir, []runtime.PluginInstanceOverride{
		{Type: "text-replace", ID: "wm-public", Enabled: boolPtr(true)},
	})
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins/wm-public", "tok",
		map[string]any{"enabld": false})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%+v", status, body)
	}
	if body["error"] != "bad_body" {
		t.Errorf("error = %v, want bad_body", body["error"])
	}
}

// --- test helpers ---

func boolPtr(b bool) *bool { return &b }

// mustSaveOverrideList seeds the override file directly via the
// runtime persistence layer, sidestepping the PUT endpoint so the test
// under examination is the one being exercised.
func mustSaveOverrideList(t *testing.T, dir string, instances []runtime.PluginInstanceOverride) {
	t.Helper()
	if err := runtime.SaveOverride(dir, func(o *runtime.Overrides) {
		o.Plugins = &runtime.PluginsOverride{Instances: instances}
	}); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	// Sanity check the seed actually landed (catches a typo in the
	// fixture before the assertions confuse the failure mode).
	if _, err := os.Stat(filepath.Join(dir, "runtime_overrides.json")); err != nil {
		t.Fatalf("override file not present after seed: %v", err)
	}
}

// --- W4.2 hot-reload tests ----------------------------------------
//
// These exercise the registry-swap path Deps.PluginV2Reg lights up.
// The non-reload tests above leave PluginV2Reg nil so the handlers
// fall back to persist-only behavior; here we wire a registry and
// register fake builtins so Reload can actually instantiate.

// stubBeforePlugin is a minimal BeforePlugin used to verify Reload
// swapped the registry's live config. The Name field tags each
// instance so a test can tell "before-reload" and "after-reload"
// instances apart by reading reg.Instances() after the PUT.
type stubBeforePlugin struct{ name string }

func (s *stubBeforePlugin) Name() string { return s.name }
func (s *stubBeforePlugin) OnBefore(ctx context.Context, req *pluginv2.ParsedRequest, cfg map[string]any) pluginv2.BeforeResult {
	return pluginv2.BeforeResult{Action: pluginv2.ActionContinue}
}

// stubBadPlugin is a Ctor that always errors — used to drive the
// rollback path: a PUT carrying this type must leave the on-disk file
// AND the live registry untouched.
//
// Both stub plugin types register under their own type name via
// withStubBuiltins; tests scope their lifetime with t.Cleanup.
func withStubBuiltins(t *testing.T) {
	t.Helper()
	pluginv2.ResetBuiltinsForTest()
	pluginv2.RegisterBuiltin("stub", func(cfg map[string]any) (any, error) {
		name, _ := cfg["name"].(string)
		return &stubBeforePlugin{name: name}, nil
	})
	pluginv2.RegisterBuiltin("stub-bad", func(_ map[string]any) (any, error) {
		return nil, errBadCtor
	})
	t.Cleanup(pluginv2.ResetBuiltinsForTest)
}

var errBadCtor = badCtorErr("stub-bad: always fails")

type badCtorErr string

func (e badCtorErr) Error() string { return string(e) }

// newPluginsHotReloadServer returns a harness whose Deps carry a live
// pluginv2.Registry. The returned *Registry lets the test assert on
// the in-memory instance list directly after a PUT — that's the
// fixture-level "the next request would see the new config" check.
func newPluginsHotReloadServer(t *testing.T, token string) (*httptest.Server, string, *pluginv2.Registry) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg, errs := pluginv2.NewRegistry(nil)
	if len(errs) > 0 {
		t.Fatalf("initial registry build: %v", errs)
	}

	mux := NewMux(Deps{
		Store:       store,
		Counters:    counters.New(),
		AdminToken:  token,
		Version:     "test",
		StartedAt:   time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
		DataDir:     dir,
		PluginV2Reg: reg,
		YAMLPlugins: nil,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, dir, reg
}

func TestPutConfigPlugins_HotReload(t *testing.T) {
	// PUT a new instance, then assert the in-memory registry reflects
	// the change without restart. This is the W4.2 acceptance test:
	// the GET round-trip already worked in W3 (persisted); the new
	// behavior is the live-registry swap.
	withStubBuiltins(t)
	srv, _, reg := newPluginsHotReloadServer(t, "tok")

	// Pre-condition: empty registry.
	if got := reg.Instances(); len(got) != 0 {
		t.Fatalf("pre-PUT instances = %d, want 0", len(got))
	}

	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{
			"instances": []map[string]any{
				{
					"type":    "stub",
					"id":      "stub-1",
					"enabled": true,
					"config":  map[string]any{"name": "hot"},
				},
			},
		})
	if status != http.StatusOK {
		t.Fatalf("PUT status = %d; body=%+v", status, body)
	}

	// Live registry MUST hold the new instance now — no process
	// restart. The handler's response also confirms persist worked;
	// this assertion is the unique W4.2 contract.
	got := reg.Instances()
	if len(got) != 1 || got[0].ID != "stub-1" || got[0].Type != "stub" {
		t.Errorf("post-PUT registry instances = %+v, want [stub-1/stub]", got)
	}

	// Follow-up GET still reports the same effective config (proof the
	// on-disk + in-memory views agree — the swap didn't desync them).
	status, body = doJSON(t, "GET", srv.URL+"/api/config/plugins", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d", status)
	}
	if body["source"] != "override" {
		t.Errorf("source = %v, want override", body["source"])
	}
	inst := body["instances"].([]any)
	if len(inst) != 1 || inst[0].(map[string]any)["id"] != "stub-1" {
		t.Errorf("GET instances = %+v, want [stub-1]", inst)
	}
}

func TestPutConfigPlugins_HotReload_InitErrorRollsBack(t *testing.T) {
	// PUT carrying a config a Ctor will reject MUST:
	//   1. Return 500.
	//   2. Leave the live registry on the previous snapshot.
	//   3. Leave runtime_overrides.json on the previous Plugins block.
	withStubBuiltins(t)
	srv, dir, reg := newPluginsHotReloadServer(t, "tok")

	// Seed a known-good instance via PUT so we have a real prior state
	// to roll back to.
	if status, _ := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{
			"instances": []map[string]any{
				{"type": "stub", "id": "ok-1", "enabled": true, "config": map[string]any{"name": "ok"}},
			},
		}); status != http.StatusOK {
		t.Fatalf("seed PUT failed: status=%d", status)
	}
	// Confirm the seeded registry has it.
	if got := reg.Instances(); len(got) != 1 || got[0].ID != "ok-1" {
		t.Fatalf("seeded registry has wrong instance: %+v", got)
	}

	// Failing PUT: stub-bad ctor errors out.
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{
			"instances": []map[string]any{
				{"type": "stub-bad", "id": "boom", "enabled": true},
			},
		})
	if status != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body=%+v", status, body)
	}
	if body["error"] != "reload_failed" {
		t.Errorf("error = %v, want reload_failed", body["error"])
	}

	// Live registry MUST still show the seeded instance — swap rolled
	// back (or rather: never happened, because Reload is all-or-nothing).
	got := reg.Instances()
	if len(got) != 1 || got[0].ID != "ok-1" {
		t.Errorf("post-failed-PUT registry = %+v, want [ok-1] (rollback)", got)
	}

	// On-disk file MUST also show the seeded instance — the handler
	// rolled back the SaveOverride.
	ov, err := runtime.LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Plugins == nil || len(ov.Plugins.Instances) != 1 || ov.Plugins.Instances[0].ID != "ok-1" {
		t.Errorf("post-failed-PUT disk = %+v, want [ok-1] (rollback)", ov.Plugins)
	}
}

func TestDeleteConfigPlugins_HotReload(t *testing.T) {
	// DELETE swaps the live registry to YAML defaults (empty in v0).
	withStubBuiltins(t)
	srv, _, reg := newPluginsHotReloadServer(t, "tok")

	// Seed an instance, then DELETE it.
	if status, _ := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{
			"instances": []map[string]any{
				{"type": "stub", "id": "to-clear", "enabled": true},
			},
		}); status != http.StatusOK {
		t.Fatalf("seed PUT failed")
	}
	if got := reg.Instances(); len(got) != 1 {
		t.Fatalf("pre-DELETE registry empty: %+v", got)
	}

	status, body := doJSON(t, "DELETE", srv.URL+"/api/config/plugins", "tok", nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE status = %d; body=%+v", status, body)
	}
	if got := reg.Instances(); len(got) != 0 {
		t.Errorf("post-DELETE registry instances = %+v, want []", got)
	}
}

func TestPutPluginInstance_HotReload(t *testing.T) {
	// PUT /api/config/plugins/{id} (PATCH): toggle Enabled and assert
	// the live registry picks up the change.
	withStubBuiltins(t)
	srv, _, reg := newPluginsHotReloadServer(t, "tok")

	// Seed via PUT so the registry sees the initial state too.
	if status, _ := doJSON(t, "PUT", srv.URL+"/api/config/plugins", "tok",
		map[string]any{
			"instances": []map[string]any{
				{"type": "stub", "id": "patchme", "enabled": true, "config": map[string]any{"name": "v1"}},
			},
		}); status != http.StatusOK {
		t.Fatalf("seed PUT failed")
	}
	if got := reg.Instances(); len(got) != 1 || !got[0].Enabled {
		t.Fatalf("pre-PATCH: instances = %+v, want one enabled", got)
	}

	// PATCH: toggle Enabled off.
	status, body := doJSON(t, "PUT", srv.URL+"/api/config/plugins/patchme", "tok",
		map[string]any{"enabled": false})
	if status != http.StatusOK {
		t.Fatalf("PATCH status = %d; body=%+v", status, body)
	}
	got := reg.Instances()
	if len(got) != 1 || got[0].Enabled {
		t.Errorf("post-PATCH registry = %+v, want one DISABLED instance", got)
	}
}
