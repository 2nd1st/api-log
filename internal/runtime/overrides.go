// Package runtime is the persistence layer for tiny operator-toggleable
// settings that need to survive process restart and be writable at runtime
// from an API endpoint (e.g. PUT /api/config/media).
//
// The file lives at <DataDir>/runtime_overrides.json and is the highest-
// precedence config layer per phase-k-media-contract.md § 6:
//
//	default < yaml < env < runtime_overrides.json
//
// Why a separate package (not config.go):
//   - config.Load() reads a static YAML at startup; this layer is writable
//     at runtime by the API server. Keeping it separate makes the
//     "static vs mutable" boundary explicit.
//   - Pointer-valued fields (*bool) distinguish "no override set" from
//     "override set to false" so absence falls through to env/yaml/default.
//
// Atomicity: SaveOverride writes a sibling .tmp file and renames; readers
// never observe a partial JSON document.
package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// overridesFilename is the on-disk basename, relative to DataDir.
const overridesFilename = "runtime_overrides.json"

// Overrides is the on-disk shape of runtime_overrides.json.
//
// Every nested struct uses pointer fields so absent == nil (== "fall
// through to the layer below"); a present false value is meaningful and
// MUST not collapse into "unset".
type Overrides struct {
	Media     MediaOverrides       `json:"media"`
	Plugins   *PluginsOverride     `json:"plugins,omitempty"`
	Retention *RetentionOverrides  `json:"retention,omitempty"`
}

// RetentionOverrides toggles the storage-coordinator retention loop at
// runtime. Mirrors storage.RetentionConfig but with pointer fields so
// "operator set max_bytes to 0" (i.e. disable the byte cap) is
// distinguishable from "operator hasn't touched this knob" (fall
// through to default).
//
//   - nil pointer (Overrides.Retention == nil): no override at all;
//     the coord starts with retention disabled per startup default.
//   - non-nil pointer, all sub-pointers nil: deliberately-empty override
//     written by an earlier PUT; equivalent to retention disabled.
//   - any sub-pointer set: apply that value to coord.UpdateConfig at
//     startup, then leave the rest at storage.RetentionConfig defaults.
type RetentionOverrides struct {
	MaxBytes      *int64 `json:"max_bytes,omitempty"`
	MaxAgeDays    *int   `json:"max_age_days,omitempty"`
	WarnAtPercent *int   `json:"warn_at_percent,omitempty"`
}

// MediaOverrides toggles the media extraction subsystem at runtime.
//
// SaveAttachments is *bool so we can distinguish:
//   - nil: no override; use yaml or hardcoded default
//   - &true: force extraction on
//   - &false: force extraction off
type MediaOverrides struct {
	SaveAttachments *bool `json:"save_attachments,omitempty"`
}

// PluginsOverride is the runtime-override block for the plugin instance
// list, mirroring plugin-b-c-spec §3.3.2 / §3.3.3.
//
// The bug-prone distinction is between a nil pointer and a non-nil
// pointer with an empty Instances slice:
//
//   - Overrides.Plugins == nil          → no override; YAML wins.
//   - Overrides.Plugins != nil with
//     Instances == nil OR len() == 0    → "operator turned all plugins
//     off"; override wins, no
//     plugins run.
//   - Overrides.Plugins != nil with
//     a non-empty Instances             → full replace of the YAML list.
//
// Because of that, Instances has NO omitempty: an explicit empty array
// must round-trip through json.Marshal → json.Unmarshal as the same
// non-nil empty slice we just wrote.
type PluginsOverride struct {
	Instances []PluginInstanceOverride `json:"instances"`
}

// PluginInstanceOverride is one configured plugin instance as stored on
// disk. Field semantics:
//
//   - Type, ID: identifying tuple. Operator chooses ID (unique across
//     all instances). Both are required when an instance is persisted.
//   - Enabled: *bool because a single-instance PATCH may carry "config
//     only" or "enabled only"; nil means "do not change this field"
//     when applied by PUT /api/config/plugins/{id}. For full-list PUTs
//     (the common path) clients SHOULD send a concrete bool.
//   - Config: per-type config blob. nil pointer (map==nil) means "do
//     not change this field" on PATCH; an empty map means "clear
//     overrides for this instance."
//
// The pointer-on-Enabled choice mirrors MediaOverrides.SaveAttachments
// — absent in the wire format MUST be distinguishable from `false`.
type PluginInstanceOverride struct {
	Type    string         `json:"type"`
	ID      string         `json:"id"`
	Enabled *bool          `json:"enabled,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
}

// LoadOverrides reads <dataDir>/runtime_overrides.json.
//
// A missing file is NOT an error: it returns the zero Overrides{} so
// callers can treat "no override" as the common case without special-
// casing os.IsNotExist at every callsite.
//
// A malformed file IS an error — silently swallowing a parse failure
// would hide configuration drift from the operator.
func LoadOverrides(dataDir string) (Overrides, error) {
	var ov Overrides
	path := filepath.Join(dataDir, overridesFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Overrides{}, nil
		}
		return Overrides{}, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		// Empty file is treated the same as missing — operator may
		// have created it as a placeholder; do not error.
		return Overrides{}, nil
	}
	if err := json.Unmarshal(data, &ov); err != nil {
		return Overrides{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return ov, nil
}

// SaveOverride atomically updates one or more fields in
// runtime_overrides.json by:
//
//  1. Loading the current file (or empty Overrides if missing).
//  2. Applying mutate(&ov) so the caller can set just the field it owns
//     without clobbering siblings.
//  3. Marshalling, writing to a .tmp sibling, then os.Rename — readers
//     see either the old file or the new one, never a torn write.
//
// The directory is created with 0o700 if it does not exist; matches
// the rest of the data dir tree (the writer + capture also use 0o700)
// since the same tree carries raw API keys via JSONL siblings.
func SaveOverride(dataDir string, mutate func(*Overrides)) error {
	if mutate == nil {
		return fmt.Errorf("SaveOverride: mutate fn is nil")
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dataDir, err)
	}

	ov, err := LoadOverrides(dataDir)
	if err != nil {
		return fmt.Errorf("load existing overrides: %w", err)
	}
	mutate(&ov)

	// Marshal indented for human-edit-friendliness — this file is
	// small (a few bytes) and operators are expected to occasionally
	// `cat` or hand-edit it.
	data, err := json.MarshalIndent(ov, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal overrides: %w", err)
	}
	data = append(data, '\n')

	path := filepath.Join(dataDir, overridesFilename)
	tmp := path + ".tmp"

	// O_TRUNC so a leftover .tmp from a previous crash is overwritten,
	// not appended to. 0o600 — the file itself carries no secrets, but
	// it shares the data dir with JSONL traces that do; keep the perm
	// floor consistent so a forgotten chmod doesn't reset the dir to a
	// permissive world-read state.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Best-effort cleanup of the temp file; ignore secondary error.
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
