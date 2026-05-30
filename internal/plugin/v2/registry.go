package v2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
)

// InstanceConfig is the per-instance config tuple loaded from
// config.yaml or runtime_overrides.json. The Registry consumes a
// slice of these (in operator-declared order) and instantiates each.
//
// W3 wires this to internal/runtime.PluginsOverrides. v2 defines its
// own type so the package builds independently of the overrides
// extension (which lands in a later WP).
type InstanceConfig struct {
	Type    string
	ID      string
	Enabled bool
	Config  map[string]any
}

// OverrideList wraps the runtime-override block. The pointer
// semantics mirror spec §3.3.2:
//
//   - nil OverrideList               → no override at all; YAML wins.
//   - non-nil with Instances == nil  → same as nil (no override).
//   - non-nil with Instances == []   → "operator turned all plugins
//                                      off"; override wins, no plugins
//                                      run.
//   - non-nil with Instances == [..] → full replace of the YAML list.
//
// The distinction between nil and "non-nil empty" is the bug-prone
// part the spec calls out. Load enforces it explicitly.
type OverrideList struct {
	Instances []InstanceConfig
}

// Load merges YAML defaults with runtime overrides and returns the
// effective ordered instance list. See OverrideList for the merge
// semantics.
//
// Caller pattern (process start and runtime-edit hot swap):
//
//	effective := plugin.Load(yamlList, overrides)
//	reg, errs := plugin.NewRegistry(effective)
//	if len(errs) > 0 && runtimeEdit { /* do not swap */ }
//	atomicPtr.Store(reg)
//
// Load itself never errors — it is a pure merge over already-decoded
// slices. Error surfacing lives in NewRegistry where the Init failures
// happen.
func Load(yamlList []InstanceConfig, overrides *OverrideList) []InstanceConfig {
	// nil override or "Instances is nil" → fall through to YAML.
	if overrides == nil || overrides.Instances == nil {
		return cloneInstances(yamlList)
	}
	// non-nil override with explicit empty list → all plugins off.
	// non-nil override with non-empty list      → full replace.
	return cloneInstances(overrides.Instances)
}

// cloneInstances returns a defensive copy so callers can hold the
// returned slice without aliasing the source. Config maps are passed
// through by reference; the Ctor receives them and copies as needed.
func cloneInstances(in []InstanceConfig) []InstanceConfig {
	if in == nil {
		return nil
	}
	out := make([]InstanceConfig, len(in))
	copy(out, in)
	return out
}

// Ctor is a builtin factory registered at compile time. cfg is the
// instance's Config map; the constructor MAY return either a
// BeforePlugin, an AfterPlugin, or a value implementing both — the
// Registry detects via type assertion at instance-load time.
//
// Returning a non-nil error from a Ctor is reported by the Registry's
// Load call as an Init error; the registry returns the partial state
// and the error, and the caller (proxy main / API handler) decides
// whether to swap. See spec §4.4.
type Ctor func(cfg map[string]any) (any, error)

// builtinRegistry maps type-name → Ctor for all in-tree plugin types.
// Built-ins register at init() time via RegisterBuiltin; the map is
// process-global and immutable after main() runs.
var (
	builtinMu       sync.RWMutex
	builtinRegistry = map[string]Ctor{}
)

// RegisterBuiltin records a builtin plugin type's factory. Called
// from each builtin package's init(). Duplicate type-names panic at
// process start so misconfiguration cannot ship.
func RegisterBuiltin(typeName string, ctor Ctor) {
	if typeName == "" {
		panic("v2.RegisterBuiltin: empty type name")
	}
	if ctor == nil {
		panic(fmt.Sprintf("v2.RegisterBuiltin: nil ctor for %q", typeName))
	}
	builtinMu.Lock()
	defer builtinMu.Unlock()
	if _, exists := builtinRegistry[typeName]; exists {
		panic(fmt.Sprintf("v2.RegisterBuiltin: duplicate type %q", typeName))
	}
	builtinRegistry[typeName] = ctor
}

// LookupBuiltin returns the Ctor for a type-name. Used by tests and by
// the future Load() implementation. Returns (nil, false) for unknown
// types.
func LookupBuiltin(typeName string) (Ctor, bool) {
	builtinMu.RLock()
	defer builtinMu.RUnlock()
	c, ok := builtinRegistry[typeName]
	return c, ok
}

