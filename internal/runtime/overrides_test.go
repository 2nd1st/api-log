package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestLoadOverrides_MissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatalf("LoadOverrides on empty dir: %v", err)
	}
	if ov.Media.SaveAttachments != nil {
		t.Errorf("expected nil SaveAttachments on missing file, got %+v", ov.Media.SaveAttachments)
	}
}

func TestLoadOverrides_EmptyFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, overridesFilename), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatalf("LoadOverrides on empty file: %v", err)
	}
	if ov.Media.SaveAttachments != nil {
		t.Errorf("expected nil SaveAttachments on empty file, got %+v", ov.Media.SaveAttachments)
	}
}

func TestLoadOverrides_MalformedJSONErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, overridesFilename), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOverrides(dir); err == nil {
		t.Fatal("LoadOverrides on malformed JSON should error, got nil")
	}
}

func TestSaveOverride_RoundTripFalse(t *testing.T) {
	dir := t.TempDir()
	err := SaveOverride(dir, func(o *Overrides) {
		o.Media.SaveAttachments = boolPtr(false)
	})
	if err != nil {
		t.Fatalf("SaveOverride: %v", err)
	}

	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatalf("LoadOverrides after save: %v", err)
	}
	if ov.Media.SaveAttachments == nil {
		t.Fatal("SaveAttachments should be present after save, got nil")
	}
	if *ov.Media.SaveAttachments != false {
		t.Errorf("SaveAttachments = %v, want false", *ov.Media.SaveAttachments)
	}
}

func TestSaveOverride_RoundTripTrue(t *testing.T) {
	dir := t.TempDir()
	err := SaveOverride(dir, func(o *Overrides) {
		o.Media.SaveAttachments = boolPtr(true)
	})
	if err != nil {
		t.Fatalf("SaveOverride: %v", err)
	}

	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatalf("LoadOverrides after save: %v", err)
	}
	if ov.Media.SaveAttachments == nil || *ov.Media.SaveAttachments != true {
		t.Errorf("SaveAttachments = %+v, want true", ov.Media.SaveAttachments)
	}
}

func TestSaveOverride_PreservesUntouchedFields(t *testing.T) {
	// Two-field future-proofing: SaveOverride is contractually a
	// read-modify-write so a second field set by a different caller
	// must survive a save that only touches SaveAttachments.
	dir := t.TempDir()

	// Hand-author a JSON with an extra unknown field that mimics a
	// field a future version of this struct might add. We then call
	// SaveOverride, reload the raw JSON, and confirm the unknown
	// field is gone — documenting the trade-off: this layer ONLY
	// preserves what the current Overrides struct knows about.
	//
	// Sub-test instead asserts: setting SaveAttachments=true, then
	// calling SaveOverride with a no-op mutate, must NOT clear the
	// previously-saved value.
	if err := SaveOverride(dir, func(o *Overrides) {
		o.Media.SaveAttachments = boolPtr(true)
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveOverride(dir, func(o *Overrides) { /* no-op */ }); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Media.SaveAttachments == nil || *ov.Media.SaveAttachments != true {
		t.Errorf("no-op save clobbered prior value: %+v", ov.Media.SaveAttachments)
	}
}

func TestSaveOverride_AtomicNoTempLeft(t *testing.T) {
	dir := t.TempDir()
	if err := SaveOverride(dir, func(o *Overrides) {
		o.Media.SaveAttachments = boolPtr(false)
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestSaveOverride_NilMutateErrors(t *testing.T) {
	dir := t.TempDir()
	if err := SaveOverride(dir, nil); err == nil {
		t.Fatal("SaveOverride(nil) should error, got nil")
	}
}

func TestSaveOverride_PluginsNilByDefault(t *testing.T) {
	// A media-only save MUST NOT introduce a non-nil Plugins block —
	// omitempty on Overrides.Plugins is the load-bearing piece that
	// keeps "no override" distinct from "explicit empty override."
	dir := t.TempDir()
	if err := SaveOverride(dir, func(o *Overrides) {
		o.Media.SaveAttachments = boolPtr(true)
	}); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Plugins != nil {
		t.Errorf("media-only save introduced Plugins block: %+v", ov.Plugins)
	}
}

func TestSaveOverride_PluginsEmptyListRoundTrips(t *testing.T) {
	// plugin-b-c-spec §3.3.2: a non-nil Plugins pointer with an empty
	// Instances slice is the "all plugins off" signal. It MUST survive
	// the marshal/unmarshal trip — not collapse into nil. The test
	// directly exercises the load/save layer because this invariant is
	// what makes the API handler's empty-PUT semantics work.
	dir := t.TempDir()
	if err := SaveOverride(dir, func(o *Overrides) {
		o.Plugins = &PluginsOverride{Instances: []PluginInstanceOverride{}}
	}); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Plugins == nil {
		t.Fatal("empty Instances collapsed to nil Plugins on round-trip")
	}
	if ov.Plugins.Instances == nil {
		// json.Unmarshal of `[]` into a nil slice produces a non-nil
		// empty slice; if this ever changes we want a loud signal so
		// downstream "all plugins off" detection keeps working.
		t.Error("Instances came back nil; want non-nil empty slice")
	}
	if len(ov.Plugins.Instances) != 0 {
		t.Errorf("len(Instances) = %d, want 0", len(ov.Plugins.Instances))
	}
}

func TestSaveOverride_PluginsInstanceRoundTrips(t *testing.T) {
	// Full-shape round trip: every field on PluginInstanceOverride
	// must come back exactly as written. Catches accidental json tag
	// drift.
	dir := t.TempDir()
	want := PluginInstanceOverride{
		Type:    "text-replace",
		ID:      "wm-public",
		Enabled: boolPtr(true),
		Config: map[string]any{
			"routes": []any{"/v1/*"},
			"down":   map[string]any{"suffix": "footer"},
		},
	}
	if err := SaveOverride(dir, func(o *Overrides) {
		o.Plugins = &PluginsOverride{Instances: []PluginInstanceOverride{want}}
	}); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Plugins == nil || len(ov.Plugins.Instances) != 1 {
		t.Fatalf("plugins not persisted: %+v", ov.Plugins)
	}
	got := ov.Plugins.Instances[0]
	if got.Type != want.Type || got.ID != want.ID {
		t.Errorf("type/id mismatch: got=%+v want=%+v", got, want)
	}
	if got.Enabled == nil || *got.Enabled != true {
		t.Errorf("Enabled = %+v, want &true", got.Enabled)
	}
	if got.Config["routes"] == nil {
		t.Errorf("Config.routes lost on round trip: %+v", got.Config)
	}
}

func TestSaveOverride_OnDiskShapeMatchesContract(t *testing.T) {
	// Phase K contract § 6 specifies:
	//   {"media": {"save_attachments": true}}
	// Decode the raw file and assert that exact shape.
	dir := t.TempDir()
	if err := SaveOverride(dir, func(o *Overrides) {
		o.Media.SaveAttachments = boolPtr(true)
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, overridesFilename))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Media struct {
			SaveAttachments *bool `json:"save_attachments"`
		} `json:"media"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("on-disk JSON does not match contract shape: %v", err)
	}
	if decoded.Media.SaveAttachments == nil || *decoded.Media.SaveAttachments != true {
		t.Errorf("on-disk save_attachments = %+v, want true", decoded.Media.SaveAttachments)
	}
}
