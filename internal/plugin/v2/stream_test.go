package v2

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/leoyun/api-log/internal/sse"
)

// ----- Classifier coverage per protocol ---------------------------

func TestClassify_Messages(t *testing.T) {
	cases := []struct {
		name string
		ev   sse.Event
		want EventClass
		text string
	}{
		{
			name: "text_delta",
			ev: sse.Event{Name: "content_block_delta", Data: json.RawMessage(
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)},
			want: ClassContentDelta,
			text: "hi",
		},
		{
			name: "thinking_delta",
			ev: sse.Event{Name: "content_block_delta", Data: json.RawMessage(
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`)},
			want: ClassReasoningDelta,
			text: "hmm",
		},
		{
			name: "input_json_delta",
			ev: sse.Event{Name: "content_block_delta", Data: json.RawMessage(
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`)},
			want: ClassToolUseDelta,
		},
		{
			name: "message_stop",
			ev:   sse.Event{Name: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
			want: ClassTerminal,
		},
		{
			name: "content_block_stop",
			ev:   sse.Event{Name: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":0}`)},
			want: ClassContentBlockStop,
		},
		{
			name: "content_block_start (no delta)",
			ev:   sse.Event{Name: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":0}`)},
			want: ClassOther,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(ProtocolMessages, tc.ev)
			if got.Class != tc.want {
				t.Errorf("class = %v, want %v", got.Class, tc.want)
			}
			if got.Text != tc.text {
				t.Errorf("text = %q, want %q", got.Text, tc.text)
			}
		})
	}
}

func TestClassify_Chat(t *testing.T) {
	cases := []struct {
		name string
		data string
		want EventClass
		text string
	}{
		{"content delta",
			`{"choices":[{"delta":{"content":"hi"}}]}`,
			ClassContentDelta, "hi"},
		{"tool_call delta",
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
			ClassToolUseDelta, ""},
		{"empty delta",
			`{"choices":[{"delta":{}}]}`,
			ClassOther, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(ProtocolChat, sse.Event{Data: json.RawMessage(tc.data)})
			if got.Class != tc.want {
				t.Errorf("class = %v, want %v", got.Class, tc.want)
			}
			if got.Text != tc.text {
				t.Errorf("text = %q, want %q", got.Text, tc.text)
			}
		})
	}
}

func TestClassify_Responses(t *testing.T) {
	cases := []struct {
		name string
		ev   sse.Event
		want EventClass
		text string
	}{
		{
			name: "output_text.delta",
			ev: sse.Event{Name: "response.output_text.delta", Data: json.RawMessage(
				`{"type":"response.output_text.delta","delta":"hi"}`)},
			want: ClassContentDelta,
			text: "hi",
		},
		{
			name: "output_text.done",
			ev: sse.Event{Name: "response.output_text.done", Data: json.RawMessage(
				`{"type":"response.output_text.done"}`)},
			want: ClassContentBlockStop,
		},
		{
			name: "function_call_arguments.delta",
			ev: sse.Event{Name: "response.function_call_arguments.delta", Data: json.RawMessage(
				`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"x\":1}"}`)},
			want: ClassToolUseDelta,
		},
		{
			name: "reasoning_summary_text.delta",
			ev: sse.Event{Name: "response.reasoning_summary_text.delta", Data: json.RawMessage(
				`{"type":"response.reasoning_summary_text.delta","delta":"hmm"}`)},
			want: ClassReasoningDelta,
			text: "hmm",
		},
		{
			name: "response.completed",
			ev: sse.Event{Name: "response.completed", Data: json.RawMessage(
				`{"type":"response.completed","response":{}}`)},
			want: ClassTerminal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(ProtocolResponses, tc.ev)
			if got.Class != tc.want {
				t.Errorf("class = %v, want %v", got.Class, tc.want)
			}
			if got.Text != tc.text {
				t.Errorf("text = %q, want %q", got.Text, tc.text)
			}
		})
	}
}

// ----- Dispatcher tool_use carve-out (§10.6) ----------------------
//
// The dispatcher MUST NOT route tool-use deltas through content
// transforms even when the match string sits inside the tool-call
// argument JSON.

