package v2

import (
	"encoding/json"
	"fmt"

	"github.com/leoyun/api-log/internal/sse"
)

// EventClass is the semantic class of an SSE event, computed by the
// per-protocol classifier.
//
// The classifier is the single line of defense for the spec §10.6
// carve-out: tool_use input_json_delta events are a DISTINCT class
// (ClassToolUseDelta). The dispatcher routes by class, and content
// transforms only ever see ClassContentDelta / ClassReasoningDelta.
// Misclassifying a tool-use event as content is the bug that breaks
// the carve-out; the test suite drives this case explicitly.
type EventClass int

const (
	// ClassOther covers events the dispatcher passes through unchanged
	// without inspecting their payload (start/stop markers, pings,
	// unknown event types).
	ClassOther EventClass = iota

	// ClassContentDelta is an event carrying a text fragment of the
	// assistant's content. Anthropic content_block_delta with text_delta,
	// Chat data with delta.content, Responses response.output_text.delta,
	// Gemini text part deltas.
	ClassContentDelta

	// ClassReasoningDelta is an event carrying a text fragment of the
	// model's reasoning / thinking trace. Anthropic content_block_delta
	// with thinking_delta, Responses reasoning_summary_text.delta.
	ClassReasoningDelta

	// ClassToolUseDelta is an event carrying a tool-call argument
	// fragment. Anthropic content_block_delta with input_json_delta,
	// Chat delta.tool_calls[*].function.arguments, Responses
	// response.function_call_arguments.delta.
	//
	// Per spec §10.6, these events are PASSED THROUGH UNTOUCHED by the
	// AFTER-hook dispatcher even when content transforms are registered.
	ClassToolUseDelta

	// ClassTerminal marks the stream-end event. Anthropic message_stop,
	// Chat data:[DONE] (out-of-band — never reaches the dispatcher as
	// an event), Responses response.completed. Pass-through; any
	// remaining buffered last-delta is flushed (with OnLastTextDelta
	// applied) before the terminal event re-emits.
	ClassTerminal

	// ClassContentBlockStop marks the per-content-block terminator.
	// Anthropic content_block_stop, OpenAI Responses
	// response.output_text.done. The dispatcher pairs this with the
	// buffered last text delta of the same block: it runs the
	// OnLastTextDelta chain on the buffered text, re-emits the
	// mutated delta, then emits the stop event. For protocols
	// without a per-block terminator (Chat, Gemini) the EOF flush
	// covers the same case.
	ClassContentBlockStop
)

// ClassifiedEvent is the dispatcher's working tuple. Text is the
// extracted text payload when Class is ContentDelta / ReasoningDelta;
// empty otherwise. Mutator writes a new event back (with the same
// envelope and a substituted text payload).
//
// BlockIndex is the content-block index for ContentDelta and
// ContentBlockStop events. Anthropic Messages carries this in the
// wire format ("index" field); other protocols use 0 as the virtual
// block index for the single text stream they emit. The dispatcher
// uses BlockIndex to pair a buffered last-delta with the right
// ContentBlockStop.
type ClassifiedEvent struct {
	Event      sse.Event
	Class      EventClass
	Text       string
	BlockIndex int
}

// Classify computes the semantic class of an SSE event for the given
// protocol, extracting the text payload for content/reasoning deltas.
//
// Tool-use deltas are recognized and returned as ClassToolUseDelta;
// their text payload is intentionally NOT extracted (the dispatcher
// must not have a path that could route tool-call JSON into a content
// transform).
//
// Unknown event shapes return ClassOther; the dispatcher re-emits them
// unchanged.
func Classify(proto Protocol, ev sse.Event) ClassifiedEvent {
	switch proto {
	case ProtocolMessages:
		return classifyMessages(ev)
	case ProtocolChat:
		return classifyChat(ev)
	case ProtocolResponses:
		return classifyResponses(ev)
	case ProtocolGemini:
		return classifyGemini(ev)
	default:
		return ClassifiedEvent{Event: ev, Class: ClassOther}
	}
}

