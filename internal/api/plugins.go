package api

// Plugin runtime-config handlers per plugin-b-c-spec.md §8.5.
//
// Four endpoints, all under authMW:
//
//	GET  /api/plugins/types          → list builtin types + ConfigSchema
//	GET  /api/config/plugins         → effective instance list + source
//	PUT  /api/config/plugins         → full-list replace (spec §3.3.2)
//	PUT  /api/config/plugins/{id}    → single-instance patch
//
// Truth model:
//
//   - GET reports what's on disk: if `<DataDir>/runtime_overrides.json`
//     carries a non-nil plugins block, that's the effective list and
//     source="override"; otherwise we report an empty list with
//     source="yaml".
//   - PUT / DELETE write through runtime.SaveOverride AND, when
//     Deps.PluginV2Reg is wired, call Reload to atomically swap the
//     live in-memory registry (W4.2 — spec §3.4 / §4.4). If Reload
//     reports an Init error the persisted file is rolled back to the
//     previous Plugins block and the response is 500 with the Init
//     error in the body; the old registry stays live so in-flight
//     traffic is unaffected.
//
// PUT semantics:
//
//   - PUT /api/config/plugins replaces the entire override list. An
//     empty `instances:[]` is the explicit "all plugins off" signal
//     (spec §3.3.2) and is persisted as a non-nil pointer with empty
//     slice so reload doesn't collapse to "no override."
//   - PUT /api/config/plugins/{id} patches one instance in place:
//       * if no override exists, 404 — operator must seed via PUT
//         full-list first.
//       * if the id is not in the override list, 404.
//       * missing fields preserve current values. {enabled:false}
//         alone toggles without touching config.
//
// `errors: []` in PUT responses carries Reload init failures when one
// occurs after the persist step succeeded but before rollback — in v1
// rollback always succeeds and the array stays empty.

import (
	"encoding/json"
	"net/http"
	"strconv"

	pluginv2 "github.com/leoyun/api-log/internal/plugin/v2"
	"github.com/leoyun/api-log/internal/runtime"
)

// PluginTypeDescriptor is one entry in GET /api/plugins/types.
//
// Spec §12.4 explicitly hands W3 the wire shape; this is the shape
// future builtins (W2) and the viewer Settings UI (W4) will both pin
// against. Keep fields stable.
type PluginTypeDescriptor struct {
	Type         string             `json:"type"`
	Description  string             `json:"description,omitempty"`
	ConfigSchema PluginConfigSchema `json:"config_schema"`
}

// PluginConfigSchema describes the per-instance config a viewer Add /
// Edit form must render for a given plugin type (spec §8.6).
type PluginConfigSchema struct {
	Fields []PluginConfigField `json:"fields"`
}

// PluginConfigField is one form field's descriptor.
//
// Type is one of "string" | "int" | "bool" | "string_array" | "enum".
// The viewer renders an appropriate input control per Type; unknown
// types fall back to a string text field on the viewer side.
type PluginConfigField struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Default     any      `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
}

// pluginInstanceJSON is the wire shape for a single instance in the
// GET / PUT bodies. Distinct from runtime.PluginInstanceOverride so
// the wire format can evolve without dragging the on-disk shape.
//
// Enabled is a plain bool on the full-list shape: clients SHOULD send
// a concrete value in a full-list replace. The single-instance PATCH
// (PUT /{id}) uses pluginInstancePatchJSON below, where Enabled is a
// pointer so absent means "do not change."
type pluginInstanceJSON struct {
	Type    string         `json:"type"`
	ID      string         `json:"id"`
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config"`
}

type pluginsConfigJSON struct {
	Instances []pluginInstanceJSON `json:"instances"`
	Source    string               `json:"source,omitempty"`
}

type pluginsConfigPutJSON struct {
	Ok        bool                 `json:"ok"`
	Instances []pluginInstanceJSON `json:"instances"`
	// Errors is always present (possibly empty) so the wire shape is
	// stable. With W4.2 hot-reload, the policy on Init failure is
	// rollback-and-500: the file is restored to its previous Plugins
	// block and the handler returns an error response (not 200 with a
	// populated Errors array). The field stays in the shape so clients
	// can future-proof against a softer policy without a wire break.
	Errors []string `json:"errors"`
}

type pluginInstancePatchJSON struct {
	Type    *string         `json:"type,omitempty"`
	Enabled *bool           `json:"enabled,omitempty"`
	Config  *map[string]any `json:"config,omitempty"`
}

type pluginInstancePutResponseJSON struct {
	Ok       bool               `json:"ok"`
	Instance pluginInstanceJSON `json:"instance"`
}