func TestStreamDispatcher_ToolUseDeltaUntouched(t *testing.T) {
	for _, proto := range []Protocol{ProtocolMessages, ProtocolChat, ProtocolResponses} {
		t.Run(proto.String(), func(t *testing.T) {
			ac := &AfterContext{}
			ac.OnContentDelta(func(text string) string {
				return strings.ReplaceAll(text, "city", "CITY-REPLACED")
			})
			d := &StreamDispatcher{Protocol: proto, After: ac}

			var contentEv, toolEv sse.Event
			switch proto {
			case ProtocolMessages:
				contentEv = sse.Event{Name: "content_block_delta", Data: json.RawMessage(
					`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"city"}}`)}
				toolEv = sse.Event{Name: "content_block_delta", Data: json.RawMessage(
					`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"sf\"}"}}`)}
			case ProtocolChat:
				contentEv = sse.Event{Data: json.RawMessage(
					`{"choices":[{"delta":{"content":"city"}}]}`)}
				toolEv = sse.Event{Data: json.RawMessage(
					`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"sf\"}"}}]}}]}`)}
			case ProtocolResponses:
				contentEv = sse.Event{Name: "response.output_text.delta", Data: json.RawMessage(
					`{"type":"response.output_text.delta","delta":"city"}`)}
				toolEv = sse.Event{Name: "response.function_call_arguments.delta", Data: json.RawMessage(
					`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"city\":\"sf\"}"}`)}
			}

			// Content event is buffered (length 0).
			out := d.Process(contentEv)
			if len(out) != 0 {
				t.Fatalf("content event buffered: out len = %d", len(out))
			}
			// Tool-use event passes through untouched WITHOUT
			// flushing the buffered content delta — tool-use blocks
			// have their own indices and the text content buffer
			// keeps waiting for its own block stop / EOF.
			out = d.Process(toolEv)
			if len(out) != 1 {
				t.Fatalf("tool event: out len = %d (want 1: tool-use passes through, content still buffered)", len(out))
			}
			if string(out[0].Data) != string(toolEv.Data) {
				t.Errorf("tool-use event mutated! before:\n%s\nafter:\n%s",
					toolEv.Data, out[0].Data)
			}
			if strings.Contains(string(out[0].Data), "CITY-REPLACED") {
				t.Errorf("tool-use event leaked into content transform: %s", out[0].Data)
			}
			// Flush the buffer; the content delta surfaces mutated.
			flushed := d.FlushBeforeFinish()
			if len(flushed) != 1 {
				t.Fatalf("flushed content out = %d, want 1", len(flushed))
			}
			if !strings.Contains(string(flushed[0].Data), "CITY-REPLACED") {
				t.Errorf("flushed content delta missing mutation: %s", flushed[0].Data)
			}
		})
	}
}

// ----- Dispatcher content mutation paths --------------------------
//
// With the R1 lookahead model, a content delta is buffered until the
// next event arrives or the dispatcher is flushed. Each test below
// drives Process + FlushBeforeFinish (or Process with a follow-up
// event) so the buffered delta surfaces.

func TestStreamDispatcher_ContentDeltaMutation(t *testing.T) {
	ac := &AfterContext{}
	ac.OnContentDelta(func(s string) string {
		return strings.ToUpper(s)
	})
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	ev := sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)}
	d.Process(ev) // buffered
	out := d.FlushBeforeFinish()
	if len(out) != 1 {
		t.Fatalf("flushed len = %d", len(out))
	}
	if !strings.Contains(string(out[0].Data), `"HELLO"`) {
		t.Errorf("mutation not applied: %s", out[0].Data)
	}
}

func TestStreamDispatcher_ContentDeltaIdentityShortCircuit(t *testing.T) {
	// An identity OnContentDelta transform must produce the original
	// event bytes (not a re-marshal that might lose field ordering).
	ac := &AfterContext{}
	ac.OnContentDelta(func(s string) string { return s })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	ev := sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi","custom_field":"keep"}}`)}
	d.Process(ev)
	out := d.FlushBeforeFinish()
	if len(out) != 1 {
		t.Fatalf("flushed len = %d", len(out))
	}
	if string(out[0].Data) != string(ev.Data) {
		t.Errorf("identity transform should pass through bytes verbatim; lost %q", out[0].Data)
	}
}

