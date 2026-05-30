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
		name  string
		ev    sse.Event
		want  EventClass
		text  string
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
// This is the spec §10.6 invariant: tool_use input_json_delta events
// MUST NOT pass through content transforms even when the match string
// sits inside the tool-call argument JSON. The test registers a
// transform that would corrupt any string containing "city", then
// runs a stream where the substring appears in BOTH a text delta
// (mutation expected) AND a tool-call args delta (must pass through).

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

			// Content event SHOULD be mutated.
			out := d.Process(contentEv)
			if len(out) != 1 {
				t.Fatalf("content event: out len = %d", len(out))
			}
			if !strings.Contains(string(out[0].Data), "CITY-REPLACED") {
				t.Errorf("content event: expected mutation, got %s", out[0].Data)
			}

			// Tool-use event MUST pass through untouched.
			out = d.Process(toolEv)
			if len(out) != 1 {
				t.Fatalf("tool event: out len = %d", len(out))
			}
			if string(out[0].Data) != string(toolEv.Data) {
				t.Errorf("tool-use event mutated! before:\n%s\nafter:\n%s",
					toolEv.Data, out[0].Data)
			}
			if strings.Contains(string(out[0].Data), "CITY-REPLACED") {
				t.Errorf("tool-use event leaked into content transform: %s", out[0].Data)
			}
		})
	}
}

// ----- Dispatcher content mutation paths --------------------------

func TestStreamDispatcher_ContentDeltaMutation(t *testing.T) {
	ac := &AfterContext{}
	ac.OnContentDelta(func(s string) string {
		return strings.ToUpper(s)
	})
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	ev := sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)}
	out := d.Process(ev)
	if len(out) != 1 {
		t.Fatalf("out len = %d", len(out))
	}
	if !strings.Contains(string(out[0].Data), `"HELLO"`) {
		t.Errorf("mutation not applied: %s", out[0].Data)
	}
}

func TestStreamDispatcher_ContentDeltaIdentityShortCircuit(t *testing.T) {
	// A transform that returns the input unchanged must yield the
	// original event bytes (not a re-marshal that might lose ordering).
	ac := &AfterContext{}
	ac.OnContentDelta(func(s string) string { return s })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	ev := sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi","custom_field":"keep"}}`)}
	out := d.Process(ev)
	if string(out[0].Data) != string(ev.Data) {
		t.Errorf("identity transform should pass through bytes verbatim; lost %q", out[0].Data)
	}
}

