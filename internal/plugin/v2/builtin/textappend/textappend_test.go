package textappend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	v2 "github.com/leoyun/api-log/internal/plugin/v2"
	"github.com/leoyun/api-log/internal/sse"
)

func mkReq(path string, msgs []v2.Message, system string) *v2.ParsedRequest {
	return &v2.ParsedRequest{
		Path:         path,
		Messages:     msgs,
		SystemPrompt: system,
	}
}

func textTurn(role, text string) v2.Message {
	return v2.Message{
		Role:    role,
		Content: []v2.ContentPart{{Type: "text", Text: text}},
	}
}

// ----- config validation ---------------------------------------------

func TestNew_AppliesTargetDefaults(t *testing.T) {
	p, err := New(map[string]any{
		"up":   map[string]any{"suffix": "x"},
		"down": map[string]any{"suffix": "y"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if p.cfg.Up.Target != TargetLastUserMessage {
		t.Errorf("up.target default = %q", p.cfg.Up.Target)
	}
	if p.cfg.Down.Target != TargetContent {
		t.Errorf("down.target default = %q", p.cfg.Down.Target)
	}
}

func TestNew_RejectsUnknownTargets(t *testing.T) {
	if _, err := New(map[string]any{
		"up": map[string]any{"suffix": "x", "target": "bogus"},
	}); err == nil {
		t.Errorf("unknown up.target should error")
	}
	if _, err := New(map[string]any{
		"down": map[string]any{"suffix": "x", "target": "nowhere"},
	}); err == nil {
		t.Errorf("unknown down.target should error")
	}
}

func TestNew_NilCfgIsValidNoop(t *testing.T) {
	p, err := New(nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res := p.OnBefore(context.Background(), mkReq("/v1/messages", []v2.Message{textTurn("user", "hi")}, ""), nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("nil cfg should be a no-op")
	}
}

// ----- BEFORE: non-match route ---------------------------------------

func TestOnBefore_NonMatchRoute(t *testing.T) {
	p, err := New(map[string]any{
		"routes": []any{"/v1/chat/completions"},
		"up":     map[string]any{"suffix": "\n\n谢谢你"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := mkReq("/v1/messages", []v2.Message{textTurn("user", "hello")}, "")
	res := p.OnBefore(context.Background(), req, nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("non-match route should Continue, got %v", res.Action)
	}
	if req.Messages[0].Content[0].Text != "hello" {
		t.Errorf("input request mutated: %q", req.Messages[0].Content[0].Text)
	}
}

// ----- BEFORE: empty rules -------------------------------------------

func TestOnBefore_EmptyUpSuffix(t *testing.T) {
	p, err := New(map[string]any{"routes": []any{"/v1/*"}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res := p.OnBefore(context.Background(), mkReq("/v1/messages", []v2.Message{textTurn("user", "x")}, ""), nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("empty Up.Suffix should Continue, got %v", res.Action)
	}
}

// ----- BEFORE: single rule matching ----------------------------------

// TestOnBefore_LastUserMessage: the suffix lands on the trailing user
// turn even when there are assistant turns interleaved.
func TestOnBefore_LastUserMessage(t *testing.T) {
	p, err := New(map[string]any{
		"up": map[string]any{"suffix": "\n\n谢谢你", "target": TargetLastUserMessage},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := mkReq("/v1/messages", []v2.Message{
		textTurn("user", "first ask"),
		textTurn("assistant", "first answer"),
		textTurn("user", "second ask"),
	}, "")
	res := p.OnBefore(context.Background(), req, nil)
	if res.Action != v2.ActionMutate {
		t.Fatalf("Action = %v, want Mutate", res.Action)
	}
	got := res.Mutated.Messages[2].Content[0].Text
	if got != "second ask\n\n谢谢你" {
		t.Errorf("trailing user turn text = %q", got)
	}
	// Untouched turns must remain.
	if res.Mutated.Messages[0].Content[0].Text != "first ask" {
		t.Errorf("earlier user turn mutated: %q", res.Mutated.Messages[0].Content[0].Text)
	}
	if res.Mutated.Messages[1].Content[0].Text != "first answer" {
		t.Errorf("assistant turn mutated: %q", res.Mutated.Messages[1].Content[0].Text)
	}
	// Original request must not be touched (deep-enough copy invariant).
	if req.Messages[2].Content[0].Text != "second ask" {
		t.Errorf("input request mutated in place: %q", req.Messages[2].Content[0].Text)
	}
}

func TestOnBefore_SystemPromptTarget(t *testing.T) {
	p, err := New(map[string]any{
		"up": map[string]any{"suffix": " | footer", "target": TargetSystemPrompt},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := mkReq("/v1/messages", []v2.Message{textTurn("user", "x")}, "be terse")
	res := p.OnBefore(context.Background(), req, nil)
	if res.Action != v2.ActionMutate {
		t.Fatalf("Action = %v, want Mutate", res.Action)
	}
	if res.Mutated.SystemPrompt != "be terse | footer" {
		t.Errorf("system = %q", res.Mutated.SystemPrompt)
	}
	if req.SystemPrompt != "be terse" {
		t.Errorf("original system mutated in place: %q", req.SystemPrompt)
	}
}

func TestOnBefore_LastUserMessage_NoUserTurnContinues(t *testing.T) {
	// A request shape with no user-role text part (e.g. tool-only)
	// should not produce a no-op Mutate that just re-wraps the original.
	p, err := New(map[string]any{
		"up": map[string]any{"suffix": "x", "target": TargetLastUserMessage},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := mkReq("/v1/messages", []v2.Message{
		{Role: "tool", Name: "lookup", Content: []v2.ContentPart{
			{Type: "tool_result", ToolResult: &v2.ToolResult{Content: "42"}},
		}},
	}, "")
	res := p.OnBefore(context.Background(), req, nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("no user turn should Continue, got %v", res.Action)
	}
}

// ----- AFTER non-streaming -------------------------------------------

func TestOnAfter_NonStreaming_Content(t *testing.T) {
	p, err := New(map[string]any{
		"down": map[string]any{"suffix": "\n\n谢谢", "target": TargetContent},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ac := &v2.AfterContext{Response: &v2.ParsedResponse{Content: "you are welcome"}}
	res := p.OnAfter(context.Background(), &v2.ParsedRequest{Path: "/v1/messages"}, ac, nil)
	if res.Action != v2.ActionMutate {
		t.Fatalf("Action = %v, want Mutate", res.Action)
	}
	if res.Mutated.Content != "you are welcome\n\n谢谢" {
		t.Errorf("content = %q", res.Mutated.Content)
	}
	if ac.Response.Content != "you are welcome" {
		t.Errorf("input response mutated in place: %q", ac.Response.Content)
	}
}

func TestOnAfter_NonStreaming_Reasoning(t *testing.T) {
	p, err := New(map[string]any{
		"down": map[string]any{"suffix": " [end]", "target": TargetReasoning},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ac := &v2.AfterContext{Response: &v2.ParsedResponse{Reasoning: "thinking", Content: "ans"}}
	res := p.OnAfter(context.Background(), &v2.ParsedRequest{Path: "/v1/messages"}, ac, nil)
	if res.Action != v2.ActionMutate {
		t.Fatalf("Action = %v, want Mutate", res.Action)
	}
	if res.Mutated.Reasoning != "thinking [end]" {
		t.Errorf("reasoning = %q", res.Mutated.Reasoning)
	}
	if res.Mutated.Content != "ans" {
		t.Errorf("content should not change when target=reasoning: %q", res.Mutated.Content)
	}
}

func TestOnAfter_NonStreaming_NonMatchRoute(t *testing.T) {
	p, err := New(map[string]any{
		"routes": []any{"/v1/chat/completions"},
		"down":   map[string]any{"suffix": "x"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ac := &v2.AfterContext{Response: &v2.ParsedResponse{Content: "hi"}}
	res := p.OnAfter(context.Background(), &v2.ParsedRequest{Path: "/v1/messages"}, ac, nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("non-match route should Continue, got %v", res.Action)
	}
}

func TestOnAfter_NonStreaming_EmptySuffix(t *testing.T) {
	p, err := New(nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ac := &v2.AfterContext{Response: &v2.ParsedResponse{Content: "hi"}}
	res := p.OnAfter(context.Background(), &v2.ParsedRequest{Path: "/v1/messages"}, ac, nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("empty Down.Suffix should Continue, got %v", res.Action)
	}
}

// ----- AFTER streaming -----------------------------------------------

// TestOnAfter_Streaming_ContentSuffixEmitsViaFlush: the W1
// EmitBeforeFinish callback must produce one synthesized content delta
// carrying the suffix. We assert on FlushBeforeFinish (the synchronous
// entry point) so the test does not depend on goroutine scheduling.
func TestOnAfter_Streaming_ContentSuffixEmitsViaFlush(t *testing.T) {
	p, err := New(map[string]any{
		"down": map[string]any{"suffix": "\n\nthanks", "target": TargetContent},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := &v2.ParsedRequest{Path: "/v1/messages", Protocol: v2.ProtocolMessages, Streaming: true}
	ac := &v2.AfterContext{}
	res := p.OnAfter(context.Background(), req, ac, nil)
	if res.Action != v2.ActionContinue {
		t.Fatalf("streaming OnAfter should Continue and register; got %v", res.Action)
	}
	d := &v2.StreamDispatcher{Protocol: v2.ProtocolMessages, After: ac}
	flushed := d.FlushBeforeFinish()
	if len(flushed) != 1 {
		t.Fatalf("expected 1 synth event, got %d", len(flushed))
	}
	if !strings.Contains(string(flushed[0].Data), "thanks") {
		t.Errorf("synth event missing suffix payload: %s", flushed[0].Data)
	}
	// The synthesized event must use the protocol's text-delta shape so
	// the W1 stream dispatcher does NOT mis-classify it as something
	// else when re-read.
	classified := v2.Classify(v2.ProtocolMessages, flushed[0])
	if classified.Class != v2.ClassContentDelta {
		t.Errorf("synth event class = %v, want ClassContentDelta", classified.Class)
	}
	if !strings.Contains(classified.Text, "thanks") {
		t.Errorf("synth event payload = %q", classified.Text)
	}
}

// TestOnAfter_Streaming_ToolUseUntouched: the only thing this plugin
// registers in streaming mode is an EmitBeforeFinish callback — the W1
// dispatcher will not let it touch per-event payloads at all, but we
// still assert that a tool-use event flowing through the dispatcher
// (with the plugin's registration active) passes through byte-identical.
func TestOnAfter_Streaming_ToolUseUntouched(t *testing.T) {
	p, err := New(map[string]any{
		"down": map[string]any{"suffix": "city", "target": TargetContent},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := &v2.ParsedRequest{Path: "/v1/messages", Protocol: v2.ProtocolMessages, Streaming: true}
	ac := &v2.AfterContext{}
	p.OnAfter(context.Background(), req, ac, nil)
	d := &v2.StreamDispatcher{Protocol: v2.ProtocolMessages, After: ac}
	tool := sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"sf\"}"}}`)}
	out := d.Process(tool)
	if len(out) != 1 {
		t.Fatalf("out len = %d", len(out))
	}
	if string(out[0].Data) != string(tool.Data) {
		t.Errorf("tool_use event mutated\nbefore: %s\nafter:  %s", tool.Data, out[0].Data)
	}
}

func TestOnAfter_Streaming_NonMatchRoute_NoRegistration(t *testing.T) {
	p, err := New(map[string]any{
		"routes": []any{"/v1/chat/completions"},
		"down":   map[string]any{"suffix": "x"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ac := &v2.AfterContext{}
	p.OnAfter(context.Background(), &v2.ParsedRequest{Path: "/v1/messages"}, ac, nil)
	if got := ac.BeforeFinishCallbacks(); len(got) != 0 {
		t.Errorf("non-match route should not register; got %d callbacks", len(got))
	}
}

func TestOnAfter_Streaming_ReasoningTargetSkipped(t *testing.T) {
	// W1 has no reasoning-delta synthesizer; the plugin documents this
	// as a v1 limitation by registering NOTHING in this branch.
	p, err := New(map[string]any{
		"down": map[string]any{"suffix": "x", "target": TargetReasoning},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ac := &v2.AfterContext{}
	res := p.OnAfter(context.Background(), &v2.ParsedRequest{Path: "/v1/messages"}, ac, nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("reasoning-target streaming should Continue, got %v", res.Action)
	}
	if got := ac.BeforeFinishCallbacks(); len(got) != 0 {
		t.Errorf("reasoning-target streaming should register nothing; got %d", len(got))
	}
}

// ----- ConfigSchema --------------------------------------------------

func TestConfigSchema_HasEnumTargets(t *testing.T) {
	p, err := New(nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	schema := p.ConfigSchema()
	foundUpTarget, foundDownTarget := false, false
	for _, f := range schema.Fields {
		if f.Name == "up.target" {
			foundUpTarget = true
			if f.Type != "enum" || len(f.Enum) != 2 {
				t.Errorf("up.target schema = %+v", f)
			}
		}
		if f.Name == "down.target" {
			foundDownTarget = true
			if f.Type != "enum" || len(f.Enum) != 2 {
				t.Errorf("down.target schema = %+v", f)
			}
		}
	}
	if !foundUpTarget || !foundDownTarget {
		t.Errorf("schema missing target enums: up=%v down=%v", foundUpTarget, foundDownTarget)
	}
}

// ----- Registration sanity -------------------------------------------

func TestRegisteredCtor_BuildsViaLookup(t *testing.T) {
	ctor, ok := v2.LookupBuiltin(Type)
	if !ok {
		t.Fatalf("init() did not register %q", Type)
	}
	built, err := ctor(map[string]any{
		"up":   map[string]any{"suffix": "x"},
		"down": map[string]any{"suffix": "y"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if _, ok := built.(v2.BeforePlugin); !ok {
		t.Errorf("built value should implement BeforePlugin")
	}
	if _, ok := built.(v2.AfterPlugin); !ok {
		t.Errorf("built value should implement AfterPlugin")
	}
}