func TestStreamDispatcher_ReasoningDeltaMutation(t *testing.T) {
	// Reasoning deltas are NOT subject to the lookahead — they pass
	// through Process immediately (the v1 contract has no
	// OnLastReasoningDelta hook).
	ac := &AfterContext{}
	ac.OnReasoningDelta(func(s string) string { return "[r]" + s })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	ev := sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`)}
	out := d.Process(ev)
	if len(out) != 1 {
		t.Fatalf("reasoning out len = %d", len(out))
	}
	if !strings.Contains(string(out[0].Data), `"[r]hmm"`) {
		t.Errorf("reasoning mutation not applied: %s", out[0].Data)
	}
}

func TestStreamDispatcher_MultipleTransformsInOrder(t *testing.T) {
	ac := &AfterContext{}
	ac.OnContentDelta(func(s string) string { return s + "-a" })
	ac.OnContentDelta(func(s string) string { return s + "-b" })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	ev := sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`)}
	d.Process(ev)
	out := d.FlushBeforeFinish()
	if len(out) != 1 {
		t.Fatalf("flushed len = %d", len(out))
	}
	if !strings.Contains(string(out[0].Data), `"x-a-b"`) {
		t.Errorf("expected ordered chain output; got %s", out[0].Data)
	}
}

// ----- Terminal + last-delta semantics ----------------------------

func TestStreamDispatcher_TerminalPassesThroughUnchanged(t *testing.T) {
	// The terminal event flows untouched; any registered (but
	// deprecated) EmitBeforeFinish callbacks are NOT invoked.
	ac := &AfterContext{}
	ac.EmitBeforeFinish(func(emit func(text string)) { emit("should not appear here") })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	terminal := sse.Event{Name: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)}
	out := d.Process(terminal)
	if len(out) != 1 {
		t.Fatalf("Process(terminal) should yield 1 event, got %d", len(out))
	}
	if string(out[0].Data) != string(terminal.Data) {
		t.Errorf("terminal mutated by Process: %s", out[0].Data)
	}
	// Deprecated EmitBeforeFinish must NOT synthesize new events.
	flushed := d.FlushBeforeFinish()
	if len(flushed) != 0 {
		t.Errorf("deprecated EmitBeforeFinish callbacks should not fire on flush; got %d events", len(flushed))
	}
}

// TestStreamDispatcher_LastTextDeltaFiresOnContentBlockStop is the
// load-bearing R1 invariant: the OnLastTextDelta hook runs on the
// buffered last delta when content_block_stop arrives (Anthropic) or
// response.output_text.done arrives (Responses). The wire sequence
// must end with the mutated delta, then the per-block stop — never a
// synthesized event AFTER the stop.
func TestStreamDispatcher_LastTextDeltaFiresOnContentBlockStop_Messages(t *testing.T) {
	ac := &AfterContext{}
	ac.OnLastTextDelta(func(idx int, text string) string {
		if idx != 0 {
			t.Errorf("expected block index 0, got %d", idx)
		}
		return text + "FOOTER"
	})
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}

	d.Process(sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)})
	stop := sse.Event{Name: "content_block_stop", Data: json.RawMessage(
		`{"type":"content_block_stop","index":0}`)}
	out := d.Process(stop)
	if len(out) != 2 {
		t.Fatalf("Process(stop) out = %d, want 2 (mutated delta + stop)", len(out))
	}
	if !strings.Contains(string(out[0].Data), "hiFOOTER") {
		t.Errorf("last delta not mutated: %s", out[0].Data)
	}
	if out[1].Name != "content_block_stop" {
		t.Errorf("stop event not preserved: %+v", out[1])
	}
}

func TestStreamDispatcher_LastTextDeltaFiresOnContentBlockStop_Responses(t *testing.T) {
	ac := &AfterContext{}
	ac.OnLastTextDelta(func(_ int, text string) string {
		return text + "FOOTER"
	})
	d := &StreamDispatcher{Protocol: ProtocolResponses, After: ac}

	d.Process(sse.Event{Name: "response.output_text.delta", Data: json.RawMessage(
		`{"type":"response.output_text.delta","delta":"hi"}`)})
	done := sse.Event{Name: "response.output_text.done", Data: json.RawMessage(
		`{"type":"response.output_text.done"}`)}
	out := d.Process(done)
	if len(out) != 2 {
		t.Fatalf("Process(done) out = %d, want 2 (mutated delta + done)", len(out))
	}
	if !strings.Contains(string(out[0].Data), "hiFOOTER") {
		t.Errorf("last delta not mutated: %s", out[0].Data)
	}
	if out[1].Name != "response.output_text.done" {
		t.Errorf("done event not preserved: %+v", out[1])
	}
}