func TestStreamDispatcher_ReasoningDeltaMutation(t *testing.T) {
	ac := &AfterContext{}
	ac.OnReasoningDelta(func(s string) string { return "[r]" + s })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	ev := sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`)}
	out := d.Process(ev)
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
	out := d.Process(ev)
	if !strings.Contains(string(out[0].Data), `"x-a-b"`) {
		t.Errorf("expected ordered chain output; got %s", out[0].Data)
	}
}

// ----- EmitBeforeFinish via FlushBeforeFinish ---------------------
//
// EmitBeforeFinish is wired to stream-EOF, not to a protocol-specific
// terminal event — so it works uniformly across Chat (no terminal
// event), Gemini (no terminal sentinel), Messages, and Responses.
// FlushBeforeFinish is the synchronous trigger; WrapStream invokes it
// on input-channel close. Process does NOT trigger it (the terminal
// event passes through unchanged).

func TestStreamDispatcher_TerminalPassesThroughUnchanged(t *testing.T) {
	// Process should never inject synthesized deltas itself; the
	// terminal event flows untouched.
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
}

func TestFlushBeforeFinish_SingleCallback(t *testing.T) {
	ac := &AfterContext{}
	ac.EmitBeforeFinish(func(emit func(text string)) {
		emit("\n\n--footer--")
	})
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	out := d.FlushBeforeFinish()
	if len(out) != 1 {
		t.Fatalf("expected 1 synth event, got %d", len(out))
	}
	if out[0].Name != "content_block_delta" {
		t.Errorf("synth event name = %q", out[0].Name)
	}
	if !strings.Contains(string(out[0].Data), "--footer--") {
		t.Errorf("synth event missing footer: %s", out[0].Data)
	}
}

func TestFlushBeforeFinish_MultipleCallbacksInOrder(t *testing.T) {
	ac := &AfterContext{}
	ac.EmitBeforeFinish(func(emit func(text string)) { emit("first") })
	ac.EmitBeforeFinish(func(emit func(text string)) { emit("second") })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}
	out := d.FlushBeforeFinish()
	if len(out) != 2 {
		t.Fatalf("expected 2 synth events, got %d", len(out))
	}
	if !strings.Contains(string(out[0].Data), "first") {
		t.Errorf("first synth missing: %s", out[0].Data)
	}
	if !strings.Contains(string(out[1].Data), "second") {
		t.Errorf("second synth missing: %s", out[1].Data)
	}
}

// TestFlushBeforeFinish_WorksForChat is the bug guard: Chat has no
// terminal event class, so Process would never have triggered the
// emit hook. The EOF-trigger model fixes this.
func TestFlushBeforeFinish_WorksForChat(t *testing.T) {
	ac := &AfterContext{}
	ac.EmitBeforeFinish(func(emit func(text string)) { emit("via-flush") })
	d := &StreamDispatcher{Protocol: ProtocolChat, After: ac}
	out := d.FlushBeforeFinish()
	if len(out) != 1 {
		t.Fatalf("Chat flush should emit 1 event, got %d", len(out))
	}
	if !strings.Contains(string(out[0].Data), "via-flush") {
		t.Errorf("Chat synth missing payload: %s", out[0].Data)
	}
}

func TestFlushBeforeFinish_GeminiDropsSilently(t *testing.T) {
	// Gemini synth is not pinned down in v1; flush MUST NOT crash,
	// and silently drops the synthesized event(s).
	ac := &AfterContext{}
	ac.EmitBeforeFinish(func(emit func(text string)) { emit("x") })
	d := &StreamDispatcher{Protocol: ProtocolGemini, After: ac}
	if out := d.FlushBeforeFinish(); len(out) != 0 {
		t.Errorf("Gemini flush should yield 0 events; got %d", len(out))
	}
}

// ----- WrapStream channel form -----------------------------------

func TestWrapStream_CallsFlushOnInputClose(t *testing.T) {
	ac := &AfterContext{}
	ac.OnContentDelta(func(s string) string { return strings.ToUpper(s) })
	ac.EmitBeforeFinish(func(emit func(text string)) { emit("\nfooter") })
	d := &StreamDispatcher{Protocol: ProtocolMessages, After: ac}

	in := make(chan sse.Event, 4)
	in <- sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)}
	in <- sse.Event{Name: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)}
	close(in)

	out := d.WrapStream(in)
	var got []sse.Event
	for ev := range out {
		got = append(got, ev)
	}
	// 1 mutated content delta + 1 terminal pass-through + 1 synth flush = 3.
	if len(got) != 3 {
		t.Fatalf("WrapStream out events = %d, want 3", len(got))
	}
	if !strings.Contains(string(got[0].Data), `"HI"`) {
		t.Errorf("content delta should be uppercased: %s", got[0].Data)
	}
	if got[1].Name != "message_stop" {
		t.Errorf("terminal not preserved: %+v", got[1])
	}
	if !strings.Contains(string(got[2].Data), "footer") {
		t.Errorf("flush synth missing: %s", got[2].Data)
	}
}

func TestWrapStream_ChatFlushFiresOnEOF(t *testing.T) {
	// Critical: Chat has no terminal event, so the only way
	// EmitBeforeFinish fires is via channel close.
	ac := &AfterContext{}
	ac.EmitBeforeFinish(func(emit func(text string)) { emit("chat-footer") })
	d := &StreamDispatcher{Protocol: ProtocolChat, After: ac}

	in := make(chan sse.Event, 2)
	in <- sse.Event{Data: json.RawMessage(`{"choices":[{"delta":{"content":"hi"}}]}`)}
	close(in)

	out := d.WrapStream(in)
	var got []sse.Event
	for ev := range out {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("Chat wrap events = %d, want 2", len(got))
	}
	if !strings.Contains(string(got[1].Data), "chat-footer") {
		t.Errorf("Chat footer not emitted on EOF: %s", got[1].Data)
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
	// The resulting JSON must round-trip back to the same text.
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
