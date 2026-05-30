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
	// Chat data:[DONE], Responses response.completed. The dispatcher
	// runs registered EmitBeforeFinish callbacks immediately before
	// re-emitting the terminal event.
	ClassTerminal
)

// ClassifiedEvent is the dispatcher's working tuple. Text is the
// extracted text payload when Class is ContentDelta / ReasoningDelta;
// empty otherwise. Mutator writes a new event back (with the same
// envelope and a substituted text payload).
type ClassifiedEvent struct {
	Event sse.Event
	Class EventClass
	// Text is the extracted text payload for content/reasoning deltas.
	// Empty for other classes.
	Text string
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
	case "content_block_delta":
		var frame struct {
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
			return ClassifiedEvent{Event: ev, Class: ClassContentDelta, Text: frame.Delta.Text}
		case "thinking_delta":
			return ClassifiedEvent{Event: ev, Class: ClassReasoningDelta, Text: frame.Delta.Thinking}
		case "input_json_delta":
			return ClassifiedEvent{Event: ev, Class: ClassToolUseDelta}
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
	case "response.output_text.delta":
		var frame struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &frame); err != nil {
			return ClassifiedEvent{Event: ev, Class: ClassOther}
		}
		return ClassifiedEvent{Event: ev, Class: ClassContentDelta, Text: frame.Delta}
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
//   - Per-event Process does NOT emit before-finish deltas. End-of-
//     stream is signaled by the caller closing the input channel of
//     WrapStream (or by an explicit call to FlushBeforeFinish for the
//     synchronous-loop integration). This keeps the trigger uniform
//     across all four protocols even though only Messages and
//     Responses emit a named terminal event (Chat's [DONE] is
//     swallowed by the SSE parser; Gemini has no terminal sentinel).
//
// The dispatcher itself is synchronous and goroutine-free. WrapStream
// is the channel wrapper that adds the EOF-trigger semantics.
type StreamDispatcher struct {
	Protocol Protocol
	After    *AfterContext
}

// Process classifies one event, applies registered transforms, and
// returns the event to re-emit. Returns the original event unchanged
// for ClassOther / ClassTerminal / ClassToolUseDelta.
//
// Returns a slice (always length 1) for symmetry with WrapStream and
// to leave room for a future variant that might split content blocks.
func (d *StreamDispatcher) Process(ev sse.Event) []sse.Event {
	classified := Classify(d.Protocol, ev)
	switch classified.Class {
	case ClassContentDelta:
		text := classified.Text
		for _, fn := range d.After.ContentDeltaTransforms() {
			text = fn(text)
		}
		if text == classified.Text {
			return []sse.Event{ev}
		}
		out, err := RewriteDeltaText(d.Protocol, classified, text)
		if err != nil {
			// Rewrite failure → preserve original (fail-open).
			return []sse.Event{ev}
		}
		return []sse.Event{out}
	case ClassReasoningDelta:
		text := classified.Text
		for _, fn := range d.After.ReasoningDeltaTransforms() {
			text = fn(text)
		}
		if text == classified.Text {
			return []sse.Event{ev}
		}
		out, err := RewriteDeltaText(d.Protocol, classified, text)
		if err != nil {
			return []sse.Event{ev}
		}
		return []sse.Event{out}
	case ClassToolUseDelta:
		// Carve-out: tool-use deltas pass through untouched even when
		// content transforms are registered. The classifier already
		// extracted no Text; we re-emit the original.
		return []sse.Event{ev}
	case ClassTerminal:
		// Terminal events pass through unchanged. The before-finish
		// emit hooks fire on stream EOF (WrapStream / FlushBeforeFinish),
		// not on the terminal event itself — Chat has no terminal
		// event at all and Gemini's terminal is also out-of-band, so
		// uniform handling means "react to channel close" rather than
		// "react to a protocol-specific sentinel."
		return []sse.Event{ev}
	default:
		return []sse.Event{ev}
	}
}

// FlushBeforeFinish runs every registered EmitBeforeFinish callback
// and returns the synthesized events the framework should write to
// the client before closing the response stream.
//
// Used by both the channel-form WrapStream (on input channel close)
// and any synchronous integration that wants the same EOF semantics.
// Calling FlushBeforeFinish twice yields a duplicate flush; the
// framework MUST call it exactly once per stream.
func (d *StreamDispatcher) FlushBeforeFinish() []sse.Event {
	cbs := d.After.BeforeFinishCallbacks()
	if len(cbs) == 0 {
		return nil
	}
	out := make([]sse.Event, 0, len(cbs))
	for _, fn := range cbs {
		fn(func(text string) {
			delta, err := SynthesizeContentDelta(d.Protocol, text)
			if err != nil {
				return
			}
			out = append(out, delta)
		})
	}
	return out
}

// WrapStream is the channel-form integration of the dispatcher.
//
// It reads from upstream until that channel is closed, applies per-
// event transforms, and writes the re-emitted events to the returned
// channel. On upstream close it runs FlushBeforeFinish — synthesizing
// the registered after-finish deltas — BEFORE closing the output
// channel.
//
// The output channel buffer matches the input buffer size. The caller
// owns the input channel (closes it on upstream EOF or context
// cancel); the wrapper owns the output channel (closes it after
// flushing).
//
// Two correctness invariants:
//
//   1. EmitBeforeFinish callbacks fire on stream END, not on a
//      protocol-specific terminal event — works for all four
//      protocols including Chat (no terminal event at all).
//
//   2. Tool-use deltas pass through untouched even on EOF; the flush
//      step only adds synthesized content deltas, never tool-use
//      events.
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
