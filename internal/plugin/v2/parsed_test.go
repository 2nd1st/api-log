package v2

import (
	"encoding/json"
	"testing"

	"github.com/2nd1st/api-log/internal/sse"
	"github.com/2nd1st/api-log/internal/trace"
)

// Fixtures here lean on the same shapes parser/usage_test.go uses, plus
// a few content-extraction cases that aren't in the usage tests
// (multi-part user content, tool_use blocks, streaming text deltas).

func mkTrace(path, reqBody, respBody string, events []sse.Event) trace.Trace {
	t := trace.Trace{Path: path}
	if reqBody != "" {
		t.Req.Body = json.RawMessage(reqBody)
	}
	if respBody != "" {
		t.Resp.Body = json.RawMessage(respBody)
	}
	if events != nil {
		t.Resp.Events = events
	}
	return t
}

func TestDetectProtocol(t *testing.T) {
	cases := map[string]Protocol{
		"/v1/chat/completions":   ProtocolChat,
		"/v1/messages":           ProtocolMessages,
		"/v1/messages?beta=true": ProtocolMessages,
		"/v1/responses":          ProtocolResponses,
		"/v1beta/models/gemini-2.0-flash:generateContent":     ProtocolGemini,
		"/v1beta/models/gemini-1.5-pro:streamGenerateContent": ProtocolGemini,
		"/healthz": ProtocolUnknown,
	}
	for path, want := range cases {
		if got := DetectProtocol(path); got != want {
			t.Errorf("DetectProtocol(%q) = %v, want %v", path, got, want)
		}
	}
}

// ----- Chat request -------------------------------------------------

func TestParsedRequest_Chat_StringContent(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"hello"}
		]
	}`
	req := ParsedRequestFromTrace(mkTrace("/v1/chat/completions", body, "", nil))
	if req.Protocol != ProtocolChat {
		t.Fatalf("protocol = %v", req.Protocol)
	}
	if req.Model != "gpt-4o" {
		t.Errorf("model = %q", req.Model)
	}
	if req.SystemPrompt != "be terse" {
		t.Errorf("system prompt = %q", req.SystemPrompt)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (system lifted)", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("role = %q", req.Messages[0].Role)
	}
	if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Text != "hello" {
		t.Errorf("content = %+v", req.Messages[0].Content)
	}
}

func TestParsedRequest_Chat_MultiPartContent(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe this"},
			{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}
		]}]
	}`
	req := ParsedRequestFromTrace(mkTrace("/v1/chat/completions", body, "", nil))
	parts := req.Messages[0].Content
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "describe this" {
		t.Errorf("parts[0] = %+v", parts[0])
	}
	if parts[1].Type != "image" || parts[1].URL != "https://example.com/x.png" {
		t.Errorf("parts[1] = %+v", parts[1])
	}
}

func TestParsedRequest_Chat_Streaming(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := ParsedRequestFromTrace(mkTrace("/v1/chat/completions", body, "", nil))
	if !req.Streaming {
		t.Errorf("streaming should be true")
	}
}

func TestParsedRequest_Chat_Tools(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{"name":"weather","description":"get weather","parameters":{"type":"object"}}
		}]
	}`
	req := ParsedRequestFromTrace(mkTrace("/v1/chat/completions", body, "", nil))
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d", len(req.Tools))
	}
	if req.Tools[0].Name != "weather" {
		t.Errorf("tool name = %q", req.Tools[0].Name)
	}
	if string(req.Tools[0].Schema) != `{"type":"object"}` {
		t.Errorf("tool schema = %s", req.Tools[0].Schema)
	}
}

func TestParsedRequest_Chat_AssistantToolCall(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"sf\"}"}}
			]}
		]
	}`
	req := ParsedRequestFromTrace(mkTrace("/v1/chat/completions", body, "", nil))
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	asst := req.Messages[1]
	if asst.Role != "assistant" {
		t.Errorf("role = %q", asst.Role)
	}
	// Tool call lifted into a tool_use part.
	var found *ToolUse
	for _, p := range asst.Content {
		if p.Type == "tool_use" {
			found = p.ToolUse
			break
		}
	}
	if found == nil || found.Name != "weather" {
		t.Fatalf("tool_use missing or wrong: %+v", asst.Content)
	}
}

// ----- Chat response (non-streaming) --------------------------------

func TestParsedResponse_Chat_NonStreaming(t *testing.T) {
	respBody := `{
		"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}
	}`
	resp := ParsedResponseFromTrace(mkTrace("/v1/chat/completions", "", respBody, nil))
	if resp.Content != "answer" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.Usage.FinishReason == nil || *resp.Usage.FinishReason != "stop" {
		t.Errorf("finish reason = %+v", resp.Usage.FinishReason)
	}
}