// ResetBuiltinsForTest clears the builtin map. TEST-ONLY — used by
// registry_test to isolate cases that register temporary type names.
// Not exposed in production code paths.
func ResetBuiltinsForTest() {
	builtinMu.Lock()
	defer builtinMu.Unlock()
	builtinRegistry = map[string]Ctor{}
}

// Instance is one running plugin: the configured tuple plus the
// concrete typed handles. before / after are nil when the plugin
// instance does not implement that hook side.
type Instance struct {
	Type    string
	ID      string
	Enabled bool
	Config  map[string]any

	before BeforePlugin
	after  AfterPlugin
}

// Name returns the type-name (operator-facing identifier). The
// instance's per-config ID is held on the Instance struct.
func (i *Instance) Name() string { return i.Type }

// HasBefore reports whether this instance implements BeforePlugin.
// Cheap accessor used by the proxy hot-path "any-BEFORE-plugins?" check
// so the handler can short-circuit body buffering when nothing needs it.
func (i *Instance) HasBefore() bool { return i != nil && i.before != nil }

// HasAfter reports whether this instance implements AfterPlugin.
// Symmetric to HasBefore; used by the AFTER-side dispatcher entry to
// avoid wrapping the response body when no plugin would observe it.
func (i *Instance) HasAfter() bool { return i != nil && i.after != nil }

// InstanceHasBefore is a free-function alias for the Instance accessor,
// exported so external packages (cmd/api-log) can ask the question
// without importing the Instance type directly.
func InstanceHasBefore(i *Instance) bool { return i.HasBefore() }

// InstanceHasAfter mirrors InstanceHasBefore.
func InstanceHasAfter(i *Instance) bool { return i.HasAfter() }

// Registry holds the ordered list of plugin instances and exposes the
// iteration entry points used by the proxy hot path.
//
// W4.2 hot-reload (spec §3.4 / §4.4): the live instance list is held
// inside an atomic.Pointer so Reload can swap it without invalidating
// the outer *Registry that main.go / api.Deps captured at startup.
// All iteration methods load the current snapshot at call entry; an
// in-flight call uses the snapshot it loaded, and the next call after
// a swap picks up the new one. Old instances are NOT Close()d on swap
// — Close runs only on graceful shutdown (Registry.Close).
type Registry struct {
	// set is an *instanceSnapshot. Never nil after the first store; the
	// snapshot helper returns an empty snapshot if the pointer is nil so
	// zero-value Registry literals stay safe (the panic-cap tests use them).
	set atomic.Pointer[instanceSnapshot]
}

// instanceSnapshot is the atomic value swapped in by Reload. A struct
// (not a bare slice) gives the atomic.Pointer a concrete element type
// without the brackets-in-type-position parse hazard.
type instanceSnapshot struct {
	instances []*Instance
}

// snapshot returns the current instance list snapshot, never nil. A nil
// atomic value (zero-value Registry literal in tests) yields an empty
// snapshot rather than panicking — keeps the iterate calls inert.
func (r *Registry) snapshot() *instanceSnapshot {
	if r == nil {
		return &instanceSnapshot{}
	}
	s := r.set.Load()
	if s == nil {
		return &instanceSnapshot{}
	}
	return s
}

// NewRegistry constructs a Registry from an ordered slice of instance
// configs. Each entry's Type is looked up in the builtin registry; if
// the type is unknown, that entry is skipped and an error is appended
// to the returned errs slice. Init failures (factory returns err) are
// also collected. Other instances continue to load.
//
// Collect-and-continue is the process-start policy: main.go sees the
// errs slice and decides to log + exit on any failure. The runtime hot-
// reload path (Reload) uses the same builder but applies all-or-nothing
// semantics — see Reload for the rollback contract (spec §4.4).
func NewRegistry(configs []InstanceConfig) (*Registry, []error) {
	r := &Registry{}
	instances, errs := buildInstances(configs)
	r.set.Store(&instanceSnapshot{instances: instances})
	return r, errs
}

