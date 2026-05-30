package v2

import (
	"context"
)

// BeforePlugin is the BEFORE-hook contract: post-receive, pre-forward.
//
// Implementations:
//
//   - Treat ParsedRequest as logically read-only. Return a Mutated copy
//     when you want to change a field.
//   - Never return error; if you fail internally, log via slog and
//     return {Action: ActionContinue}. The dispatcher fails open.
//   - Panics are recovered by the dispatcher (recover.go) with the same
//     fail-open semantics.
//
// Name is the TYPE name (e.g. "watermark", "text-replace"). The
// per-instance ID is held by the Registry and is not the plugin's
// concern. Plugin authors typically embed a zero-value baseline and
// override Name only.
type BeforePlugin interface {
	// Name returns the stable type identifier. Used in trace markers,
	// logs, and the viewer Settings dropdown.
	Name() string

	// OnBefore inspects req and decides the next action.
	//
	// cfg is the per-instance config blob, passed by-value to each call
	// so plugins do not have to manage their own atomic-load. cfg is a
	// shallow copy; plugins that mutate map values inside the call may
	// race with other goroutines reading the same instance.
	OnBefore(ctx context.Context, req *ParsedRequest, cfg map[string]any) BeforeResult
}

// AfterPlugin is the AFTER-hook contract: post-upstream-response,
// pre-client-send.
//
// Two branches, dispatched by req.Streaming:
//
//   - Non-streaming. AfterContext.Response is non-nil. The plugin
//     inspects req+ac.Response, returns {Action: ActionMutate, Mutated:
//     <new ParsedResponse>} or {Action: ActionContinue} or
//     {Action: ActionIntercept, Intercept: <...>}.
//
//   - Streaming. AfterContext.Response is nil. The plugin registers
//     semantic callbacks on ac (OnContentDelta, OnReasoningDelta,
//     OnLastTextDelta) — see AfterContext below — then returns
//     {Action: ActionContinue}. The dispatcher applies the registered
//     transforms as events flow. ActionMutate is ignored in the
//     streaming branch (there is no buffered response to replace);
//     ActionIntercept still works (the dispatcher drops the stream and
//     serves the intercept body).
//
// OnAfter is a ONE-SHOT registration call in the streaming branch, not
// a per-event call. Per-event work happens inside the callbacks.
//
// Forward compatibility (spec §10.6): Phase D will add an opt-in
// ToolCallMutator interface detected via type assertion (Go stdlib's
// io.WriterTo / io.ReaderFrom evolution pattern). Existing AfterPlugin
// implementations will NOT need to change. Plugins implementing
// ToolCallMutator will gain an
//
//	OnToolCall(ctx, req, call *ToolCall) ToolCallResult
//
// callback for buffered tool-call arguments (Continue / Mutate the
// args / Intercept the single call). The dispatcher will spin up the
// buffer-then-expose machinery only when at least one registered
// plugin opts in via ToolCallMutator. The name "ToolCallMutator" is
// the verbatim interface identifier Phase D will land in this package;
// it is referenced here so a search for the name surfaces both the
// commitment and this note.
type AfterPlugin interface {
	Name() string
	OnAfter(ctx context.Context, req *ParsedRequest, ac *AfterContext, cfg map[string]any) AfterResult
}

// AfterContext is the per-call collaboration handle the framework
// passes to AfterPlugin.OnAfter. Field availability depends on
// req.Streaming.
type AfterContext struct {
	// Response is the buffered upstream response. Non-nil in the
	// non-streaming branch; nil when streaming.
	Response *ParsedResponse

	// onContentDelta is the registered chain of content-text transforms,
	// applied in registration order to each content delta event the
	// classifier extracts a text payload from. Plugins push onto this
	// chain via OnContentDelta.
	onContentDelta []func(string) string

	// onReasoningDelta is the same idea for reasoning/thinking text.
	onReasoningDelta []func(string) string

	// onLastTextDelta is the registered chain applied to the LAST
	// text-delta of each content block. The dispatcher buffers one
	// event of lookahead per block; on the block's terminator
	// (Anthropic content_block_stop, OpenAI Responses
	// response.output_text.done, or stream EOF for protocols without a
	// per-block terminator), it fires these callbacks on the buffered
	// text and re-emits the mutated delta. Plugins push via
	// OnLastTextDelta.
	onLastTextDelta []func(blockIndex int, text string) string

	// beforeFinish is preserved as a struct field for source
	// compatibility with R0 plugins compiled against the synthesize-
	// new-event design. The framework no longer calls these callbacks
	// (the R1 amendment replaced the "emit a new content block after
	// the terminator" model with "mutate the last delta of an existing
	// block"). New code should use OnLastTextDelta.
	//
	// Deprecated: register OnLastTextDelta instead. EmitBeforeFinish
	// callbacks are accepted but never fired.
	beforeFinish []func(emit func(text string))
}

