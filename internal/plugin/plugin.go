// Package plugin defines the Phase A observe-class plugin surface
// (ObserveOnFinalize / ObserveAfterWrite) used by the path-filter plugin
// to drop traces before they reach the writer. Plugins are registered
// in-process at startup from the operator's enabled list; there is no
// hot-load. Hook-class plugins (request/response mutation) live under
// internal/plugin/v2.
package plugin

import (
	"context"
	"fmt"

	"github.com/2nd1st/api-log/internal/trace"
)

// Plugin is the base contract every observe-class plugin satisfies.
//
// A plugin opts into individual hooks by also implementing one of the
// hook interfaces (ObserveOnFinalize, ObserveAfterWrite). The base
// contract here is only "I am a named, configurable, closable thing."
type Plugin interface {
	// Name returns the stable identifier used in config and telemetry.
	// MUST be a compile-time constant string and MUST match the key
	// the registry knows the plugin by. Two plugins with the same Name
	// are a configuration error.
	Name() string

	// Init is called once at process startup with the plugin's parsed
	// config subtree (the value at plugins.config.<name> in YAML, or
	// the equivalent struct field). Returning a non-nil error fails
	// process start; the caller logs and exits.
	//
	// Plugins MUST NOT perform background work, open files, or start
	// goroutines from Init unless those resources are owned and closed
	// in Close. Init should be cheap and synchronous.
	Init(cfg map[string]any) error

	// Close is called at process shutdown after the writer has drained.
	// Plugins SHOULD flush any buffered state here. Errors are logged
	// but do not block shutdown.
	Close() error
}

// ObserveOnFinalize is the observe-class hook called BEFORE the writer
// chain runs. It fires inside the trace finalize block, after buildTrace
// has produced a *trace.Trace but before the writer goroutine has been
// notified.
//
// Returning shouldRecord=false drops the trace: no JSONL line is appended,
// no SQLite row is inserted, no downstream observation fires. This is the
// path-filter plugin's mechanism for honoring operator-configured noise
// filters at capture time (the principled home for ROADMAP § 4
// "capture-time skip"; see uiux-research/plugin.md § 7.1).
//
// Returning a non-nil err is LOGGED and surfaced via /healthz counters
// but does NOT block forwarding and does NOT change shouldRecord on its
// own — fail-open per PHILOSOPHY principle 3 ("fail open on capture").
// A plugin that wants to drop on error returns shouldRecord=false
// alongside its err; a plugin that wants to record on error returns
// shouldRecord=true.
//
// The trace value is passed BY VALUE to communicate read-only intent.
// Plugins MUST NOT retain references to req/resp body bytes past
// the call; the underlying buffers may be reused by the parser.
//
// Hook contract:
//
//   - Runs after the response has been fully delivered to the client.
//     Cannot affect forwarding (principle 2 honored by construction).
//   - Runs synchronously on the finalize goroutine; the writer is not
//     notified until every OnFinalize plugin has returned.
//   - Runs in registration order. Returning shouldRecord=false from any
//     plugin SHORT-CIRCUITS the iteration: subsequent plugins do not
//     see the dropped trace.
//
// Renamed from ObserveBeforeRecord per plugin-b-c-spec §7.3 (Phase A
// migration) to reflect that this fires inside the finalize block, not
// the writer pipeline. Observer-class plugins decide whether the trace
// is recorded; hook-class plugins (v2.BeforePlugin / v2.AfterPlugin)
// shape what flows to the client.
type ObserveOnFinalize interface {
	Plugin
	OnFinalize(ctx context.Context, tr trace.Trace) (shouldRecord bool, err error)
}

// ObserveAfterWrite is the observe-class hook called AFTER the writer
// chain has finished. It fires after the JSONL line is on disk and the
// SQLite row has been upserted — i.e. the trace is durable and queryable.
//
// AfterWrite is the natural home for side-effecting observation:
// counter exports, downstream notification sinks, operator dashboards
// (the doc's "alert" and "metrics" categories — see
// uiux-research/plugin.md § 2.6, § 7.5). Per PHILOSOPHY principle 4
// ("compose, don't absorb"), the in-tree project does NOT ship an
// alerting plugin; operators who want webhooks write their own and
// register it here.
//
// Hook contract:
//
//   - Runs after the writer ack. (Phase A.1 caveat: TrySend is currently
//     non-blocking; the trace is queued but may not be on disk when
//     AfterWrite fires. See uiux-research/plugin.md § 10 "after_record
//     durability semantics" — Phase A ships best-effort and explicitly
//     documents it.)
//   - Runs in registration order, sequentially per trace. Phase A does
//     NOT fan out to a worker pool; that is Phase B territory if the
//     observed plugins prove load-bearing.
//   - Errors are logged. There is no way for an AfterWrite plugin to
//     un-write an already-recorded trace (principle 6: filesystem is
//     truth, append-only). Operators learn about plugin errors via
//     /healthz counters.
//
// Renamed from ObserveAfterRecord per plugin-b-c-spec §7.3 (Phase A
// migration); the "AfterWrite" name keeps the side-effecting-only
// semantics distinct from the hook-class AFTER (which can mutate the
// response before the client sees it).
type ObserveAfterWrite interface {
	Plugin
	AfterWrite(ctx context.Context, tr trace.Trace)
}