// buildInstances is the shared instantiation pass used by NewRegistry
// (collect-and-continue) and Reload (all-or-nothing). It returns the
// instances that were successfully built and any errors collected; the
// caller chooses how to react.
func buildInstances(configs []InstanceConfig) ([]*Instance, []error) {
	var instances []*Instance
	var errs []error
	for _, c := range configs {
		if c.ID == "" {
			errs = append(errs, fmt.Errorf("plugin instance has empty id (type=%q)", c.Type))
			continue
		}
		ctor, ok := LookupBuiltin(c.Type)
		if !ok {
			errs = append(errs, fmt.Errorf("plugin instance %q: unknown type %q", c.ID, c.Type))
			continue
		}
		inst := &Instance{
			Type:    c.Type,
			ID:      c.ID,
			Enabled: c.Enabled,
			Config:  c.Config,
		}
		built, err := ctor(c.Config)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin instance %q init: %w", c.ID, err))
			continue
		}
		if bp, ok := built.(BeforePlugin); ok {
			inst.before = bp
		}
		if ap, ok := built.(AfterPlugin); ok {
			inst.after = ap
		}
		if inst.before == nil && inst.after == nil {
			errs = append(errs, fmt.Errorf("plugin instance %q (type=%q) implements neither BeforePlugin nor AfterPlugin", c.ID, c.Type))
			continue
		}
		instances = append(instances, inst)
	}
	return instances, errs
}

// Reload atomically swaps the registry's live instance list to a fresh
// build of (yamlList × overrides). All-or-nothing: any builder error
// returns a joined error and leaves the OLD snapshot live. In-flight
// requests on the old snapshot finish naturally; Close on old instances
// is the responsibility of graceful shutdown, NOT the swap (spec §4.4).
//
// This is the entry point the API handlers invoke after persisting a
// config edit; the handler rolls back the persisted file when Reload
// returns an error. main.go does NOT need to call Reload at startup —
// NewRegistry already seeds the initial snapshot.
//
// Snapshot granularity is per iterate call. Within a single in-flight
// proxy request the BEFORE and AFTER chains load the snapshot
// independently, so a Reload landing between them means BEFORE runs on
// the old config and AFTER on the new. This is intentional (spec: each
// iterate call loads the freshest instances at entry); per-request
// snapshot consistency would require threading a snapshot through ctx
// and is out of scope.
func (r *Registry) Reload(yamlList []InstanceConfig, overrides *OverrideList) error {
	if r == nil {
		return errors.New("v2.Registry.Reload: nil receiver")
	}
	effective := Load(yamlList, overrides)
	instances, errs := buildInstances(effective)
	if len(errs) > 0 {
		// Build a flat joined error so the API layer can surface it
		// verbatim in the rollback response. Old snapshot stays live.
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return fmt.Errorf("plugin reload init failed: %s", strings.Join(msgs, "; "))
	}
	r.set.Store(&instanceSnapshot{instances: instances})
	return nil
}

// Instances returns a snapshot of registered instances in
// registration order. Returned slice is a copy; safe to inspect from
// any goroutine.
func (r *Registry) Instances() []*Instance {
	if r == nil {
		return nil
	}
	snap := r.snapshot()
	out := make([]*Instance, len(snap.instances))
	copy(out, snap.instances)
	return out
}

// InterceptInfo identifies which plugin instance produced an intercept,
// alongside the intercept response itself. Callers use Type / ID / Hook
// to populate trace.PluginIntercepted (spec §5.2) so operators can tell
// "this 403 came from plugin X" apart from "upstream returned 403".
type InterceptInfo struct {
	Type     string
	ID       string
	Hook     string // "before" | "after"
	Response *InterceptResponse
}

// IterateBefore runs the BEFORE chain in registration order.
//
// Semantics (spec §2.2 / §2.4 / §4):
//   - Plugins not implementing BeforePlugin are skipped.
//   - Disabled instances are skipped.
//   - ActionContinue: pass current request to next plugin.
//   - ActionMutate:   replace current request with Mutated (when
//                     non-nil); pass new request to next plugin.
//   - ActionIntercept: STOP iteration; return (current request,
//                     intercept info). Caller serves the
//                     intercept body and runs the AFTER chain on it.
//
// Panics are recovered by safeBefore (recover.go) with the request
// flowing unchanged.
func (r *Registry) IterateBefore(ctx context.Context, req *ParsedRequest) (*ParsedRequest, *InterceptInfo) {
	cur := req
	snap := r.snapshot()
	for _, inst := range snap.instances {
		if !inst.Enabled || inst.before == nil {
			continue
		}
		res := safeBefore(ctx, r, inst, cur)
		switch res.Action {
		case ActionMutate:
			if res.Mutated != nil {
				cur = res.Mutated
			}
		case ActionIntercept:
			if res.Intercept != nil {
				return cur, &InterceptInfo{
					Type: inst.Type, ID: inst.ID, Hook: "before",
					Response: res.Intercept,
				}
			}
			// Plugin said Intercept but produced no body — treat as
			// Continue (defensive). Logged via slog; a future WP may
			// re-introduce per-trace breadcrumbs when an adopter needs
			// them.
			slog.Warn("plugin defensive continue",
				"type", inst.Type, "id", inst.ID, "hook", "before",
				"reason", "ActionIntercept with nil Intercept body")
		}
	}
	return cur, nil
}