// ----- Chat response (streaming) ------------------------------------

func TestParsedResponse_Chat_Streaming(t *testing.T) {
	events := []sse.Event{
		{Data: json.RawMessage(`{"choices":[{"delta":{"content":"hello "}}]}`)},
		{Data: json.RawMessage(`{"choices":[{"delta":{"content":"world"}}]}`)},
	}
	resp := ParsedResponseFromTrace(mkTrace("/v1/chat/completions", "", "", events))
	if resp.Content != "hello world" {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestParsedResponse_Chat_StreamingToolCall(t *testing.T) {
	events := []sse.Event{
		{Data: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"weather","arguments":"{\"ci"}}]}}]}`)},
		{Data: json.RawMessage(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"sf\"}"}}]}}]}`)},
	}
	resp := ParsedResponseFromTrace(mkTrace("/v1/chat/completions", "", "", events))
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "weather" || tc.Arguments != `{"city":"sf"}` {
		t.Errorf("tool call = %+v", tc)
	}
}

// ----- Messages request --------------------------------------------

func TestParsedRequest_Messages_StringContent(t *testing.T) {
	body := `{
		"model":"claude-3-5-sonnet",
		"system":"be terse",
		"messages":[{"role":"user","content":"hi"}]
	}`
	req := ParsedRequestFromTrace(mkTrace("/v1/messages", body, "", nil))
	if req.SystemPrompt != "be terse" {
		t.Errorf("system = %q", req.SystemPrompt)
	}
	if req.Messages[0].Content[0].Text != "hi" {
		t.Errorf("content = %+v", req.Messages[0].Content)
	}
}

func TestParsedRequest_Messages_MultiPart(t *testing.T) {
	body := `{
		"model":"claude-3-5-sonnet",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"see this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA="}}
		]}]
	}`
	req := ParsedRequestFromTrace(mkTrace("/v1/messages", body, "", nil))
	parts := req.Messages[0].Content
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	if parts[1].Type != "image" || parts[1].MediaType != "image/png" || parts[1].DataB64 != "AAA=" {
		t.Errorf("image = %+v", parts[1])
	}
}

func TestParsedRequest_Messages_ToolResultBlock(t *testing.T) {
	body := `{
		"model":"claude-3-5-sonnet",
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":"42 degrees","is_error":false}
		]}]
	}`
	req := ParsedRequestFromTrace(mkTrace("/v1/messages", body, "", nil))
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	p := req.Messages[0].Content[0]
	if p.Type != "tool_result" || p.ToolResult == nil {
		t.Fatalf("part = %+v", p)
	}
	if p.ToolResult.ToolUseID != "toolu_1" || p.ToolResult.Content != "42 degrees" {
		t.Errorf("tool result = %+v", p.ToolResult)
	}
}

// ----- Messages response (non-streaming) ---------------------------

