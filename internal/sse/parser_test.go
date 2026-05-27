package sse

import (
	"strings"
	"testing"
)

func TestParseOpenAIChatDataOnly(t *testing.T) {
	// Real-shape OpenAI Chat Completions SSE: data-only, terminated by [DONE].
	in := `data: {"id":"abc","choices":[{"delta":{"content":"H"}}]}

data: {"id":"abc","choices":[{"delta":{"content":"i"}}]}

data: [DONE]

`
	r := Parse(strings.NewReader(in))
	if !r.StreamDone {
		t.Errorf("StreamDone = false, want true")
	}
	if len(r.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(r.Events))
	}
	for i, e := range r.Events {
		if e.Name != "" {
			t.Errorf("event[%d].Name = %q, want empty (data-only)", i, e.Name)
		}
	}
	if r.ParseError != "" {
		t.Errorf("ParseError = %q, want empty", r.ParseError)
	}
}

func TestParseAnthropicMessages(t *testing.T) {
	// Real-shape Anthropic Messages SSE: event-named, terminator message_stop.
	in := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1"}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"text":"Hello"}}

event: message_stop
data: {"type":"message_stop"}

`
	r := Parse(strings.NewReader(in))
	if !r.StreamDone {
		t.Errorf("StreamDone = false, want true (message_stop)")
	}
	if len(r.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(r.Events))
	}
	wantNames := []string{"message_start", "content_block_delta", "message_stop"}
	for i, want := range wantNames {
		if r.Events[i].Name != want {
			t.Errorf("event[%d].Name = %q, want %q", i, r.Events[i].Name, want)
		}
	}
}

func TestParseOpenAIResponses(t *testing.T) {
	// Real-shape OpenAI Responses API SSE: event-named, terminator response.completed.
	in := `event: response.created
data: {"type":"response.created","response":{"id":"resp_1"}}

event: response.output_text.delta
data: {"delta":"H"}

event: response.completed
data: {"type":"response.completed","response":{"usage":{}}}

`
	r := Parse(strings.NewReader(in))
	if !r.StreamDone {
		t.Errorf("StreamDone = false, want true (response.completed)")
	}
	if len(r.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(r.Events))
	}
}

func TestParseEOFWithoutTerminatorFlushesPending(t *testing.T) {
	// Mid-stream cut: no terminator, no trailing blank line. The pending
	// frame should still be emitted; StreamDone should be false.
	in := `event: message_start
data: {"id":"msg_1"}

event: content_block_delta
data: {"delta":"H"}`
	r := Parse(strings.NewReader(in))
	if r.StreamDone {
		t.Errorf("StreamDone = true, want false (cut mid-stream)")
	}
	if len(r.Events) != 2 {
		t.Fatalf("events = %d, want 2 (pending must flush)", len(r.Events))
	}
}

func TestParseMultiLineData(t *testing.T) {
	// Per SSE spec, multiple `data:` lines in one frame join with newline.
	in := `data: line1
data: line2

`
	r := Parse(strings.NewReader(in))
	if len(r.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(r.Events))
	}
	// data was "line1\nline2" — not valid JSON, so it gets wrapped as a string.
	got := string(r.Events[0].Data)
	if got != `"line1\nline2"` {
		t.Errorf("Data = %s, want %q", got, `"line1\nline2"`)
	}
}

func TestParseIgnoresIdAndRetry(t *testing.T) {
	// SSE-spec fields we explicitly don't preserve in v0 (ARCHITECTURE § 6.4).
	in := `id: 1
retry: 5000
event: ping
data: {"ok":true}

`
	r := Parse(strings.NewReader(in))
	if len(r.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(r.Events))
	}
	if r.Events[0].Name != "ping" {
		t.Errorf("name = %q, want ping", r.Events[0].Name)
	}
}

func TestParseCommentLinesIgnored(t *testing.T) {
	in := `: keep-alive comment
data: {"v":1}

: another comment
data: {"v":2}

`
	r := Parse(strings.NewReader(in))
	if len(r.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(r.Events))
	}
}

func TestParseNonSSEFallback(t *testing.T) {
	// Random bytes that don't match the SSE shape.
	in := `<html><body>not SSE</body></html>`
	r := Parse(strings.NewReader(in))
	if r.ParseError == "" {
		t.Errorf("ParseError empty, want non-empty for non-SSE input")
	}
	if len(r.Events) != 0 {
		t.Errorf("events = %d, want 0 for non-SSE input", len(r.Events))
	}
}

func TestParseChatStreamWithoutDONESentinel(t *testing.T) {
	// Some upstreams cut without [DONE]. StreamDone false, events captured.
	in := `data: {"id":"x","choices":[{"delta":{"content":"H"}}]}

data: {"id":"x","choices":[{"delta":{"content":"i"}}]}

`
	r := Parse(strings.NewReader(in))
	if r.StreamDone {
		t.Errorf("StreamDone = true, want false (no [DONE])")
	}
	if len(r.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(r.Events))
	}
}

func TestParseEmptyInput(t *testing.T) {
	r := Parse(strings.NewReader(""))
	if len(r.Events) != 0 || r.StreamDone || r.ParseError != "" {
		t.Errorf("empty input: got %+v", r)
	}
}

func TestParseLargeFrame(t *testing.T) {
	// A frame whose data: line is large (~512 KB). Must not blow the
	// scanner buffer.
	big := strings.Repeat("a", 512*1024)
	in := "data: \"" + big + "\"\n\ndata: [DONE]\n\n"
	r := Parse(strings.NewReader(in))
	if !r.StreamDone {
		t.Errorf("StreamDone = false (large frame followed by DONE)")
	}
	if len(r.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(r.Events))
	}
}