// IterateAfter runs the AFTER chain in registration order against an
// AfterContext. See AfterPlugin docs (hook.go) for the per-branch
// semantics.
//
// Returns InterceptInfo when any plugin intercepts; nil otherwise.
// ActionMutate replaces ac.Response in the non-streaming branch.
func (r *Registry) IterateAfter(ctx context.Context, req *ParsedRequest, ac *AfterContext) *InterceptInfo {
	snap := r.snapshot()
	for _, inst := range snap.instances {
		if !inst.Enabled || inst.after == nil {
			continue
		}
		res := safeAfter(ctx, r, inst, req, ac)
		switch res.Action {
		case ActionMutate:
			if res.Mutated != nil && ac != nil {
				ac.Response = res.Mutated
			}
		case ActionIntercept:
			if res.Intercept != nil {
				return &InterceptInfo{
					Type: inst.Type, ID: inst.ID, Hook: "after",
					Response: res.Intercept,
				}
			}
			slog.Warn("plugin defensive continue",
				"type", inst.Type, "id", inst.ID, "hook", "after",
				"reason", "ActionIntercept with nil Intercept body")
		}
	}
	return nil
}

// WrapStreamDispatcher returns a StreamDispatcher seeded with the
// registered AFTER plugins' callbacks for the given request.
//
// Order of operations:
//   1. Build a fresh AfterContext.
//   2. Call IterateAfter(req, ac) so each AFTER plugin registers
//      its callbacks (and the dispatcher records any panics).
//   3. Return a StreamDispatcher referencing the populated context.
//
// The caller's stream loop then calls Dispatcher.Process(ev) per
// upstream event.
//
// When an AFTER plugin intercepts during registration, the returned
// dispatcher is nil and the InterceptInfo is non-nil: the caller
// should drop the upstream stream and serve the intercept body.
func (r *Registry) WrapStreamDispatcher(ctx context.Context, req *ParsedRequest) (*StreamDispatcher, *InterceptInfo) {
	ac := &AfterContext{}
	if intercept := r.IterateAfter(ctx, req, ac); intercept != nil {
		return nil, intercept
	}
	return &StreamDispatcher{Protocol: req.Protocol, After: ac}, nil
}

// Close releases any resources held by registered plugin instances.
// Plugins that implement io.Closer (or any `Close() error` shape) get
// their Close called in registration order. Errors are returned as a
// joined error so the caller (main.go shutdown) can log them without
// blocking on any single failure. Plugins that don't implement Close
// are skipped silently — v2 hook plugins are stateless in v1.
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	type closer interface{ Close() error }
	var firstErr error
	snap := r.snapshot()
	for _, inst := range snap.instances {
		if c, ok := inst.before.(closer); ok && c != nil {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("plugin %q close (before): %w", inst.ID, err)
			}
		}
		// after may be the same value as before (when one Go type implements
		// both hooks); avoid the double-close in that case.
		if inst.after != nil && any(inst.after) != any(inst.before) {
			if c, ok := inst.after.(closer); ok && c != nil {
				if err := c.Close(); err != nil && firstErr == nil {
					firstErr = fmt.Errorf("plugin %q close (after): %w", inst.ID, err)
				}
			}
		}
	}
	return firstErr
}

// InterceptedBy is a convenience helper that returns the first non-nil
// InterceptInfo from a slice. Used by tests that drive both hooks.
func InterceptedBy(infos ...*InterceptInfo) *InterceptInfo {
	for _, i := range infos {
		if i != nil {
			return i
		}
	}
	return nil
}
