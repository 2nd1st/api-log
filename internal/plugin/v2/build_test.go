package v2

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Roundtrip strategy per advisor: assert semantic equality (Unmarshal
// both sides to map[string]any and compare), not byte equality.
// Field-ordering and optional-whitespace differences are deliberate.

func roundtripRequest(t *testing.T, path, body string) (got, want map[string]any) {
	t.Helper()
	req := ParsedRequestFromTrace(mkTrace(path, body, "", nil))
	out, err := BuildRequestBody(&req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal built: %v", err)
	}
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	return got, want
}

// ----- Chat ---------------------------------------------------------

func TestBuildRequest_Chat_StringContent(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	got, want := roundtripRequest(t, "/v1/chat/completions", body)
	if !reflect.DeepEqual(got["model"], want["model"]) {
		t.Errorf("model: got %v want %v", got["model"], want["model"])
	}
	gm := got["messages"].([]any)[0].(map[string]any)
	wm := want["messages"].([]any)[0].(map[string]any)
	if gm["role"] != wm["role"] || gm["content"] != wm["content"] {
		t.Errorf("first message diff:\n got=%v\nwant=%v", gm, wm)
	}
}

func TestBuildRequest_Chat_PreservesUnknownFields(t *testing.T) {
	// Fields the normalizer does not lift (temperature, response_format,
	// custom keys) MUST survive the roundtrip via RawBody.
	body := `{
		"model":"gpt-4o",
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.5,
		"response_format":{"type":"json_object"}
	}`
	got, want := roundtripRequest(t, "/v1/chat/completions", body)
	if !reflect.DeepEqual(got["temperature"], want["temperature"]) {
		t.Errorf("temperature lost: %v vs %v", got["temperature"], want["temperature"])
	}
	if !reflect.DeepEqual(got["response_format"], want["response_format"]) {
		t.Errorf("response_format lost: %v vs %v", got["response_format"], want["response_format"])
	}
}

func TestBuildRequest_Chat_SystemPromptRoundtrip(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"hi"}
		]
	}`
	got, _ := roundtripRequest(t, "/v1/chat/completions", body)
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages roundtrip count = %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be terse" {
		t.Errorf("system prompt not preserved: %+v", first)
	}
}

func TestBuildRequest_Chat_ToolCallsRoundtrip(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"messages":[
			{"role":"assistant","content":"","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"sf\"}"}}
			]}
		]
	}`
	got, _ := roundtripRequest(t, "/v1/chat/completions", body)
	asst := got["messages"].([]any)[0].(map[string]any)
	tcs, ok := asst["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls = %+v", asst["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("id = %v", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "weather" || fn["arguments"] != `{"city":"sf"}` {
		t.Errorf("function = %+v", fn)
	}
}

// ----- Messages -----------------------------------------------------

func TestBuildRequest_Messages_StringContent(t *testing.T) {
	body := `{
		"model":"claude-3-5-sonnet",
		"system":"be terse",
		"messages":[{"role":"user","content":"hi"}]
	}`
	got, _ := roundtripRequest(t, "/v1/messages", body)
	if got["system"] != "be terse" {
		t.Errorf("system = %v", got["system"])
	}
	m := got["messages"].([]any)[0].(map[string]any)
	if m["content"] != "hi" {
		t.Errorf("content = %v", m["content"])
	}
}

func TestBuildRequest_Messages_MultiPart(t *testing.T) {
	body := `{
		"model":"claude-3-5-sonnet",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"see this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA="}}
		]}]
	}`
	got, _ := roundtripRequest(t, "/v1/messages", body)
	parts := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts roundtrip = %d", len(parts))
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image" {
		t.Errorf("image type = %v", img["type"])
	}
	src := img["source"].(map[string]any)
	if src["media_type"] != "image/png" || src["data"] != "AAA=" {
		t.Errorf("source = %+v", src)
	}
}

func TestBuildRequest_Messages_PreservesUnknownFields(t *testing.T) {
	// max_tokens and metadata are NOT lifted; they live in RawBody.
	body := `{
		"model":"claude-3-5-sonnet",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hi"}],
		"metadata":{"user_id":"abc"}
	}`
	got, want := roundtripRequest(t, "/v1/messages", body)
	if !reflect.DeepEqual(got["max_tokens"], want["max_tokens"]) {
		t.Errorf("max_tokens: %v vs %v", got["max_tokens"], want["max_tokens"])
	}
	if !reflect.DeepEqual(got["metadata"], want["metadata"]) {
		t.Errorf("metadata: %v vs %v", got["metadata"], want["metadata"])
	}
}

// ----- Responses ----------------------------------------------------

func TestBuildRequest_Responses_StringInput(t *testing.T) {
	// /v1/responses normalizes the input field into an array of message
	// objects; the roundtrip target is the normalized shape (semantic
	// equality, not byte equality — the input string becomes one user
	// message with one input_text part).
	body := `{"model":"gpt-4o","input":"hi"}`
	req := ParsedRequestFromTrace(mkTrace("/v1/responses", body, "", nil))
	out, err := BuildRequestBody(&req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["model"] != "gpt-4o" {
		t.Errorf("model = %v", got["model"])
	}
	arr, ok := got["input"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("input = %+v", got["input"])
	}
	msg := arr[0].(map[string]any)
	parts := msg["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("parts = %+v", parts)
	}
	first := parts[0].(map[string]any)
	if first["type"] != "input_text" || first["text"] != "hi" {
		t.Errorf("part = %+v", first)
	}
}

// ----- Empty / defensive ------------------------------------------

func TestBuildRequest_NilReturnsError(t *testing.T) {
	_, err := BuildRequestBody(nil)
	if err == nil {
		t.Errorf("nil should return error")
	}
}

func TestBuildResponseBody_NilReturnsError(t *testing.T) {
	_, err := BuildResponseBody(nil)
	if err == nil {
		t.Errorf("nil should return error")
	}
}

func TestBuildRequest_UnknownProtocolErrors(t *testing.T) {
	req := &ParsedRequest{Protocol: ProtocolUnknown}
	if _, err := BuildRequestBody(req); err == nil {
		t.Errorf("unknown protocol should error")
	}
}

// ----- Response build (Chat non-streaming) ------------------------

func TestBuildResponse_Chat(t *testing.T) {
	body := `{
		"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`
	resp := ParsedResponseFromTrace(mkTrace("/v1/chat/completions", "", body, nil))
	out, err := BuildResponseBody(&resp)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	choices := got["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "answer" {
		t.Errorf("content = %v", msg["content"])
	}
}

func TestBuildResponse_Messages_ContentOrdering(t *testing.T) {
	// When both reasoning and content are present, the build emits
	// thinking BEFORE text — matching Anthropic's on-the-wire ordering.
	resp := ParsedResponse{
		Protocol:  ProtocolMessages,
		Reasoning: "think",
		Content:   "answer",
	}
	out, err := BuildResponseBody(&resp)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	blocks := got["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	if blocks[0].(map[string]any)["type"] != "thinking" {
		t.Errorf("block[0].type = %v", blocks[0].(map[string]any)["type"])
	}
	if blocks[1].(map[string]any)["type"] != "text" {
		t.Errorf("block[1].type = %v", blocks[1].(map[string]any)["type"])
	}
}