// listPluginTypes implements GET /api/plugins/types.
//
// The list source is Deps.PluginTypes — a nil-guarded provider func
// the main.go wiring populates from the builtin registry. v2 does not
// expose a list-all API and W3 is not allowed to modify it; the
// provider seam lets W2 / main.go inject the catalogue without
// inverting the dependency direction.
//
// Nil provider returns an empty list (the W3 default state — no
// builtins exist yet). The empty array shape is intentional so the
// viewer Add modal renders an honest "no plugin types registered"
// state instead of failing on a missing field.
func listPluginTypes(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var types []PluginTypeDescriptor
		if deps.PluginTypes != nil {
			types = deps.PluginTypes()
		}
		if types == nil {
			types = []PluginTypeDescriptor{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"types": types})
	})
}

// getConfigPlugins implements GET /api/config/plugins.
//
// Returns the effective override list when one exists on disk, else an
// empty list with source="yaml" — the empty-array vs missing-field
// distinction matches the rest of the API (no nil-vs-empty foot-guns).
func getConfigPlugins(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ov, err := runtime.LoadOverrides(deps.DataDir)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "override_read_failed",
				map[string]string{"detail": err.Error()})
			return
		}
		body := pluginsConfigJSON{
			Instances: []pluginInstanceJSON{},
			Source:    "yaml",
		}
		if ov.Plugins != nil {
			body.Source = "override"
			body.Instances = instancesToJSON(ov.Plugins.Instances)
		}
		writeJSON(w, http.StatusOK, body)
	})
}

