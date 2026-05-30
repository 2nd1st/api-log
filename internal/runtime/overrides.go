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
	Media MediaOverrides `json:"media"`
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
// The directory is created with 0o755 if it does not exist; this matches
// the writer's behavior for the data dir tree.
func SaveOverride(dataDir string, mutate func(*Overrides)) error {
	if mutate == nil {
		return fmt.Errorf("SaveOverride: mutate fn is nil")
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
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
	// not appended to. 0o644 because the file contains no secrets.
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Best-effort cleanup of the temp file; ignore secondary error.
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
