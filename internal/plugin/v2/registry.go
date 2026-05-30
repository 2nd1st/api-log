package v2

import (
	"context"
	"fmt"
	"sync"
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
// Registry is constructed once per atomic-swap cycle (process start +
// each runtime config edit). After construction the slice is treated
// as immutable; concurrent reads are safe without a lock.
type Registry struct {
	instances []*Instance

	// errors accumulates plugin panic / error breadcrumbs that the
	// finalize layer attaches to the trace's plugin_errors field
	// (spec §5.3). Bounded to keep trace lines small.
	errMu  sync.Mutex
	errors []PluginError
}

// PluginError is one entry in the per-request error breadcrumb list.
// See spec §5.3.
type PluginError struct {
	Type string
	ID   string
	Hook string // "before" | "after"
	Msg  string
}

// maxPluginErrors is the cap on the per-request breadcrumb list. Spec
// §12.4 leaves the cap up to W1; 4 entries is enough to cover a chain
// of cascading failures without bloating disk usage.
const maxPluginErrors = 4

// NewRegistry constructs a Registry from an ordered slice of instance
// configs. Each entry's Type is looked up in the builtin registry; if
// the type is unknown, that entry is skipped and an error is appended
// to the returned errs slice. Init failures (factory returns err) are
// also collected. Other instances continue to load.
//
// The caller (Phase A1 / W3) decides whether to swap based on errs.
// If errs is non-empty AND the caller is in a runtime-edit path, it
// MUST NOT swap — spec §4.4: "Init failure does NOT swap the atomic
// pointer". For process-start the caller logs and exits.
func NewRegistry(configs []InstanceConfig) (*Registry, []error) {
	r := &Registry{}
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
		r.instances = append(r.instances, inst)
	}
	return r, errs
}

// Instances returns a snapshot of registered instances in
// registration order. Returned slice is a copy; safe to inspect from
// any goroutine.
func (r *Registry) Instances() []*Instance {
	if r == nil {
		return nil
	}
	out := make([]*Instance, len(r.instances))
	copy(out, r.instances)
	return out
}

// Errors returns the accumulated plugin error breadcrumbs, capped at
// maxPluginErrors. Caller (finalize) drains via DrainErrors after the
// trace is built so the next request starts fresh.
//
// NOTE: in the dispatcher-per-request model (W3), errors are collected
// per *call* not per *registry*. v1 of the registry uses a single
// slice with a mutex; W3 will replace this with a per-request
// collector passed through ctx.
func (r *Registry) Errors() []PluginError {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	out := make([]PluginError, len(r.errors))
	copy(out, r.errors)
	return out
}

// DrainErrors returns the current error list and clears it. Used by
// the finalize block after the trace is built.
func (r *Registry) DrainErrors() []PluginError {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	out := r.errors
	r.errors = nil
	return out
}

// recordError appends one error, dropping the oldest if the cap is
// already reached. Bounded to keep JSONL lines small.
func (r *Registry) recordError(typeName, id, hook, msg string) {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	if len(r.errors) >= maxPluginErrors {
		// Drop oldest.
		copy(r.errors, r.errors[1:])
		r.errors[len(r.errors)-1] = PluginError{Type: typeName, ID: id, Hook: hook, Msg: msg}
		return
	}
	r.errors = append(r.errors, PluginError{Type: typeName, ID: id, Hook: hook, Msg: msg})
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
	for _, inst := range r.instances {
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
			// Continue (defensive; recorded as an error for the
			// trace breadcrumb).
			r.recordError(inst.Type, inst.ID, "before", "ActionIntercept with nil Intercept body; treated as Continue")
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
	for _, inst := range r.instances {
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
			r.recordError(inst.Type, inst.ID, "after", "ActionIntercept with nil Intercept body; treated as Continue")
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
	for _, inst := range r.instances {
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