// putConfigPlugins implements PUT /api/config/plugins.
//
// Full-list replace (spec §3.3.2 / checklist #11). An explicit empty
// list is meaningful — it persists as a non-nil PluginsOverride{
// Instances: []} so reload reads it back as "all plugins off," not as
// "no override."
//
// Each entry's Type and ID must be non-empty; ID must be unique within
// the list (operator picks IDs).
func putConfigPlugins(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Instances *[]pluginInstanceJSON `json:"instances"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_body",
				map[string]string{"detail": err.Error()})
			return
		}
		if body.Instances == nil {
			writeError(w, http.StatusBadRequest, "missing_field",
				map[string]string{"field": "instances"})
			return
		}

		seen := make(map[string]struct{}, len(*body.Instances))
		for i, inst := range *body.Instances {
			if inst.Type == "" {
				writeError(w, http.StatusBadRequest, "bad_instance",
					map[string]string{"detail": "missing type", "index": strconv.Itoa(i)})
				return
			}
			if inst.ID == "" {
				writeError(w, http.StatusBadRequest, "bad_instance",
					map[string]string{"detail": "missing id", "index": strconv.Itoa(i)})
				return
			}
			if _, dup := seen[inst.ID]; dup {
				writeError(w, http.StatusBadRequest, "duplicate_id",
					map[string]string{"id": inst.ID})
				return
			}
			seen[inst.ID] = struct{}{}
		}

		// Build the persistence shape. Enabled is materialized into a
		// fresh *bool so the JSON we write back never aliases the
		// request body (defensive — protects against callers reusing
		// the slice after the handler returns).
		persisted := make([]runtime.PluginInstanceOverride, 0, len(*body.Instances))
		for _, inst := range *body.Instances {
			enabled := inst.Enabled
			persisted = append(persisted, runtime.PluginInstanceOverride{
				Type:    inst.Type,
				ID:      inst.ID,
				Enabled: &enabled,
				Config:  inst.Config,
			})
		}

		// Empty list MUST persist as a non-nil pointer with an empty
		// (but non-nil) slice — see PluginsOverride docstring.
		if persisted == nil {
			persisted = []runtime.PluginInstanceOverride{}
		}

		// Snapshot the previous Plugins block before writing so a
		// post-write Reload failure can roll back to exactly what was
		// on disk a moment ago (W4.2 — spec §4.4).
		prev, lerr := runtime.LoadOverrides(deps.DataDir)
		if lerr != nil {
			writeError(w, http.StatusInternalServerError, "override_read_failed",
				map[string]string{"detail": lerr.Error()})
			return
		}

		err := runtime.SaveOverride(deps.DataDir, func(ov *runtime.Overrides) {
			ov.Plugins = &runtime.PluginsOverride{Instances: persisted}
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "override_write_failed",
				map[string]string{"detail": err.Error()})
			return
		}

		// Hot-reload: swap the live registry atomically. On Init
		// failure roll back the file and surface the error; the old
		// registry stays live so in-flight requests are unaffected.
		if rerr := reloadPluginRegistry(deps, &runtime.PluginsOverride{Instances: persisted}); rerr != nil {
			rollbackPluginsOverride(deps, prev.Plugins)
			writeError(w, http.StatusInternalServerError, "reload_failed",
				map[string]string{"detail": rerr.Error()})
			return
		}

		writeJSON(w, http.StatusOK, pluginsConfigPutJSON{
			Ok:        true,
			Instances: instancesToJSON(persisted),
			Errors:    []string{},
		})
	})
}

// deleteConfigPlugins implements DELETE /api/config/plugins (spec
// §8.5 row 5). Clears the runtime override block, so subsequent GET
// reports source="yaml" and the effective registry reverts to the
// YAML defaults on next reload.
//
// Idempotent: deleting a never-saved override is a 200, not 404 —
// "make the state be no-override" is the operator's intent regardless
// of whether one exists.
func deleteConfigPlugins(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prev, lerr := runtime.LoadOverrides(deps.DataDir)
		if lerr != nil {
			writeError(w, http.StatusInternalServerError, "override_read_failed",
				map[string]string{"detail": lerr.Error()})
			return
		}
		err := runtime.SaveOverride(deps.DataDir, func(ov *runtime.Overrides) {
			ov.Plugins = nil
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "override_write_failed",
				map[string]string{"detail": err.Error()})
			return
		}
		// Reload with nil override → registry falls back to YAML
		// defaults (deps.YAMLPlugins). Rollback restores prev on
		// failure so the live registry and on-disk state agree.
		if rerr := reloadPluginRegistry(deps, nil); rerr != nil {
			rollbackPluginsOverride(deps, prev.Plugins)
			writeError(w, http.StatusInternalServerError, "reload_failed",
				map[string]string{"detail": rerr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"source": "yaml",
		})
	})
}

// putConfigPluginInstance implements PUT /api/config/plugins/{id}.
//
// Single-instance patch: load current override list, locate the entry
// by id, apply the patch (missing fields preserve current values),
// write back.
//
// 404s on two paths: no override file at all, or override file present
// but no entry with this id. The "promote-from-YAML on first patch"
// path is not implemented in W3 — operator must seed via the full-list
// PUT first when migrating from YAML defaults.
func putConfigPluginInstance(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "missing_param",
				map[string]string{"param": "id"})
			return
		}

		var patch pluginInstancePatchJSON
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&patch); err != nil {
			writeError(w, http.StatusBadRequest, "bad_body",
				map[string]string{"detail": err.Error()})
			return
		}

		ov, err := runtime.LoadOverrides(deps.DataDir)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "override_read_failed",
				map[string]string{"detail": err.Error()})
			return
		}
		if ov.Plugins == nil {
			writeError(w, http.StatusNotFound, "not_found",
				map[string]string{"detail": "no plugin overrides; seed with PUT /api/config/plugins first"})
			return
		}

		idx := -1
		for i := range ov.Plugins.Instances {
			if ov.Plugins.Instances[i].ID == id {
				idx = i
				break
			}
		}
		if idx == -1 {
			writeError(w, http.StatusNotFound, "not_found",
				map[string]string{"detail": "no plugin instance with id=" + id})
			return
		}

		// Apply the patch. Each field is touched only when the wire
		// pointer is non-nil; absent fields preserve existing state.
		// Type is patchable to support the spec §10.2 "operator can
		// change an instance's type via the API without changing its
		// identity" goal — rare, but the contract allows it.
		if patch.Type != nil {
			if *patch.Type == "" {
				writeError(w, http.StatusBadRequest, "bad_body",
					map[string]string{"detail": "type cannot be empty"})
				return
			}
			ov.Plugins.Instances[idx].Type = *patch.Type
		}
		if patch.Enabled != nil {
			v := *patch.Enabled
			ov.Plugins.Instances[idx].Enabled = &v
		}
		if patch.Config != nil {
			ov.Plugins.Instances[idx].Config = *patch.Config
		}

		patched := ov.Plugins.Instances[idx]

		// Snapshot the pre-write Plugins block for rollback. SaveOverride
		// does its own LoadOverrides → mutate → write, so we grab the
		// authoritative state before that race window — if reload fails
		// after the file changed, we restore exactly this.
		prev, lerr := runtime.LoadOverrides(deps.DataDir)
		if lerr != nil {
			writeError(w, http.StatusInternalServerError, "override_read_failed",
				map[string]string{"detail": lerr.Error()})
			return
		}

		// SaveOverride does its own LoadOverrides → mutate → write.
		// Re-find the instance in that fresh snapshot and replace it.
		// If a concurrent full-list PUT removed the instance between
		// the handler's read and SaveOverride's read, the patch is
		// dropped on the floor — the same lost-update behavior the
		// rest of this unguarded layer has, not something to
		// special-case here.
		err = runtime.SaveOverride(deps.DataDir, func(out *runtime.Overrides) {
			if out.Plugins == nil {
				return
			}
			for i := range out.Plugins.Instances {
				if out.Plugins.Instances[i].ID == id {
					out.Plugins.Instances[i] = patched
					return
				}
			}
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "override_write_failed",
				map[string]string{"detail": err.Error()})
			return
		}

		// Hot-reload uses the post-save authoritative state, not the
		// in-handler ov — SaveOverride's read-modify-write may have
		// observed a concurrent edit and our patched copy is no longer
		// guaranteed to match disk. Load the freshest snapshot for the
		// swap input.
		fresh, ferr := runtime.LoadOverrides(deps.DataDir)
		if ferr != nil {
			rollbackPluginsOverride(deps, prev.Plugins)
			writeError(w, http.StatusInternalServerError, "override_read_failed",
				map[string]string{"detail": ferr.Error()})
			return
		}
		if rerr := reloadPluginRegistry(deps, fresh.Plugins); rerr != nil {
			rollbackPluginsOverride(deps, prev.Plugins)
			writeError(w, http.StatusInternalServerError, "reload_failed",
				map[string]string{"detail": rerr.Error()})
			return
		}

		writeJSON(w, http.StatusOK, pluginInstancePutResponseJSON{
			Ok:       true,
			Instance: instanceToJSON(patched),
		})
	})
}

// instancesToJSON converts the on-disk shape to the wire shape.
// Enabled is materialized: nil-on-disk renders as false on the wire
// (the full-list response contract is a concrete bool per instance).
func instancesToJSON(in []runtime.PluginInstanceOverride) []pluginInstanceJSON {
	out := make([]pluginInstanceJSON, len(in))
	for i, inst := range in {
		out[i] = instanceToJSON(inst)
	}
	return out
}

func instanceToJSON(inst runtime.PluginInstanceOverride) pluginInstanceJSON {
	enabled := false
	if inst.Enabled != nil {
		enabled = *inst.Enabled
	}
	cfg := inst.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	return pluginInstanceJSON{
		Type:    inst.Type,
		ID:      inst.ID,
		Enabled: enabled,
		Config:  cfg,
	}
}

// reloadPluginRegistry asks Deps.PluginV2Reg to swap to a fresh build
// of (YAML defaults × the supplied override). Nil-safe: when no
// registry is wired (the persistence-only test harness) this is a
// no-op so PUT / DELETE remain pure persistence calls. The override
// arg comes from the freshest LoadOverrides snapshot so the live
// registry tracks exactly what's on disk.
func reloadPluginRegistry(deps Deps, ov *runtime.PluginsOverride) error {
	if deps.PluginV2Reg == nil {
		return nil
	}
	return deps.PluginV2Reg.Reload(deps.YAMLPlugins, runtimeOverrideToV2(ov))
}

// rollbackPluginsOverride restores the persisted Plugins block to
// `prev` after a failed Reload. Best-effort: a secondary write error
// (disk full, permission flip mid-handler) is swallowed because the
// primary error is already on its way back to the operator and a
// rollback failure has nowhere useful to surface. The handler's job
// here is to leave the on-disk state matching the live registry so a
// subsequent GET reports the truth.
func rollbackPluginsOverride(deps Deps, prev *runtime.PluginsOverride) {
	_ = runtime.SaveOverride(deps.DataDir, func(o *runtime.Overrides) {
		o.Plugins = prev
	})
}

// runtimeOverrideToV2 adapts the runtime persistence shape to the v2
// hook layer's OverrideList. Nil propagates (no override → fall through
// to YAML defaults); a non-nil PluginsOverride with a nil/empty
// Instances slice means "all plugins off" and is preserved as a
// non-nil OverrideList with an empty Instances slice so v2.Load's
// merge semantics match.
func runtimeOverrideToV2(ov *runtime.PluginsOverride) *pluginv2.OverrideList {
	if ov == nil {
		return nil
	}
	out := make([]pluginv2.InstanceConfig, 0, len(ov.Instances))
	for _, inst := range ov.Instances {
		ic := pluginv2.InstanceConfig{
			Type:   inst.Type,
			ID:     inst.ID,
			Config: inst.Config,
		}
		if inst.Enabled != nil {
			ic.Enabled = *inst.Enabled
		}
		out = append(out, ic)
	}
	return &pluginv2.OverrideList{Instances: out}
}