func classifyMessages(ev sse.Event) ClassifiedEvent {
	switch ev.Name {
	case "message_stop":
		return ClassifiedEvent{Event: ev, Class: ClassTerminal}
	case "content_block_stop":
		var frame struct {
			Index int `json:"index"`
		}
		// Best-effort index extraction; an unparseable frame falls back
		// to block 0 — flushing the wrong block is recoverable (we
		// emit the buffered delta unchanged), classifying it as Other
		// would silently drop the OnLastTextDelta opportunity.
		_ = json.Unmarshal(ev.Data, &frame)
		return ClassifiedEvent{Event: ev, Class: ClassContentBlockStop, BlockIndex: frame.Index}
	case "content_block_delta":
		var frame struct {
			Index int `json:"index"`
			Delta *struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &frame); err != nil || frame.Delta == nil {
			return ClassifiedEvent{Event: ev, Class: ClassOther}
		}
		switch frame.Delta.Type {
		case "text_delta":
			return ClassifiedEvent{Event: ev, Class: ClassContentDelta, Text: frame.Delta.Text, BlockIndex: frame.Index}
		case "thinking_delta":
			return ClassifiedEvent{Event: ev, Class: ClassReasoningDelta, Text: frame.Delta.Thinking, BlockIndex: frame.Index}
		case "input_json_delta":
			return ClassifiedEvent{Event: ev, Class: ClassToolUseDelta, BlockIndex: frame.Index}
		}
		return ClassifiedEvent{Event: ev, Class: ClassOther}
	}
	return ClassifiedEvent{Event: ev, Class: ClassOther}
}