func TestStreamDispatcher_LastTextDeltaFiresOnEOFForChat(t *testing.T) {
	// Chat has no per-block stop in the wire. EOF flush handles it.
	ac := &AfterContext{}
	ac.OnLastTextDelta(func(_ int, text string) string {
		return text + "FOOTER"
	})
	d := &StreamDispatcher{Protocol: ProtocolChat, After: ac}

	d.Process(sse.Event{Data: json.RawMessage(
		`{"choices":[{"delta":{"content":"hi"}}]}`)})
	out := d.FlushBeforeFinish()
	if len(out) != 1 {
		t.Fatalf("flush out = %d, want 1", len(out))
	}
	if !strings.Contains(string(out[0].Data), "hiFOOTER") {
		t.Errorf("Chat last delta not mutated on EOF: %s", out[0].Data)
	}
}

// TestStreamDispatcher_MessagesWireSequence is the load-bearing
// invariant for the Anthropic Messages protocol: a typical stream
// produces ..., text_delta(text+suffix), content_block_stop, ...,
// message_stop, with NO synthesized event after message_stop.
func TestStreamDispatcher_MessagesWireSequence(t *testing.T) {
	ac := &AfterContext{}
	ac.OnLastTextDelta(func(_ int, text string) string { return text + "_FOOTER" })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}

	stream := []sse.Event{
		{Name: "message_start", Data: json.RawMessage(`{"type":"message_start"}`)},
		{Name: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi "}}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}`)},
		{Name: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":0}`)},
		{Name: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)},
		{Name: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
	}

	in := make(chan sse.Event, len(stream))
	for _, ev := range stream {
		in <- ev
	}
	close(in)

	out := d.WrapStream(in)
	var got []sse.Event
	for ev := range out {
		got = append(got, ev)
	}

	// Last event MUST be message_stop — no events after.
	if len(got) == 0 || got[len(got)-1].Name != "message_stop" {
		t.Fatalf("last event should be message_stop; got sequence: %s",
			eventNames(got))
	}
	// Find the mutated last text delta — should appear before
	// content_block_stop, payload must contain "there_FOOTER".
	stopIdx := -1
	for i, ev := range got {
		if ev.Name == "content_block_stop" {
			stopIdx = i
			break
		}
	}
	if stopIdx <= 0 {
		t.Fatalf("content_block_stop not found in sequence: %s", eventNames(got))
	}
	lastDelta := got[stopIdx-1]
	if !strings.Contains(string(lastDelta.Data), "there_FOOTER") {
		t.Errorf("last delta before stop should carry suffix; got %s", lastDelta.Data)
	}
}

// TestStreamDispatcher_MessagesWireSequence_PingPreservesBuffer
// guards against an Anthropic-specific quirk: ping events interleave
// with content_block_delta events during long streams. A ping arriving
// between the last text delta and content_block_stop must NOT cause
// the pending delta to flush un-suffixed.
func TestStreamDispatcher_MessagesWireSequence_PingPreservesBuffer(t *testing.T) {
	ac := &AfterContext{}
	ac.OnLastTextDelta(func(_ int, text string) string { return text + "_FOOTER" })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}

	stream := []sse.Event{
		{Name: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi "}}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}`)},
		{Name: "ping", Data: json.RawMessage(`{"type":"ping"}`)},
		{Name: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":0}`)},
		{Name: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
	}
	in := make(chan sse.Event, len(stream))
	for _, ev := range stream {
		in <- ev
	}
	close(in)

	out := d.WrapStream(in)
	var got []sse.Event
	for ev := range out {
		got = append(got, ev)
	}
	if len(got) == 0 || got[len(got)-1].Name != "message_stop" {
		t.Fatalf("last event should be message_stop; got sequence: %s", eventNames(got))
	}
	stopIdx := -1
	for i, ev := range got {
		if ev.Name == "content_block_stop" {
			stopIdx = i
			break
		}
	}
	if stopIdx <= 0 {
		t.Fatalf("content_block_stop not found: %s", eventNames(got))
	}
	lastDelta := got[stopIdx-1]
	if lastDelta.Name != "content_block_delta" {
		t.Fatalf("expected delta immediately before stop; got %+v\nfull: %s", lastDelta, eventNames(got))
	}
	if !strings.Contains(string(lastDelta.Data), "there_FOOTER") {
		t.Errorf("ping flushed buffer un-suffixed; last delta = %s", lastDelta.Data)
	}
}

// TestStreamDispatcher_ResponsesWireSequence asserts the OpenAI
// Responses protocol mirror: ...output_text.delta+suffix,
// response.output_text.done, ..., response.completed (no events after
// response.completed).
func TestStreamDispatcher_ResponsesWireSequence(t *testing.T) {
	ac := &AfterContext{}
	ac.OnLastTextDelta(func(_ int, text string) string { return text + "_FOOTER" })
	d := &StreamDispatcher{Protocol: ProtocolResponses, After: ac}

	stream := []sse.Event{
		{Name: "response.created", Data: json.RawMessage(`{"type":"response.created"}`)},
		{Name: "response.output_text.delta", Data: json.RawMessage(`{"type":"response.output_text.delta","delta":"hi "}`)},
		{Name: "response.output_text.delta", Data: json.RawMessage(`{"type":"response.output_text.delta","delta":"there"}`)},
		{Name: "response.output_text.done", Data: json.RawMessage(`{"type":"response.output_text.done"}`)},
		{Name: "response.completed", Data: json.RawMessage(`{"type":"response.completed"}`)},
	}

	in := make(chan sse.Event, len(stream))
	for _, ev := range stream {
		in <- ev
	}
	close(in)

	out := d.WrapStream(in)
	var got []sse.Event
	for ev := range out {
		got = append(got, ev)
	}

	if len(got) == 0 || got[len(got)-1].Name != "response.completed" {
		t.Fatalf("last event should be response.completed; got sequence: %s",
			eventNames(got))
	}
	doneIdx := -1
	for i, ev := range got {
		if ev.Name == "response.output_text.done" {
			doneIdx = i
			break
		}
	}
	if doneIdx <= 0 {
		t.Fatalf("response.output_text.done not found in sequence: %s", eventNames(got))
	}
	lastDelta := got[doneIdx-1]
	if !strings.Contains(string(lastDelta.Data), "there_FOOTER") {
		t.Errorf("last delta before done should carry suffix; got %s", lastDelta.Data)
	}
}

func eventNames(evs []sse.Event) string {
	var sb strings.Builder
	for i, e := range evs {
		if i > 0 {
			sb.WriteByte(',')
		}
		if e.Name != "" {
			sb.WriteString(e.Name)
		} else {
			sb.WriteString("(data)")
		}
	}
	return sb.String()
}

// ----- EmitBeforeFinish deprecation guard -------------------------
//
// The R1 amendment deprecated EmitBeforeFinish: the framework MUST NOT
// invoke registered callbacks. New code uses OnLastTextDelta.

func TestEmitBeforeFinish_DeprecatedNoLongerFires(t *testing.T) {
	called := false
	ac := &AfterContext{}
	ac.EmitBeforeFinish(func(emit func(text string)) {
		called = true
		emit("synthesized")
	})
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	out := d.FlushBeforeFinish()
	if called {
		t.Errorf("deprecated EmitBeforeFinish callback should NOT be invoked")
	}
	if len(out) != 0 {
		t.Errorf("FlushBeforeFinish should yield 0 events when only EmitBeforeFinish is registered; got %d", len(out))
	}
}

// ----- WrapStream channel form -----------------------------------

func TestWrapStream_OnLastTextDeltaOnEOF(t *testing.T) {
	ac := &AfterContext{}
	ac.OnContentDelta(func(s string) string { return strings.ToUpper(s) })
	ac.OnLastTextDelta(func(_ int, text string) string { return text + "footer" })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}

	in := make(chan sse.Event, 4)
	in <- sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)}
	close(in)

	out := d.WrapStream(in)
	var got []sse.Event
	for ev := range out {
		got = append(got, ev)
	}
	// With no terminator in the stream, the EOF flush mutates the
	// buffered delta with OnContentDelta (HI) + OnLastTextDelta
	// (append "footer"). Final wire text should be HIfooter.
	if len(got) != 1 {
		t.Fatalf("WrapStream EOF out len = %d, want 1", len(got))
	}
	if !strings.Contains(string(got[0].Data), `"HIfooter"`) {
		t.Errorf("EOF-flushed delta = %s", got[0].Data)
	}
}

