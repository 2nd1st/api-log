package textreplace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	v2 "github.com/leoyun/api-log/internal/plugin/v2"
	"github.com/leoyun/api-log/internal/sse"
)

// mkReq is a small helper for the common case: one user turn with
// single-string text content. Tests that need richer message shapes
// build the ParsedRequest by hand.
func mkReq(path, text string) *v2.ParsedRequest {
	return &v2.ParsedRequest{
		Path: path,
		Messages: []v2.Message{
			{
				Role: "user",
				Content: []v2.ContentPart{
					{Type: "text", Text: text},
				},
			},
		},
	}
}

// ----- config validation ---------------------------------------------

func TestNew_RejectsEmptyMatch(t *testing.T) {
	cases := []map[string]any{
		{"up": []any{map[string]any{"match": "", "replace": "x"}}},
		{"down": []any{map[string]any{"match": "", "replace": "x"}}},
	}
	for i, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("case %d: empty match should error", i)
		}
	}
}

func TestNew_RejectsLoneStarRouteAndEmptyRoute(t *testing.T) {
	// Lone "*" collapses to all-paths, not an error.
	p, err := New(map[string]any{"routes": []any{"*"}})
	if err != nil {
		t.Fatalf("lone '*' should compile to all-paths: %v", err)
	}
	// The compiled matcher must accept arbitrary paths after the
	// collapse — the internal `all` flag is package-private; checking
	// behavior is the contract surface.
	if !p.routes.Matches("/v1/anything") {
		t.Errorf("lone '*' should match arbitrary path")
	}
	if !p.routes.Matches("/something/else") {
		t.Errorf("lone '*' should match every path")
	}
	// Empty pattern entry is an operator typo.
	if _, err := New(map[string]any{"routes": []any{""}}); err == nil {
		t.Errorf("empty route entry should error")
	}
}

func TestNew_NilCfgIsValidNoop(t *testing.T) {
	p, err := New(nil)
	if err != nil {
		t.Fatalf("nil cfg: %v", err)
	}
	res := p.OnBefore(context.Background(), mkReq("/v1/messages", "hi"), nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("nil cfg should be a no-op")
	}
}

// ----- BEFORE: non-match route ---------------------------------------