func classifyChat(ev sse.Event) ClassifiedEvent {
	// Chat is data-only — ev.Name is empty for normal frames. The
	// sse.Parser consumes `data: [DONE]` as the stream-done marker and
	// does NOT emit it as an Event, so ClassTerminal does not arise
	// inside the per-event loop. The dispatcher's caller signals
	// terminal-stream out-of-band.
	var frame struct {
		Choices []struct {
			Delta *struct {
				Content   string                       `json:"content"`
				ToolCalls []json.RawMessage            `json:"tool_calls"`
				Reasoning *struct{ Content string }   `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(ev.Data, &frame); err != nil {
		return ClassifiedEvent{Event: ev, Class: ClassOther}
	}
	for _, c := range frame.Choices {
		if c.Delta == nil {
			continue
		}
		if len(c.Delta.ToolCalls) > 0 {
			return ClassifiedEvent{Event: ev, Class: ClassToolUseDelta}
		}
		if c.Delta.Content != "" {
			return ClassifiedEvent{Event: ev, Class: ClassContentDelta, Text: c.Delta.Content}
		}
	}
	return ClassifiedEvent{Event: ev, Class: ClassOther}
}

func classifyResponses(ev sse.Event) ClassifiedEvent {
	switch ev.Name {
	case "response.completed":
		return ClassifiedEvent{Event: ev, Class: ClassTerminal}
	case "response.output_text.done":
		// Per-content-block terminator for the output_text stream.
		// Block index 0 — the Responses wire format does not surface
		// a per-text-block index; the dispatcher treats the whole
		// text stream as a single virtual block.
		return ClassifiedEvent{Event: ev, Class: ClassContentBlockStop, BlockIndex: 0}
	case "response.output_text.delta":
		var frame struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &frame); err != nil {
			return ClassifiedEvent{Event: ev, Class: ClassOther}
		}
		return ClassifiedEvent{Event: ev, Class: ClassContentDelta, Text: frame.Delta, BlockIndex: 0}
	case "response.reasoning_summary_text.delta", "response.reasoning.delta":
		var frame struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &frame); err != nil {
			return ClassifiedEvent{Event: ev, Class: ClassOther}
		}
		return ClassifiedEvent{Event: ev, Class: ClassReasoningDelta, Text: frame.Delta}
	case "response.function_call_arguments.delta":
		return ClassifiedEvent{Event: ev, Class: ClassToolUseDelta}
	}
	return ClassifiedEvent{Event: ev, Class: ClassOther}
}

func classifyGemini(ev sse.Event) ClassifiedEvent {
	// Gemini streams as JSON objects per frame; text lives at
	// candidates[0].content.parts[*].text. functionCall parts mark
	// tool-use deltas.
	var frame struct {
		Candidates []struct {
			Content *struct {
				Parts []struct {
					Text         string          `json:"text"`
					FunctionCall json.RawMessage `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(ev.Data, &frame); err != nil {
		return ClassifiedEvent{Event: ev, Class: ClassOther}
	}
	hasFunctionCall := false
	var text string
	for _, c := range frame.Candidates {
		if c.Content == nil {
			continue
		}
		for _, p := range c.Content.Parts {
			if len(p.FunctionCall) > 0 {
				hasFunctionCall = true
			}
			if p.Text != "" {
				text += p.Text
			}
		}
	}
	if hasFunctionCall {
		return ClassifiedEvent{Event: ev, Class: ClassToolUseDelta}
	}
	if text != "" {
		return ClassifiedEvent{Event: ev, Class: ClassContentDelta, Text: text}
	}
	return ClassifiedEvent{Event: ev, Class: ClassOther}
}

// RewriteDeltaText returns a copy of ev with its text payload replaced
// by newText. Per-protocol writers; the wrapping envelope (deltas,
// frame metadata) is preserved verbatim.
//
// Pre-condition: ev was classified as ClassContentDelta or
// ClassReasoningDelta and the dispatcher is passing back the same
// event with a transformed Text. For other classes the function
// returns the event unchanged.
func RewriteDeltaText(proto Protocol, classified ClassifiedEvent, newText string) (sse.Event, error) {
	if classified.Class != ClassContentDelta && classified.Class != ClassReasoningDelta {
		return classified.Event, nil
	}
	switch proto {
	case ProtocolMessages:
		return rewriteMessagesDelta(classified.Event, classified.Class, newText)
	case ProtocolChat:
		return rewriteChatDelta(classified.Event, newText)
	case ProtocolResponses:
		return rewriteResponsesDelta(classified.Event, classified.Class, newText)
	case ProtocolGemini:
		return rewriteGeminiDelta(classified.Event, newText)
	default:
		return classified.Event, nil
	}
}

func rewriteMessagesDelta(ev sse.Event, class EventClass, newText string) (sse.Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(ev.Data, &raw); err != nil {
		return ev, fmt.Errorf("messages rewrite unmarshal: %w", err)
	}
	delta, ok := raw["delta"].(map[string]any)
	if !ok {
		return ev, fmt.Errorf("messages rewrite: missing delta")
	}
	switch class {
	case ClassContentDelta:
		delta["text"] = newText
	case ClassReasoningDelta:
		delta["thinking"] = newText
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return ev, fmt.Errorf("messages rewrite marshal: %w", err)
	}
	return sse.Event{Name: ev.Name, Data: out, TDeltaMs: ev.TDeltaMs, Offset: ev.Offset}, nil
}

func rewriteChatDelta(ev sse.Event, newText string) (sse.Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(ev.Data, &raw); err != nil {
		return ev, fmt.Errorf("chat rewrite unmarshal: %w", err)
	}
	choices, ok := raw["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ev, fmt.Errorf("chat rewrite: missing choices")
	}
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := cm["delta"].(map[string]any)
		if !ok {
			continue
		}
		if _, has := delta["content"]; has {
			delta["content"] = newText
			break
		}
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return ev, fmt.Errorf("chat rewrite marshal: %w", err)
	}
	return sse.Event{Name: ev.Name, Data: out, TDeltaMs: ev.TDeltaMs, Offset: ev.Offset}, nil
}

func rewriteResponsesDelta(ev sse.Event, _ EventClass, newText string) (sse.Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(ev.Data, &raw); err != nil {
		return ev, fmt.Errorf("responses rewrite unmarshal: %w", err)
	}
	raw["delta"] = newText
	out, err := json.Marshal(raw)
	if err != nil {
		return ev, fmt.Errorf("responses rewrite marshal: %w", err)
	}
	return sse.Event{Name: ev.Name, Data: out, TDeltaMs: ev.TDeltaMs, Offset: ev.Offset}, nil
}

func rewriteGeminiDelta(ev sse.Event, newText string) (sse.Event, error) {
	var raw map[string]any
	if err := json.Unmarshal(ev.Data, &raw); err != nil {
		return ev, fmt.Errorf("gemini rewrite unmarshal: %w", err)
	}
	cands, ok := raw["candidates"].([]any)
	if !ok || len(cands) == 0 {
		return ev, fmt.Errorf("gemini rewrite: missing candidates")
	}
	for _, c := range cands {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		content, ok := cm["content"].(map[string]any)
		if !ok {
			continue
		}
		parts, ok := content["parts"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if _, has := pm["text"]; has {
				pm["text"] = newText
				break
			}
		}
		break
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return ev, fmt.Errorf("gemini rewrite marshal: %w", err)
	}
	return sse.Event{Name: ev.Name, Data: out, TDeltaMs: ev.TDeltaMs, Offset: ev.Offset}, nil
}

// SynthesizeContentDelta builds a new sse.Event carrying the given
// text as the content delta payload, in the protocol's expected event
// shape. Used by EmitBeforeFinish to land a final framework-emitted
// text fragment just before the terminal event.
//
// Returns an error for protocols where the synthesis shape is not
// pinned down (Gemini in v1).
func SynthesizeContentDelta(proto Protocol, text string) (sse.Event, error) {
	switch proto {
	case ProtocolMessages:
		data := fmt.Sprintf(
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`,
			mustJSONString(text),
		)
		return sse.Event{Name: "content_block_delta", Data: json.RawMessage(data)}, nil
	case ProtocolChat:
		data := fmt.Sprintf(
			`{"choices":[{"index":0,"delta":{"content":%s}}]}`,
			mustJSONString(text),
		)
		return sse.Event{Data: json.RawMessage(data)}, nil
	case ProtocolResponses:
		data := fmt.Sprintf(
			`{"type":"response.output_text.delta","delta":%s}`,
			mustJSONString(text),
		)
		return sse.Event{Name: "response.output_text.delta", Data: json.RawMessage(data)}, nil
	case ProtocolGemini:
		return sse.Event{}, fmt.Errorf("v2.SynthesizeContentDelta: gemini deferred")
	default:
		return sse.Event{}, fmt.Errorf("v2.SynthesizeContentDelta: unknown protocol %s", proto)
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal on a string is total — strings always encode.
		// Falling through to a safe empty string keeps the package
		// no-panic per the spec's fail-open invariant.
		return `""`
	}
	return string(b)
}

// StreamDispatcher applies a chain of registered content/reasoning
// transforms (held on an AfterContext) to an event stream, in order,
// and produces the re-emitted event stream.
//
// Contract:
//
//   - Tool-use deltas (ClassToolUseDelta) PASS THROUGH UNTOUCHED even
//     when content transforms are registered. This is the spec §10.6
//     carve-out enforced at the dispatcher level.
//   - Content / reasoning deltas have each registered transform applied
//     in registration order; the final text is written back into the
//     event via RewriteDeltaText.
//   - One event of lookahead per content block: the dispatcher holds
//     the most recent content_delta of each block index, flushing it
//     when (a) the next content_delta for the same block arrives, (b)
//     a content_block_stop for that block arrives — in which case the
//     OnLastTextDelta chain runs first — or (c) the stream EOF flush
//     fires.
//   - Per-block stops (ClassContentBlockStop) emit the buffered last
//     delta with OnLastTextDelta applied, then the stop event itself.
//     This is the structural fix for the R1 amendment: append-to-
//     last-delta produces a wire-valid sequence ending with the
//     protocol's terminator, not a synthesized block after it.
//   - Per-event Process is the synchronous primitive. WrapStream is
//     the channel wrapper that calls Process and FlushBeforeFinish on
//     EOF.
type StreamDispatcher struct {
	Protocol Protocol
	After    *AfterContext

	// pending holds the most recent content-delta per block index,
	// already re-emitted through the OnContentDelta chain. The
	// dispatcher flushes pending on the next event for the same block,
	// on the block's terminator (applying OnLastTextDelta first), or
	// on EOF (applying OnLastTextDelta first).
	pending map[int]ClassifiedEvent
}

// pendingMap lazy-inits the dispatcher's per-block buffer so the
// zero-value StreamDispatcher works without an explicit constructor.
func (d *StreamDispatcher) pendingMap() map[int]ClassifiedEvent {
	if d.pending == nil {
		d.pending = make(map[int]ClassifiedEvent)
	}
	return d.pending
}

// Process classifies one event, applies registered transforms, and
// returns the events to re-emit. Length depends on the event class:
// content_delta usually returns 0 (buffered) or 1 (the previously
// buffered delta flushed in front of the new one); content_block_stop
// returns up to 2 (flushed-last-delta + stop); other classes return 1.
func (d *StreamDispatcher) Process(ev sse.Event) []sse.Event {
	classified := Classify(d.Protocol, ev)
	switch classified.Class {
	case ClassContentDelta:
		return d.handleContentDelta(classified, ev)
	case ClassReasoningDelta:
		text := classified.Text
		for _, fn := range d.After.ReasoningDeltaTransforms() {
			text = fn(text)
		}
		// Reasoning deltas pass through without flushing the per-block
		// content buffer. Anthropic streams thinking blocks at a
		// distinct content_block index; the text-content buffer is
		// independent and must keep waiting for its own block stop /
		// EOF before OnLastTextDelta fires.
		if text == classified.Text {
			return []sse.Event{ev}
		}
		out, err := RewriteDeltaText(d.Protocol, classified, text)
		if err != nil {
			return []sse.Event{ev}
		}
		return []sse.Event{out}
	case ClassToolUseDelta:
		// Carve-out: tool-use deltas pass through untouched. Tool-use
		// blocks have their own indices; the text-content buffer
		// stays pending — Anthropic's content_block_stop for the
		// text block is the trigger we wait on.
		return []sse.Event{ev}
	case ClassContentBlockStop:
		return d.handleContentBlockStop(classified, ev)
	case ClassTerminal:
		// Stream-level terminator. Flush any remaining buffered
		// last-deltas (applying OnLastTextDelta) so the suffix lands
		// before the protocol's terminator. This handles the Chat
		// path (no per-block stop) and is also defensive for
		// well-formed Messages / Responses streams that already
		// flushed via ContentBlockStop.
		out := d.flushAll(true)
		return append(out, ev)
	default:
		// ClassOther — pings, content_block_start, message_delta,
		// response.output_item.added, and any unknown frames. Pass
		// through; keep the pending buffer intact. Flushing here
		// would silently drop the OnLastTextDelta opportunity in
		// streams that interleave a ping between the last text
		// delta and content_block_stop — the per-block terminator
		// is the trigger we wait on.
		return []sse.Event{ev}
	}
}

// handleContentDelta applies the OnContentDelta chain to the new
// event, then buffers it. If a previous delta for the same block was
// pending, that previous one is now NOT-the-last and is flushed as-is
// in front of the new buffer.
func (d *StreamDispatcher) handleContentDelta(classified ClassifiedEvent, ev sse.Event) []sse.Event {
	text := classified.Text
	for _, fn := range d.After.ContentDeltaTransforms() {
		text = fn(text)
	}
	stored := classified
	stored.Text = text
	if text != classified.Text {
		rewritten, err := RewriteDeltaText(d.Protocol, classified, text)
		if err == nil {
			stored.Event = rewritten
		}
	}

	pending := d.pendingMap()
	var out []sse.Event
	// Flush any other-block pending first to preserve ordering. Same-
	// block pending becomes "not the last" and flushes without the
	// OnLastTextDelta hook.
	for idx, prev := range pending {
		if idx == classified.BlockIndex {
			continue
		}
		out = append(out, prev.Event)
		delete(pending, idx)
	}
	if prev, ok := pending[classified.BlockIndex]; ok {
		out = append(out, prev.Event)
	}
	pending[classified.BlockIndex] = stored
	return out
}

// handleContentBlockStop flushes the buffered last delta for the
// stop's block index with the OnLastTextDelta chain applied, then
// emits the stop event itself.
func (d *StreamDispatcher) handleContentBlockStop(classified ClassifiedEvent, ev sse.Event) []sse.Event {
	pending := d.pendingMap()
	// Flush any other-block pending first.
	var out []sse.Event
	for idx, prev := range pending {
		if idx == classified.BlockIndex {
			continue
		}
		out = append(out, prev.Event)
		delete(pending, idx)
	}
	if prev, ok := pending[classified.BlockIndex]; ok {
		out = append(out, d.applyLastTextDelta(prev))
		delete(pending, classified.BlockIndex)
	}
	out = append(out, ev)
	return out
}

// applyLastTextDelta runs the OnLastTextDelta chain on a buffered
// delta and returns the resulting event. When the chain leaves the
// text unchanged, the buffered (already-OnContentDelta-applied) event
// is emitted as-is.
func (d *StreamDispatcher) applyLastTextDelta(buffered ClassifiedEvent) sse.Event {
	cbs := d.After.LastTextDeltaCallbacks()
	if len(cbs) == 0 {
		return buffered.Event
	}
	text := buffered.Text
	for _, fn := range cbs {
		text = fn(buffered.BlockIndex, text)
	}
	if text == buffered.Text {
		return buffered.Event
	}
	// Build a fresh ClassifiedEvent describing the buffered event so
	// RewriteDeltaText sees the correct Class — buffered.Event has
	// the prior text payload, but its Name / envelope are intact.
	classified := ClassifiedEvent{
		Event:      buffered.Event,
		Class:      ClassContentDelta,
		Text:       buffered.Text,
		BlockIndex: buffered.BlockIndex,
	}
	rewritten, err := RewriteDeltaText(d.Protocol, classified, text)
	if err != nil {
		return buffered.Event
	}
	return rewritten
}

// flushAll empties the per-block buffer. When applyLast is true, the
// OnLastTextDelta chain runs on each (used at EOF and on the stream
// terminator); otherwise the buffer is emitted as-is. Returned slice
// preserves ascending block-index order so multi-block streams flush
// deterministically.
func (d *StreamDispatcher) flushAll(applyLast bool) []sse.Event {
	if len(d.pending) == 0 {
		return nil
	}
	// Sort ascending so the flush order is deterministic and matches
	// the typical upstream ordering. Map iteration is non-deterministic
	// in Go; sorting matters for the EOF case where multiple blocks
	// might still be pending (rare in practice but legal).
	keys := make([]int, 0, len(d.pending))
	for k := range d.pending {
		keys = append(keys, k)
	}
	// Insertion sort — keys is tiny in practice (one content text
	// block plus zero or one reasoning block per response).
	for i := 1; i < len(keys); i++ {
		j := i
		for j > 0 && keys[j-1] > keys[j] {
			keys[j-1], keys[j] = keys[j], keys[j-1]
			j--
		}
	}
	out := make([]sse.Event, 0, len(keys))
	for _, k := range keys {
		prev := d.pending[k]
		if applyLast {
			out = append(out, d.applyLastTextDelta(prev))
		} else {
			out = append(out, prev.Event)
		}
		delete(d.pending, k)
	}
	return out
}

// FlushBeforeFinish runs any pending last-delta buffers through the
// OnLastTextDelta chain (so suffix-style mutations land at EOF for
// protocols without a per-block terminator, notably Chat) and returns
// the resulting events.
//
// The R1 amendment changed the semantics of this function. Previously
// it invoked deprecated EmitBeforeFinish callbacks to synthesize new
// content blocks AFTER the protocol terminator (which produced a
// wire-invalid sequence). The new behavior flushes the buffered
// last-delta of each block in place; callers no longer need to hold
// the terminal event.
//
// Calling FlushBeforeFinish twice yields nothing on the second call
// (the buffer is drained).
func (d *StreamDispatcher) FlushBeforeFinish() []sse.Event {
	return d.flushAll(true)
}

// WrapStream is the channel-form integration of the dispatcher.
//
// It reads from upstream until that channel is closed, applies per-
// event transforms, and writes the re-emitted events to the returned
// channel. On upstream close it runs FlushBeforeFinish — flushing any
// remaining buffered last-delta — BEFORE closing the output channel.
//
// The output channel buffer matches the input buffer size. The caller
// owns the input channel (closes it on upstream EOF or context
// cancel); the wrapper owns the output channel (closes it after
// flushing).
//
// Three correctness invariants:
//
//  1. OnLastTextDelta callbacks fire on per-block terminators
//     (content_block_stop / response.output_text.done) when the
//     upstream emits one, and on EOF otherwise — uniform behavior
//     across all four protocols including Chat (no terminal event at
//     all).
//
//  2. Tool-use deltas pass through untouched at every stage; the
//     dispatcher never inspects nor mutates their payloads.
//
//  3. No event is emitted after the protocol's stream terminator
//     (message_stop, response.completed) — the last-delta mutation
//     happens BEFORE the terminator flows through Process.
func (d *StreamDispatcher) WrapStream(upstream <-chan sse.Event) <-chan sse.Event {
	out := make(chan sse.Event, cap(upstream))
	go func() {
		defer close(out)
		for ev := range upstream {
			for _, mut := range d.Process(ev) {
				out <- mut
			}
		}
		for _, ev := range d.FlushBeforeFinish() {
			out <- ev
		}
	}()
	return out
}