func TestWrapStream_ChatLastDeltaOnEOF(t *testing.T) {
	ac := &AfterContext{}
	ac.OnLastTextDelta(func(_ int, text string) string { return text + " chat-footer" })
	d := &StreamDispatcher{Protocol: ProtocolChat, After: ac}

	in := make(chan sse.Event, 2)
	in <- sse.Event{Data: json.RawMessage(`{"choices":[{"delta":{"content":"hi"}}]}`)}
	close(in)

	out := d.WrapStream(in)
	var got []sse.Event
	for ev := range out {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("Chat wrap events = %d, want 1", len(got))
	}
	if !strings.Contains(string(got[0].Data), "chat-footer") {
		t.Errorf("Chat footer not appended on EOF: %s", got[0].Data)
	}
}

func TestWrapStream_NoFlushWhenNoCallbacks(t *testing.T) {
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: &AfterContext{}}
	in := make(chan sse.Event, 1)
	in <- sse.Event{Name: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)}
	close(in)
	out := d.WrapStream(in)
	count := 0
	for range out {
		count++
	}
	if count != 1 {
		t.Errorf("expected only terminal, got %d events", count)
	}
}

// ----- Synthesize per protocol ------------------------------------
//
// SynthesizeContentDelta is retained as a helper for callers that
// need to build content-delta events outside the dispatcher (it is no
// longer invoked by the framework). The shape assertions guard against
// drift in the per-protocol wire builders.