func TestParsedResponse_Messages_NonStreaming(t *testing.T) {
	respBody := `{
		"model":"claude-3-5-sonnet",
		"content":[
			{"type":"thinking","thinking":"let me think"},
			{"type":"text","text":"the answer is 42"},
			{"type":"tool_use","id":"toolu_2","name":"calc","input":{"expr":"6*7"}}
		],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1}
	}`
	resp := ParsedResponseFromTrace(mkTrace("/v1/messages", "", respBody, nil))
	if resp.Reasoning != "let me think" {
		t.Errorf("reasoning = %q", resp.Reasoning)
	}
	if resp.Content != "the answer is 42" {
		t.Errorf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "calc" {
		t.Errorf("tool name = %q", resp.ToolCalls[0].Name)
	}
}

// ----- Messages response (streaming) -------------------------------
//
// Streaming Anthropic responses go through the content_block_*
// sequence. We exercise: a text block, a thinking block, and a
// tool_use block. The text and reasoning extractors must concat in
// block order; the tool_use accumulator must collect input_json_delta
// fragments verbatim.

func TestParsedResponse_Messages_Streaming(t *testing.T) {
	events := []sse.Event{
		{Name: "message_start", Data: json.RawMessage(`{"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":1}}}`)},
		{Name: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step "}}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"one"}}`)},
		{Name: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":0}`)},
		{Name: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`)},
		{Name: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":1}`)},
		{Name: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_3","name":"calc"}}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"42}"}}`)},
		{Name: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":2}`)},
		{Name: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)},
		{Name: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
	}
	resp := ParsedResponseFromTrace(mkTrace("/v1/messages", "", "", events))
	if resp.Reasoning != "step one" {
		t.Errorf("reasoning = %q", resp.Reasoning)
	}
	if resp.Content != "answer" {
		t.Errorf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Arguments != `{"x":42}` {
		t.Errorf("tool call args = %q", resp.ToolCalls[0].Arguments)
	}
}

// ----- Responses request -------------------------------------------

func TestParsedRequest_Responses_StringInput(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","instructions":"be terse"}`
	req := ParsedRequestFromTrace(mkTrace("/v1/responses", body, "", nil))
	if req.SystemPrompt != "be terse" {
		t.Errorf("instructions = %q", req.SystemPrompt)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content[0].Text != "hi" {
		t.Errorf("messages = %+v", req.Messages)
	}
}

func TestParsedRequest_Responses_ArrayInput(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`
	req := ParsedRequestFromTrace(mkTrace("/v1/responses", body, "", nil))
	if len(req.Messages) != 1 || req.Messages[0].Content[0].Text != "hello" {
		t.Errorf("messages = %+v", req.Messages)
	}
}

// ----- Responses response (streaming) ------------------------------

func TestParsedResponse_Responses_StreamingText(t *testing.T) {
	events := []sse.Event{
		{Name: "response.output_text.delta", Data: json.RawMessage(`{"type":"response.output_text.delta","delta":"hel"}`)},
		{Name: "response.output_text.delta", Data: json.RawMessage(`{"type":"response.output_text.delta","delta":"lo"}`)},
		{Name: "response.completed", Data: json.RawMessage(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)},
	}
	resp := ParsedResponseFromTrace(mkTrace("/v1/responses", `{"model":"gpt-4o"}`, "", events))
	if resp.Content != "hello" {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestParsedResponse_Responses_StreamingToolCall(t *testing.T) {
	events := []sse.Event{
		{Name: "response.output_item.added", Data: json.RawMessage(`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","name":"weather"}}`)},
		{Name: "response.function_call_arguments.delta", Data: json.RawMessage(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"ci"}`)},
		{Name: "response.function_call_arguments.delta", Data: json.RawMessage(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"ty\":\"sf\"}"}`)},
	}
	resp := ParsedResponseFromTrace(mkTrace("/v1/responses", `{"model":"gpt-4o"}`, "", events))
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "weather" {
		t.Errorf("tool name = %q", resp.ToolCalls[0].Name)
	}
	if resp.ToolCalls[0].Arguments != `{"city":"sf"}` {
		t.Errorf("tool args = %q", resp.ToolCalls[0].Arguments)
	}
}

// ----- Gemini request ----------------------------------------------

func TestParsedRequest_Gemini(t *testing.T) {
	body := `{
		"systemInstruction":{"role":"system","parts":[{"text":"be brief"}]},
		"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"text":"hello"}]}
		]
	}`
	req := ParsedRequestFromTrace(mkTrace("/v1beta/models/gemini-2.0-flash:generateContent", body, "", nil))
	if req.SystemPrompt != "be brief" {
		t.Errorf("system = %q", req.SystemPrompt)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	if req.Messages[1].Role != "assistant" {
		t.Errorf("model role should normalize to assistant: %q", req.Messages[1].Role)
	}
}

func TestParsedResponse_Gemini(t *testing.T) {
	respBody := `{
		"candidates":[{
			"content":{
				"role":"model",
				"parts":[
					{"text":"answer"},
					{"functionCall":{"name":"calc","args":{"x":1}}}
				]
			},
			"finishReason":"STOP"
		}],
		"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
	}`
	resp := ParsedResponseFromTrace(mkTrace("/v1beta/models/gemini-2.0-flash:generateContent", "", respBody, nil))
	if resp.Content != "answer" {
		t.Errorf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "calc" {
		t.Errorf("tool call = %+v", resp.ToolCalls)
	}
}

// ----- Defensive cases ---------------------------------------------

func TestParsedRequest_EmptyTrace(t *testing.T) {
	// Empty trace must not panic.
	req := ParsedRequestFromTrace(trace.Trace{})
	if req.Protocol != ProtocolUnknown {
		t.Errorf("unknown protocol expected, got %v", req.Protocol)
	}
}

func TestParsedRequest_BodyB64Fallback(t *testing.T) {
	// A trace where the body landed in BodyB64 has no parsed body; the
	// builder leaves Messages empty rather than panicking.
	tr := trace.Trace{Path: "/v1/chat/completions"}
	tr.Req.BodyB64 = "aGVsbG8="
	req := ParsedRequestFromTrace(tr)
	if len(req.Messages) != 0 {
		t.Errorf("messages should be empty on body_b64, got %d", len(req.Messages))
	}
}

func TestParsedRequest_InvalidJSONBody(t *testing.T) {
	tr := trace.Trace{Path: "/v1/chat/completions"}
	tr.Req.Body = json.RawMessage(`{not valid`)
	req := ParsedRequestFromTrace(tr)
	if len(req.Messages) != 0 || req.Model != "" {
		t.Errorf("invalid JSON should yield empty fields: %+v", req)
	}
}