// OnContentDelta registers a transform applied to each content delta
// text payload as the stream flows. Streaming branch only — no-op when
// AfterContext.Response is non-nil (the non-streaming code path does
// not invoke registered callbacks).
//
// The transform receives the delta text (after the classifier extracted
// it from the protocol-specific event) and returns the replacement text.
// Returning the input unchanged is fine and cheap.
//
// IMPORTANT: tool_use input_json_delta events are a distinct classifier
// class and NEVER reach this chain (spec §10.6). Plugins do not need
// to defensively check for tool-call arg JSON inside delta text.
func (ac *AfterContext) OnContentDelta(fn func(text string) string) {
	if ac == nil || fn == nil {
		return
	}
	ac.onContentDelta = append(ac.onContentDelta, fn)
}

// OnReasoningDelta registers a transform for reasoning/thinking text
// delta events (Anthropic thinking_delta, OpenAI Responses reasoning
// summary delta). Same semantics as OnContentDelta.
func (ac *AfterContext) OnReasoningDelta(fn func(text string) string) {
	if ac == nil || fn == nil {
		return
	}
	ac.onReasoningDelta = append(ac.onReasoningDelta, fn)
}

// OnLastTextDelta registers a transform that runs on the LAST
// text-delta of each content block (block index passed in), giving
// plugins a place to land an "ends-with" mutation that produces a
// wire-valid sequence: the buffered delta is rewritten in place and
// followed by the block's protocol-specific terminator
// (content_block_stop / response.output_text.done) without any
// synthesized event after the overall stream terminator.
//
// Used by text-append AFTER mode for the "policy footer" use case.
// Returning the input unchanged is cheap and safe.
//
// IMPORTANT: tool_use input_json_delta events are a distinct classifier
// class and NEVER reach this chain (spec §10.6 carve-out).
func (ac *AfterContext) OnLastTextDelta(fn func(blockIndex int, text string) string) {
	if ac == nil || fn == nil {
		return
	}
	ac.onLastTextDelta = append(ac.onLastTextDelta, fn)
}

// EmitBeforeFinish is retained as a no-op registration for source
// compatibility with R0 plugins. Callbacks registered here are NEVER
// fired by the framework; new code should use OnLastTextDelta.
//
// Deprecated: prefer OnLastTextDelta. See AfterContext.beforeFinish
// docstring for the migration rationale (R1 amendment 2026-05-30).
func (ac *AfterContext) EmitBeforeFinish(fn func(emit func(text string))) {
	if ac == nil || fn == nil {
		return
	}
	ac.beforeFinish = append(ac.beforeFinish, fn)
}

// ContentDeltaTransforms returns the registered content-delta
// transforms in registration order. Exported for the stream dispatcher
// and tests; plugin authors should use OnContentDelta to register.
func (ac *AfterContext) ContentDeltaTransforms() []func(string) string {
	if ac == nil {
		return nil
	}
	out := make([]func(string) string, len(ac.onContentDelta))
	copy(out, ac.onContentDelta)
	return out
}

// ReasoningDeltaTransforms returns the registered reasoning-delta
// transforms in registration order.
func (ac *AfterContext) ReasoningDeltaTransforms() []func(string) string {
	if ac == nil {
		return nil
	}
	out := make([]func(string) string, len(ac.onReasoningDelta))
	copy(out, ac.onReasoningDelta)
	return out
}

// LastTextDeltaCallbacks returns the registered last-text-delta
// transforms in registration order. Exported for the stream dispatcher
// and tests; plugin authors should use OnLastTextDelta to register.
func (ac *AfterContext) LastTextDeltaCallbacks() []func(blockIndex int, text string) string {
	if ac == nil {
		return nil
	}
	out := make([]func(blockIndex int, text string) string, len(ac.onLastTextDelta))
	copy(out, ac.onLastTextDelta)
	return out
}

// BeforeFinishCallbacks returns the registered before-finish callbacks
// in registration order.
//
// Deprecated: the framework no longer invokes these callbacks (R1
// amendment). Retained so tests and external consumers compiled
// against R0 still build. Use LastTextDeltaCallbacks instead.
func (ac *AfterContext) BeforeFinishCallbacks() []func(emit func(text string)) {
	if ac == nil {
		return nil
	}
	out := make([]func(emit func(text string)), len(ac.beforeFinish))
	copy(out, ac.beforeFinish)
	return out
}
