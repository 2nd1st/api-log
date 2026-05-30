package v2

import (
	"context"
	"fmt"
	"log/slog"
)

// safeBefore wraps a BeforePlugin.OnBefore call in defer recover() so
// a plugin panic cannot bring down the proxy. On recover, the function
// returns ActionContinue (request flows unchanged) and records the
// panic on the registry's plugin_errors breadcrumb list (spec §5.3).
//
// This is the only place a panic from plugin code is allowed to be
// turned into a normal Action — the contract is fail-open per spec
// §4.2, and only the dispatcher gets to recover.
func safeBefore(ctx context.Context, r *Registry, inst *Instance, req *ParsedRequest) (res BeforeResult) {
	defer func() {
		if rec := recover(); rec != nil {
			msg := fmt.Sprintf("%v", rec)
			r.recordError(inst.Type, inst.ID, "before", "panic: "+msg)
			slog.Warn("plugin panic",
				"type", inst.Type, "id", inst.ID, "hook", "before",
				"panic", msg)
			res = BeforeResult{Action: ActionContinue}
		}
	}()
	if inst == nil || inst.before == nil {
		return BeforeResult{Action: ActionContinue}
	}
	return inst.before.OnBefore(ctx, req, inst.Config)
}

// safeAfter wraps an AfterPlugin.OnAfter call. Same semantics as
// safeBefore. On panic the function returns ActionContinue so the
// response flows unchanged and the registered streaming callbacks (if
// any) survive — callbacks registered BEFORE the panic remain on the
// AfterContext because we never unwind the AfterContext on panic.
//
// This is a conscious choice: a partial callback registration is
// still safer than dropping all registrations (which would silently
// remove watermark/etc. behavior the operator opted into).
func safeAfter(ctx context.Context, r *Registry, inst *Instance, req *ParsedRequest, ac *AfterContext) (res AfterResult) {
	defer func() {
		if rec := recover(); rec != nil {
			msg := fmt.Sprintf("%v", rec)
			r.recordError(inst.Type, inst.ID, "after", "panic: "+msg)
			slog.Warn("plugin panic",
				"type", inst.Type, "id", inst.ID, "hook", "after",
				"panic", msg)
			res = AfterResult{Action: ActionContinue}
		}
	}()
	if inst == nil || inst.after == nil {
		return AfterResult{Action: ActionContinue}
	}
	return inst.after.OnAfter(ctx, req, ac, inst.Config)
}
