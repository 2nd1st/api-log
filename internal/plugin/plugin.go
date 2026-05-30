// Package plugin defines the api-log plugin contract.
//
// PHASE A SCAFFOLD ONLY — this package declares the interfaces and registry
// for an observe-class plugin pipeline. NOTHING in this package is wired into
// the proxy or writer hot path yet; constructing a Registry and calling its
// methods today is a no-op from the proxy's POV. The Phase A.1 commit adds
// the wiring in cmd/api-log/main.go.
//
// Why ship the scaffold separately:
//
//   - Per PHILOSOPHY principle 2 ("capture, never interferes"), any code that
//     can affect what flows through api-log is the surface the project most
//     aggressively defends. Landing the interface + first plugin as a
//     buildable artifact (with tests) — without yet allowing it to run on
//     the trace path — gives reviewers a stable contract to evaluate
//     before any behavior changes ship.
//
//   - Per PHILOSOPHY principle 6 ("filesystem is truth"), the only honest
//     way for a plugin to skip a trace is to drop it before the writer ever
//     sees it. That is what BeforeRecord does. Observe-class is the strict
//     subset that does not violate principles 1, 2, 3, or 6; it is the
//     entire surface Phase A is permitted to expose.
//
//   - The interfere-class hooks (mutate_req, before_forward) sketched in
//     uiux-research/plugin.md §§ 2.1, 2.2 are intentionally NOT present.
//     They require explicit philosophy amendments that have not been
//     ratified. See uiux-research/plugin.md § 8 for the deferred phase
//     plan.
//
// Plugins are registered in-process at construction time; there is no
// hot-load and no file uploads. The Registry is constructed in main(),
// fed the operator's enabled list from config, and handed to the trace
// finalize block as a read-only collaborator.
package plugin

import (
	"context"
	"fmt"

	"github.com/leoyun/api-log/internal/trace"
)

// Plugin is the base contract every observe-class plugin satisfies.
//
// A plugin opts into individual hooks by also implementing one of the
// hook interfaces (ObserveBeforeRecord, ObserveAfterRecord). The base
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

// ObserveBeforeRecord is the observe-class hook called BEFORE the writer
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
//     notified until every BeforeRecord plugin has returned.
//   - Runs in registration order. Returning shouldRecord=false from any
//     plugin SHORT-CIRCUITS the iteration: subsequent plugins do not
//     see the dropped trace.
type ObserveBeforeRecord interface {
	Plugin
	BeforeRecord(ctx context.Context, tr trace.Trace) (shouldRecord bool, err error)
}

// ObserveAfterRecord is the observe-class hook called AFTER the writer
// chain has finished. It fires after the JSONL line is on disk and the
// SQLite row has been upserted — i.e. the trace is durable and queryable.
//
// AfterRecord is the natural home for side-effecting observation:
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
//     AfterRecord fires. See uiux-research/plugin.md § 10 "after_record
//     durability semantics" — Phase A ships best-effort and explicitly
//     documents it.)
//   - Runs in registration order, sequentially per trace. Phase A does
//     NOT fan out to a worker pool; that is Phase B territory if the
//     observed plugins prove load-bearing.
//   - Errors are logged. There is no way for an AfterRecord plugin to
//     un-write an already-recorded trace (principle 6: filesystem is
//     truth, append-only). Operators learn about plugin errors via
//     /healthz counters.
type ObserveAfterRecord interface {
	Plugin
	AfterRecord(ctx context.Context, tr trace.Trace)
}

// Registry holds the operator's enabled plugins in registration order.
//
// The order matters: BeforeRecord plugins fire in registration order,
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

// IterateBeforeRecord runs the BeforeRecord chain in registration order.
//
// Semantics:
//
//   - Plugins that do NOT implement ObserveBeforeRecord are skipped
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
func (r *Registry) IterateBeforeRecord(ctx context.Context, tr trace.Trace) (shouldRecord bool, errs []error) {
	shouldRecord = true
	for _, p := range r.plugins {
		hook, ok := p.(ObserveBeforeRecord)
		if !ok {
			continue
		}
		ok, err := hook.BeforeRecord(ctx, tr)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %q before_record: %w", p.Name(), err))
		}
		if !ok {
			return false, errs
		}
	}
	return true, errs
}

// IterateAfterRecord runs the AfterRecord chain in registration order.
//
// Plugins that do NOT implement ObserveAfterRecord are skipped. There
// is no short-circuit semantic here — the trace is already durable,
// and every interested plugin gets a chance to observe it. Plugin
// errors are NOT returned because AfterRecord has no return value;
// plugins that need to surface errors do so via their own logging /
// counter mechanism. Phase A.1's wiring layer will be the place that
// adds shared telemetry on top of this iterator.
func (r *Registry) IterateAfterRecord(ctx context.Context, tr trace.Trace) {
	for _, p := range r.plugins {
		hook, ok := p.(ObserveAfterRecord)
		if !ok {
			continue
		}
		hook.AfterRecord(ctx, tr)
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