// Registry holds the operator's enabled plugins in registration order.
//
// The order matters: OnFinalize plugins fire in registration order,
// and a shouldRecord=false return SHORT-CIRCUITS the chain. Operators
// who care about ordering (e.g. "tag-by-path runs before path-filter
// so dropped traces still carry the tag in logs") control it via the
// order of the enabled list in config.
//
// Registry is constructed once at startup and treated as immutable
// thereafter. There is no hot-reload; restart the process.
type Registry struct {
	plugins []Plugin
}

// NewRegistry returns an empty Registry. Use Register to add plugins
// in the order the operator's config lists them.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register appends p to the registry. The registration order is the
// invocation order for every hook. Returns an error if a plugin with
// the same Name() is already registered (operator config bug).
func (r *Registry) Register(p Plugin) error {
	if p == nil {
		return fmt.Errorf("plugin: register nil")
	}
	name := p.Name()
	if name == "" {
		return fmt.Errorf("plugin: register: empty Name()")
	}
	for _, existing := range r.plugins {
		if existing.Name() == name {
			return fmt.Errorf("plugin: register: duplicate name %q", name)
		}
	}
	r.plugins = append(r.plugins, p)
	return nil
}

// Init calls Init on every registered plugin with the plugin's own
// config subtree. The cfgs map is keyed by Plugin.Name(); missing
// entries are passed as nil (plugins MUST treat nil as "use defaults").
//
// Returns on the first plugin whose Init fails. Caller (main) logs and
// exits the process — a misconfigured plugin should not silently degrade.
func (r *Registry) Init(cfgs map[string]map[string]any) error {
	for _, p := range r.plugins {
		var cfg map[string]any
		if cfgs != nil {
			cfg = cfgs[p.Name()]
		}
		if err := p.Init(cfg); err != nil {
			return fmt.Errorf("plugin %q init: %w", p.Name(), err)
		}
	}
	return nil
}

// Close calls Close on every registered plugin in registration order.
// Errors are returned joined into a single error; close attempts continue
// past failures (graceful shutdown is best-effort across all plugins).
func (r *Registry) Close() error {
	var firstErr error
	for _, p := range r.plugins {
		if err := p.Close(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("plugin %q close: %w", p.Name(), err)
			}
		}
	}
	return firstErr
}

// IterateOnFinalize runs the OnFinalize chain in registration order.
//
// Semantics:
//
//   - Plugins that do NOT implement ObserveOnFinalize are skipped
//     (a plugin can opt out of this hook entirely).
//   - Returning shouldRecord=false from any plugin causes iteration to
//     STOP and the caller receives (false, nil). Subsequent plugins do
//     not see the trace.
//   - Errors are collected in the returned errs slice (one entry per
//     erroring plugin) but do NOT stop iteration on their own.
//     Per-plugin behavior: if a plugin returns shouldRecord=false AND
//     err != nil, the err is recorded and iteration stops. If a plugin
//     returns shouldRecord=true AND err != nil, the err is recorded
//     and iteration continues to the next plugin.
//
// This mirrors PHILOSOPHY principle 3: a plugin failure does not by
// itself drop a trace; the plugin must explicitly say "drop this."
//
// The caller (writer dispatch in Phase A.1) uses shouldRecord to gate
// the TrySend call and logs errs for telemetry.
func (r *Registry) IterateOnFinalize(ctx context.Context, tr trace.Trace) (shouldRecord bool, errs []error) {
	shouldRecord = true //nolint:ineffassign // implicit return value when no plugin says drop
	for _, p := range r.plugins {
		hook, ok := p.(ObserveOnFinalize)
		if !ok {
			continue
		}
		ok, err := hook.OnFinalize(ctx, tr)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %q on_finalize: %w", p.Name(), err))
		}
		if !ok {
			return false, errs
		}
	}
	return true, errs
}

// IterateAfterWrite runs the AfterWrite chain in registration order.
//
// Plugins that do NOT implement ObserveAfterWrite are skipped. There
// is no short-circuit semantic here — the trace is already durable,
// and every interested plugin gets a chance to observe it. Plugin
// errors are NOT returned because AfterWrite has no return value;
// plugins that need to surface errors do so via their own logging /
// counter mechanism. Phase A.1's wiring layer will be the place that
// adds shared telemetry on top of this iterator.
func (r *Registry) IterateAfterWrite(ctx context.Context, tr trace.Trace) {
	for _, p := range r.plugins {
		hook, ok := p.(ObserveAfterWrite)
		if !ok {
			continue
		}
		hook.AfterWrite(ctx, tr)
	}
}

// Plugins returns the registered plugins in registration order.
// Exposed for diagnostics (e.g. /healthz "enabled plugins" list); the
// returned slice is a copy, safe for the caller to mutate.
func (r *Registry) Plugins() []Plugin {
	out := make([]Plugin, len(r.plugins))
	copy(out, r.plugins)
	return out
}