func TestSynthesizeContentDelta_PerProtocol(t *testing.T) {
	cases := []struct {
		proto Protocol
		want  string // substring assertion
	}{
		{ProtocolMessages, `"text_delta"`},
		{ProtocolChat, `"choices"`},
		{ProtocolResponses, `"response.output_text.delta"`},
	}
	for _, tc := range cases {
		t.Run(tc.proto.String(), func(t *testing.T) {
			ev, err := SynthesizeContentDelta(tc.proto, "world")
			if err != nil {
				t.Fatalf("synth: %v", err)
			}
			if !strings.Contains(string(ev.Data), tc.want) {
				t.Errorf("synth shape missing %q: %s", tc.want, ev.Data)
			}
			if !strings.Contains(string(ev.Data), "world") {
				t.Errorf("synth missing payload: %s", ev.Data)
			}
		})
	}
}

func TestSynthesizeContentDelta_GeminiDeferred(t *testing.T) {
	_, err := SynthesizeContentDelta(ProtocolGemini, "x")
	if err == nil {
		t.Errorf("Gemini synth should error (W1 deferred)")
	}
}

func TestSynthesizeContentDelta_EscapesText(t *testing.T) {
	// Synthesized payloads MUST JSON-escape the text. A naive
	// fmt.Sprintf with %q would handle quotes; this guards against a
	// future regression to %s.
	ev, err := SynthesizeContentDelta(ProtocolMessages, `quote " and \backslash`)
	if err != nil {
		t.Fatalf("synth: %v", err)
	}
	var frame struct {
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(ev.Data, &frame); err != nil {
		t.Fatalf("synth data invalid JSON: %v\n%s", err, ev.Data)
	}
	if frame.Delta.Text != `quote " and \backslash` {
		t.Errorf("text not round-tripped: got %q", frame.Delta.Text)
	}
}

// ----- Other events pass through ----------------------------------

func TestStreamDispatcher_OtherClassPassThrough(t *testing.T) {
	ac := &AfterContext{}
	ac.OnContentDelta(func(s string) string { return s + "!" })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	// A start event has no content text; it must not call the transform.
	startEv := sse.Event{Name: "content_block_start", Data: json.RawMessage(
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)}
	out := d.Process(startEv)
	if len(out) != 1 || string(out[0].Data) != string(startEv.Data) {
		t.Errorf("start event mutated: %s", out[0].Data)
	}
}