func TestOnBefore_NonMatchRoute(t *testing.T) {
	p, err := New(map[string]any{
		"routes": []any{"/v1/chat/completions"},
		"up":     []any{map[string]any{"match": "你", "replace": "Y"}},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := mkReq("/v1/messages", "你好")
	res := p.OnBefore(context.Background(), req, nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("non-matching route should Continue, got %v", res.Action)
	}
	if res.Mutated != nil {
		t.Errorf("non-matching route should not produce Mutated")
	}
	// Original request must be unchanged (defense against aliasing bugs).
	if req.Messages[0].Content[0].Text != "你好" {
		t.Errorf("original request mutated: %q", req.Messages[0].Content[0].Text)
	}
}

// ----- BEFORE: empty rules -------------------------------------------

func TestOnBefore_EmptyRules(t *testing.T) {
	p, err := New(map[string]any{"routes": []any{"/v1/*"}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res := p.OnBefore(context.Background(), mkReq("/v1/messages", "hi"), nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("empty Up rules should Continue, got %v", res.Action)
	}
}

// ----- BEFORE: single rule matching ----------------------------------

func TestOnBefore_SingleRuleMatching(t *testing.T) {
	p, err := New(map[string]any{
		"routes": []any{"/v1/*"},
		"up": []any{
			map[string]any{"match": "你", "replace": "世界上最好最好的ai"},
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	original := "你好，世界"
	req := mkReq("/v1/messages", original)
	res := p.OnBefore(context.Background(), req, nil)
	if res.Action != v2.ActionMutate {
		t.Fatalf("Action = %v, want Mutate", res.Action)
	}
	if res.Mutated == nil {
		t.Fatal("Mutated nil despite ActionMutate")
	}
	got := res.Mutated.Messages[0].Content[0].Text
	if got != "世界上最好最好的ai好，世界" {
		t.Errorf("mutated text = %q", got)
	}
	// Original turn's text must not be aliased into the result.
	if req.Messages[0].Content[0].Text != original {
		t.Errorf("input request mutated in place: %q", req.Messages[0].Content[0].Text)
	}
}

// TestOnBefore_TextOnlyMutation: non-text parts (image, tool_use,
// tool_result) MUST pass through untouched even when the match string
// appears in the unlifted JSON.
func TestOnBefore_LeavesNonTextPartsAlone(t *testing.T) {
	p, err := New(map[string]any{
		"up": []any{map[string]any{"match": "secret", "replace": "REDACTED"}},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := &v2.ParsedRequest{
		Path: "/v1/messages",
		Messages: []v2.Message{
			{
				Role: "user",
				Content: []v2.ContentPart{
					{Type: "text", Text: "this has a secret in it"},
					{Type: "image", URL: "https://example.com/secret.png"},
					{Type: "tool_use", ToolUse: &v2.ToolUse{
						Name:  "lookup",
						Input: json.RawMessage(`{"q":"secret"}`),
					}},
				},
			},
		},
	}
	res := p.OnBefore(context.Background(), req, nil)
	if res.Action != v2.ActionMutate {
		t.Fatalf("text part should trigger Mutate")
	}
	parts := res.Mutated.Messages[0].Content
	if parts[0].Text != "this has a REDACTED in it" {
		t.Errorf("text part not mutated: %q", parts[0].Text)
	}
	if parts[1].URL != "https://example.com/secret.png" {
		t.Errorf("image url should not be touched: %q", parts[1].URL)
	}
	if string(parts[2].ToolUse.Input) != `{"q":"secret"}` {
		t.Errorf("tool_use input should not be touched: %s", parts[2].ToolUse.Input)
	}
}

// TestOnBefore_NoMatchProducesContinue: the route matches and rules
// are non-empty, but the needle is absent — the plugin must avoid the
// pointless Mutate-then-rebuild round-trip.
func TestOnBefore_NoMatchProducesContinue(t *testing.T) {
	p, err := New(map[string]any{
		"up": []any{map[string]any{"match": "needle", "replace": "x"}},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res := p.OnBefore(context.Background(), mkReq("/v1/messages", "haystack"), nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("absent needle should Continue, got %v", res.Action)
	}
}

func TestOnBefore_MultipleRulesInOrder(t *testing.T) {
	p, err := New(map[string]any{
		"up": []any{
			map[string]any{"match": "A", "replace": "B"},
			map[string]any{"match": "B", "replace": "C"},
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res := p.OnBefore(context.Background(), mkReq("/v1/messages", "A"), nil)
	if res.Action != v2.ActionMutate {
		t.Fatalf("expected Mutate; got %v", res.Action)
	}
	// Rule 1 turns "A" into "B"; rule 2 turns the new "B" into "C".
	got := res.Mutated.Messages[0].Content[0].Text
	if got != "C" {
		t.Errorf("ordered application broken: got %q, want %q", got, "C")
	}
}

// ----- AFTER non-streaming -------------------------------------------

func TestOnAfter_NonStreaming_Mutates(t *testing.T) {
	p, err := New(map[string]any{
		"down": []any{
			map[string]any{"match": "AI", "replace": "世界上最好的好哥哥"},
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := &v2.ParsedRequest{Path: "/v1/messages"}
	ac := &v2.AfterContext{Response: &v2.ParsedResponse{Content: "I am an AI"}}
	res := p.OnAfter(context.Background(), req, ac, nil)
	if res.Action != v2.ActionMutate {
		t.Fatalf("Action = %v, want Mutate", res.Action)
	}
	if res.Mutated == nil {
		t.Fatal("Mutated nil")
	}
	if res.Mutated.Content != "I am an 世界上最好的好哥哥" {
		t.Errorf("content = %q", res.Mutated.Content)
	}
	// The original ParsedResponse must NOT have been mutated in place.
	if ac.Response.Content != "I am an AI" {
		t.Errorf("input response mutated in place: %q", ac.Response.Content)
	}
}

func TestOnAfter_NonStreaming_NoChangeContinue(t *testing.T) {
	p, err := New(map[string]any{
		"down": []any{map[string]any{"match": "absent", "replace": "x"}},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ac := &v2.AfterContext{Response: &v2.ParsedResponse{Content: "hello world"}}
	res := p.OnAfter(context.Background(), &v2.ParsedRequest{}, ac, nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("no-change should Continue, got %v", res.Action)
	}
}

// ----- AFTER streaming -----------------------------------------------

// TestOnAfter_Streaming_ContentDeltaMutated: the OnContentDelta
// transform registered by OnAfter must rewrite a text content delta as
// it flows through the W1 StreamDispatcher. Tests the framework
// integration, not the transform in isolation.
func TestOnAfter_Streaming_ContentDeltaMutated(t *testing.T) {
	p, err := New(map[string]any{
		"down": []any{map[string]any{"match": "你", "replace": "Y"}},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := &v2.ParsedRequest{Path: "/v1/messages", Protocol: v2.ProtocolMessages, Streaming: true}
	ac := &v2.AfterContext{} // streaming: Response nil
	res := p.OnAfter(context.Background(), req, ac, nil)
	if res.Action != v2.ActionContinue {
		t.Fatalf("streaming OnAfter should Continue and register; got %v", res.Action)
	}
	if got := ac.ContentDeltaTransforms(); len(got) != 1 {
		t.Fatalf("expected 1 content-delta transform, got %d", len(got))
	}

	d := &v2.StreamDispatcher{Protocol: v2.ProtocolMessages, After: ac}
	ev := sse.Event{Name: "content_block_delta", Data: json.RawMessage(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`)}
	// R1 dispatcher buffers content deltas one event of lookahead per
	// block; the OnContentDelta transform is applied at buffering time,
	// the buffered delta surfaces on the next event or on flush.
	d.Process(ev)
	out := d.FlushBeforeFinish()
	if len(out) != 1 {
		t.Fatalf("flushed len = %d", len(out))
	}
	if !strings.Contains(string(out[0].Data), `"Y好"`) {
		t.Errorf("content delta not mutated: %s", out[0].Data)
	}
}

// TestOnAfter_Streaming_ToolUseDeltaUntouched is the §10.6 carve-out
// guard at the plugin layer: even when the match string appears inside
// a tool_use input_json_delta, the plugin's registered transform must
// NOT see it (the W1 classifier routes tool deltas to a separate class).
// The bytes of a tool-use event before vs after dispatch must be
// identical.
func TestOnAfter_Streaming_ToolUseDeltaUntouched(t *testing.T) {
	p, err := New(map[string]any{
		"down": []any{map[string]any{"match": "city", "replace": "CORRUPTED"}},
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
		t.Errorf("tool_use event MUTATED — spec §10.6 carve-out violated\nbefore: %s\nafter:  %s",
			tool.Data, out[0].Data)
	}
	if strings.Contains(string(out[0].Data), "CORRUPTED") {
		t.Errorf("tool-use JSON leaked into content transform: %s", out[0].Data)
	}
}

// ----- AFTER: non-match route / empty rules in streaming -------------

func TestOnAfter_Streaming_NonMatchRoute_NoRegistration(t *testing.T) {
	p, err := New(map[string]any{
		"routes": []any{"/v1/chat/completions"},
		"down":   []any{map[string]any{"match": "x", "replace": "y"}},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req := &v2.ParsedRequest{Path: "/v1/messages", Protocol: v2.ProtocolMessages, Streaming: true}
	ac := &v2.AfterContext{}
	res := p.OnAfter(context.Background(), req, ac, nil)
	if res.Action != v2.ActionContinue {
		t.Errorf("non-match route should Continue, got %v", res.Action)
	}
	if got := ac.ContentDeltaTransforms(); len(got) != 0 {
		t.Errorf("non-match route should not register transforms, got %d", len(got))
	}
}

func TestOnAfter_Streaming_EmptyDown_NoRegistration(t *testing.T) {
	p, err := New(map[string]any{"routes": []any{"/v1/*"}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ac := &v2.AfterContext{}
	p.OnAfter(context.Background(), &v2.ParsedRequest{Path: "/v1/messages"}, ac, nil)
	if got := ac.ContentDeltaTransforms(); len(got) != 0 {
		t.Errorf("empty Down should not register transforms, got %d", len(got))
	}
}

// ----- ConfigSchema --------------------------------------------------

func TestConfigSchema_DescribesAllFields(t *testing.T) {
	p, err := New(nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	schema := p.ConfigSchema()
	want := map[string]bool{"routes": false, "up": false, "down": false}
	for _, f := range schema.Fields {
		if _, ok := want[f.Name]; !ok {
			t.Errorf("unexpected field %q", f.Name)
		}
		want[f.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("schema missing field %q", name)
		}
	}
}

// ----- Registration sanity -------------------------------------------

// TestRegisteredCtor_BuildsViaLookup proves init() landed the type into
// the v2 builtin registry and that the registered ctor accepts the same
// cfg shape New does. We do NOT call RegisterBuiltin again from tests —
// that would duplicate-panic per the W1 contract.
func TestRegisteredCtor_BuildsViaLookup(t *testing.T) {
	ctor, ok := v2.LookupBuiltin(Type)
	if !ok {
		t.Fatalf("init() did not register %q", Type)
	}
	built, err := ctor(map[string]any{
		"up":   []any{map[string]any{"match": "a", "replace": "b"}},
		"down": []any{map[string]any{"match": "c", "replace": "d"}},
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
