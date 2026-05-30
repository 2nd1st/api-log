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

// TestOnAfter_Streaming_ContentSuffixMutatesLastDelta: the R1 design
// appends the suffix to the LAST text_delta of the first content
// block — there is no new event after content_block_stop /
// response.output_text.done. We drive a minimal Messages stream
// through the dispatcher and assert on the wire sequence.
func TestOnAfter_Streaming_ContentSuffixMutatesLastDelta(t *testing.T) {
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
	// The plugin must register ONE OnLastTextDelta callback (not
	// EmitBeforeFinish).
	if cbs := ac.LastTextDeltaCallbacks(); len(cbs) != 1 {
		t.Fatalf("expected 1 OnLastTextDelta registration; got %d", len(cbs))
	}
	if cbs := ac.BeforeFinishCallbacks(); len(cbs) != 0 {
		t.Errorf("plugin should NOT register deprecated EmitBeforeFinish; got %d", len(cbs))
	}

	d := &v2.StreamDispatcher{Protocol: v2.ProtocolMessages, After: ac}
	// Replay a minimal Anthropic stream: one delta + content_block_stop.
	d.Process(sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)})
	out := d.Process(sse.Event{Name: "content_block_stop", Data: json.RawMessage(
		`{"type":"content_block_stop","index":0}`)})
	if len(out) != 2 {
		t.Fatalf("stop should flush + emit stop; out len = %d", len(out))
	}
	if !strings.Contains(string(out[0].Data), "hello\\n\\nthanks") {
		t.Errorf("last delta missing suffix: %s", out[0].Data)
	}
	if out[1].Name != "content_block_stop" {
		t.Errorf("second event should be content_block_stop; got %+v", out[1])
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
	if got := ac.LastTextDeltaCallbacks(); len(got) != 0 {
		t.Errorf("non-match route should not register; got %d callbacks", len(got))
	}
}

func TestOnAfter_Streaming_ReasoningTargetSkipped(t *testing.T) {
	// The dispatcher has no reasoning-last-delta hook; the plugin
	// documents this as a v1 limitation by registering NOTHING in this
	// branch.
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
	if got := ac.LastTextDeltaCallbacks(); len(got) != 0 {
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

// ----- Probability --------------------------------------------------

func TestProbability_NeverFiresWhenZero(t *testing.T) {
	zero := 0.0
	p, err := NewWithRand(map[string]any{
		"up":          map[string]any{"suffix": "x"},
		"probability": zero,
	}, func() float64 { return 0.0 })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := mkReq("/v1/messages", []v2.Message{textTurn("user", "hello")}, "")
	for i := 0; i < 5; i++ {
		res := p.OnBefore(context.Background(), req, nil)
		if res.Action != v2.ActionContinue {
			t.Errorf("probability=0 should never fire (iter %d): %v", i, res.Action)
		}
	}
}

func TestProbability_AlwaysFiresWhenOne(t *testing.T) {
	one := 1.0
	p, err := NewWithRand(map[string]any{
		"up":          map[string]any{"suffix": "x"},
		"probability": one,
	}, func() float64 { return 0.999 })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := mkReq("/v1/messages", []v2.Message{textTurn("user", "hello")}, "")
	res := p.OnBefore(context.Background(), req, nil)
	if res.Action != v2.ActionMutate {
		t.Errorf("probability=1 should always fire; got %v", res.Action)
	}
}

func TestProbability_NilDefaultsToAlwaysFire(t *testing.T) {
	// Absent probability key behaves like 1.0 — operator did not opt
	// into easter-egg mode.
	p, err := NewWithRand(map[string]any{
		"up": map[string]any{"suffix": "x"},
	}, func() float64 { return 0.999 })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if p.cfg.Probability != nil {
		t.Errorf("nil probability config should leave cfg.Probability nil; got %v", *p.cfg.Probability)
	}
	req := mkReq("/v1/messages", []v2.Message{textTurn("user", "hello")}, "")
	res := p.OnBefore(context.Background(), req, nil)
	if res.Action != v2.ActionMutate {
		t.Errorf("nil probability should always fire; got %v", res.Action)
	}
}

func TestProbability_HalfDeterministicSplit(t *testing.T) {
	// With probability=0.5 and a deterministic source that flips
	// between values straddling 0.5, exactly half the calls fire.
	half := 0.5
	draws := []float64{0.1, 0.7, 0.2, 0.8, 0.3, 0.9} // 3 < 0.5, 3 >= 0.5
	i := 0
	p, err := NewWithRand(map[string]any{
		"up":          map[string]any{"suffix": "x"},
		"probability": half,
	}, func() float64 {
		v := draws[i%len(draws)]
		i++
		return v
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := mkReq("/v1/messages", []v2.Message{textTurn("user", "hello")}, "")
	fires := 0
	for k := 0; k < len(draws); k++ {
		res := p.OnBefore(context.Background(), req, nil)
		if res.Action == v2.ActionMutate {
			fires++
		}
	}
	if fires != 3 {
		t.Errorf("expected 3/6 fires with seeded draws, got %d", fires)
	}
}

func TestProbability_OutOfRangeRejected(t *testing.T) {
	cases := []float64{-0.1, 1.1, 2.0}
	for _, v := range cases {
		_, err := New(map[string]any{
			"up":          map[string]any{"suffix": "x"},
			"probability": v,
		})
		if err == nil {
			t.Errorf("probability=%v should fail Init", v)
		}
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
