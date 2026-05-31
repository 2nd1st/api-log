package parser

import (
	"encoding/json"
	"testing"

	"github.com/2nd1st/api-log/internal/sse"
	"github.com/2nd1st/api-log/internal/trace"
)

// Fixtures below are mostly real shapes copied from sub2api dev-stack
// captures (deploy/dev-stack/data/2026-05-29/*.jsonl) for the chat and
// responses protocols. Messages and Gemini have no on-disk sample in
// this repo; their fixtures come straight from the investigation
// contract (PHILOSOPHY §1 carve-out 1 — these are the protocol-named
// shapes, no synthesis).

// ----- helpers ----------------------------------------------------------

func mkTrace(path string, reqBody, respBody string, events []sse.Event) trace.Trace {
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

func ptrEq(t *testing.T, label string, got, want *int64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got == nil && want != nil:
		t.Errorf("%s: got nil, want %d", label, *want)
	case got != nil && want == nil:
		t.Errorf("%s: got %d, want nil", label, *got)
	case *got != *want:
		t.Errorf("%s: got %d, want %d", label, *got, *want)
	}
}

func strEq(t *testing.T, label string, got, want *string) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got == nil && want != nil:
		t.Errorf("%s: got nil, want %q", label, *want)
	case got != nil && want == nil:
		t.Errorf("%s: got %q, want nil", label, *got)
	case *got != *want:
		t.Errorf("%s: got %q, want %q", label, *got, *want)
	}
}

func p(v int64) *int64    { return &v }
func sp(s string) *string { return &s }

// ----- OpenAI Chat -------------------------------------------------------

func TestExtractUsage_Chat_HappyPath(t *testing.T) {
	// Real shape from deploy/dev-stack/data/2026-05-29 chat trace.
	respBody := `{
		"id":"chatcmpl-mock-1",
		"object":"chat.completion",
		"model":"gpt-4o",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
		"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}
	}`
	got := ExtractUsage(mkTrace("/v1/chat/completions", "", respBody, nil))

	strEq(t, "Model", got.Model, sp("gpt-4o"))
	strEq(t, "FinishReason", got.FinishReason, sp("stop"))
	ptrEq(t, "PromptTokens", got.PromptTokens, p(10))
	ptrEq(t, "CompletionTokens", got.CompletionTokens, p(1))
	ptrEq(t, "TotalTokens", got.TotalTokens, p(11))
	ptrEq(t, "CachedTokens", got.CachedTokens, nil)
	ptrEq(t, "ReasoningTokens", got.ReasoningTokens, nil)
}

func TestExtractUsage_Chat_CachedAndReasoning(t *testing.T) {
	// Newer OpenAI chat responses include details blocks.
	respBody := `{
		"model":"gpt-4o-mini",
		"choices":[{"finish_reason":"length"}],
		"usage":{
			"prompt_tokens":100,
			"completion_tokens":50,
			"total_tokens":150,
			"prompt_tokens_details":{"cached_tokens":40},
			"completion_tokens_details":{"reasoning_tokens":20}
		}
	}`
	got := ExtractUsage(mkTrace("/v1/chat/completions", "", respBody, nil))
	ptrEq(t, "CachedTokens", got.CachedTokens, p(40))
	ptrEq(t, "ReasoningTokens", got.ReasoningTokens, p(20))
	strEq(t, "FinishReason", got.FinishReason, sp("length"))
}

func TestExtractUsage_Chat_ModelFallsBackToRequest(t *testing.T) {
	// Response body missing model -> fall back to request body.
	reqBody := `{"model":"gpt-4o","messages":[]}`
	respBody := `{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	got := ExtractUsage(mkTrace("/v1/chat/completions", reqBody, respBody, nil))
	strEq(t, "Model", got.Model, sp("gpt-4o"))
}

func TestExtractUsage_Chat_MissingUsage(t *testing.T) {
	// No usage block at all — token fields stay nil, no panic.
	respBody := `{"model":"gpt-4o","choices":[]}`
	got := ExtractUsage(mkTrace("/v1/chat/completions", "", respBody, nil))
	ptrEq(t, "PromptTokens", got.PromptTokens, nil)
	ptrEq(t, "CompletionTokens", got.CompletionTokens, nil)
	ptrEq(t, "TotalTokens", got.TotalTokens, nil)
	ptrEq(t, "CachedTokens", got.CachedTokens, nil)
	strEq(t, "Model", got.Model, sp("gpt-4o"))
}

// ----- Anthropic Messages ------------------------------------------------

func TestExtractUsage_Messages_HappyPath(t *testing.T) {
	// Real shape from deploy/dev-stack/data/2026-05-29 messages trace.
	// Anthropic does NOT include total_tokens — we expect it computed.
	respBody := `{
		"id":"msg_mock_1",
		"type":"message",
		"role":"assistant",
		"model":"claude-3-5-sonnet",
		"content":[{"type":"text","text":"ok"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"output_tokens":1}
	}`
	got := ExtractUsage(mkTrace("/v1/messages", "", respBody, nil))

	strEq(t, "Model", got.Model, sp("claude-3-5-sonnet"))
	strEq(t, "FinishReason", got.FinishReason, sp("end_turn"))
	ptrEq(t, "PromptTokens", got.PromptTokens, p(10))
	ptrEq(t, "CompletionTokens", got.CompletionTokens, p(1))
	// total_tokens not provided -> computed 10 + 1 = 11.
	ptrEq(t, "TotalTokens", got.TotalTokens, p(11))
	ptrEq(t, "CachedTokens", got.CachedTokens, nil)
	ptrEq(t, "CacheCreationTokens", got.CacheCreationTokens, nil)
	ptrEq(t, "ReasoningTokens", got.ReasoningTokens, nil)
}

func TestExtractUsage_Messages_CacheReadAndCreationSplit(t *testing.T) {
	// Anthropic's prompt caching surfaces two distinct fields.
	respBody := `{
		"model":"claude-3-5-sonnet",
		"stop_reason":"end_turn",
		"usage":{
			"input_tokens":50,
			"output_tokens":10,
			"cache_read_input_tokens":200,
			"cache_creation_input_tokens":75
		}
	}`
	got := ExtractUsage(mkTrace("/v1/messages", "", respBody, nil))
	ptrEq(t, "CachedTokens", got.CachedTokens, p(200))
	ptrEq(t, "CacheCreationTokens", got.CacheCreationTokens, p(75))
	// Cache fields must NOT be folded into prompt or total.
	ptrEq(t, "PromptTokens", got.PromptTokens, p(50))
	ptrEq(t, "TotalTokens", got.TotalTokens, p(60))
}

func TestExtractUsage_Messages_MissingFields(t *testing.T) {
	respBody := `{}`
	got := ExtractUsage(mkTrace("/v1/messages", "", respBody, nil))
	ptrEq(t, "PromptTokens", got.PromptTokens, nil)
	ptrEq(t, "CompletionTokens", got.CompletionTokens, nil)
	ptrEq(t, "TotalTokens", got.TotalTokens, nil)
	strEq(t, "Model", got.Model, nil)
	strEq(t, "FinishReason", got.FinishReason, nil)
}

func TestExtractUsage_Messages_Streaming_HappyPath(t *testing.T) {
	// Real shape sampled from sub2api 2026-05-30:
	//   message_start.data.message.{model, usage.{input_tokens,
	//     cache_read_input_tokens, cache_creation_input_tokens, output_tokens}}
	//   (final) message_delta.data.{delta.stop_reason, usage.output_tokens}
	//   message_stop carries no usage.
	events := []sse.Event{
		{Name: "message_start", Data: json.RawMessage(`{
			"type":"message_start",
			"message":{
				"id":"msg_x","role":"assistant","model":"claude-opus-4-8",
				"usage":{
					"input_tokens":126070,
					"cache_creation_input_tokens":267,
					"cache_read_input_tokens":109114,
					"output_tokens":1
				}
			}
		}`)},
		{Name: "content_block_start", Data: json.RawMessage(`{"type":"content_block_start","index":0}`)},
		{Name: "content_block_delta", Data: json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)},
		{Name: "content_block_stop", Data: json.RawMessage(`{"type":"content_block_stop","index":0}`)},
		{Name: "message_delta", Data: json.RawMessage(`{
			"type":"message_delta",
			"delta":{"stop_reason":"tool_use","stop_sequence":null},
			"usage":{"input_tokens":1,"output_tokens":487}
		}`)},
		{Name: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
	}
	got := ExtractUsage(mkTrace("/v1/messages?beta=true", "", "", events))

	strEq(t, "Model", got.Model, sp("claude-opus-4-8"))
	strEq(t, "FinishReason", got.FinishReason, sp("tool_use"))
	ptrEq(t, "PromptTokens", got.PromptTokens, p(126070))
	// output_tokens taken from the FINAL message_delta, not the
	// initial-and-conservatively-low value in message_start.
	ptrEq(t, "CompletionTokens", got.CompletionTokens, p(487))
	// total = prompt + completion (Anthropic does not provide it).
	ptrEq(t, "TotalTokens", got.TotalTokens, p(126557))
	ptrEq(t, "CachedTokens", got.CachedTokens, p(109114))
	ptrEq(t, "CacheCreationTokens", got.CacheCreationTokens, p(267))
}

func TestExtractUsage_Messages_Streaming_MultipleDeltasLastWins(t *testing.T) {
	// Anthropic emits message_delta multiple times during long streams;
	// each carries the cumulative output_tokens to date. We must take the
	// LAST one, not the first. Same for stop_reason (which is only emitted
	// on the final delta but treat 'most recent non-nil wins' as the rule).
	events := []sse.Event{
		{Name: "message_start", Data: json.RawMessage(`{
			"type":"message_start","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":100}}
		}`)},
		{Name: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":10}}`)},
		{Name: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":42}}`)},
		{Name: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":99}}`)},
		{Name: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
	}
	got := ExtractUsage(mkTrace("/v1/messages", "", "", events))

	strEq(t, "Model", got.Model, sp("claude-haiku-4-5"))
	strEq(t, "FinishReason", got.FinishReason, sp("end_turn"))
	ptrEq(t, "PromptTokens", got.PromptTokens, p(100))
	ptrEq(t, "CompletionTokens", got.CompletionTokens, p(99))
	ptrEq(t, "TotalTokens", got.TotalTokens, p(199))
}

func TestExtractUsage_Messages_Streaming_FirstStartWins(t *testing.T) {
	// Defensive: if a recording ever contains two message_start events
	// (it shouldn't per protocol, but be explicit), the first one wins —
	// matches the documented contract.
	events := []sse.Event{
		{Name: "message_start", Data: json.RawMessage(`{"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":1000}}}`)},
		{Name: "message_start", Data: json.RawMessage(`{"type":"message_start","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":1}}}`)},
		{Name: "message_delta", Data: json.RawMessage(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)},
	}
	got := ExtractUsage(mkTrace("/v1/messages", "", "", events))
	strEq(t, "Model", got.Model, sp("claude-opus-4-8"))
	ptrEq(t, "PromptTokens", got.PromptTokens, p(1000))
}

func TestExtractUsage_Messages_Streaming_NoUsageReturnsZero(t *testing.T) {
	// Stream cut before any message_delta lands — output_tokens absent.
	// Initial input_tokens still extracted from message_start.
	events := []sse.Event{
		{Name: "message_start", Data: json.RawMessage(`{"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":50}}}`)},
	}
	got := ExtractUsage(mkTrace("/v1/messages", "", "", events))
	strEq(t, "Model", got.Model, sp("claude-opus-4-8"))
	ptrEq(t, "PromptTokens", got.PromptTokens, p(50))
	ptrEq(t, "CompletionTokens", got.CompletionTokens, nil)
	// total stays nil — computeTotal requires both prompt and completion.
	ptrEq(t, "TotalTokens", got.TotalTokens, nil)
	strEq(t, "FinishReason", got.FinishReason, nil)
}

// ----- OpenAI Responses --------------------------------------------------

func TestExtractUsage_Responses_NonStreamingBody(t *testing.T) {
	// Real shape from deploy/dev-stack/data/2026-05-29/09e392ae.jsonl.
	// /v1/responses with a non-streaming body (top-level usage, no
	// .response wrapper).
	reqBody := `{"model":"gpt-4o","input":"hi"}`
	respBody := `{
		"id":"resp_mock_1",
		"object":"response",
		"model":"gpt-4o",
		"status":"completed",
		"output":[],
		"usage":{"input_tokens":8,"output_tokens":1,"total_tokens":9}
	}`
	got := ExtractUsage(mkTrace("/v1/responses", reqBody, respBody, nil))

	strEq(t, "Model", got.Model, sp("gpt-4o"))
	strEq(t, "FinishReason", got.FinishReason, sp("completed"))
	ptrEq(t, "PromptTokens", got.PromptTokens, p(8))
	ptrEq(t, "CompletionTokens", got.CompletionTokens, p(1))
	ptrEq(t, "TotalTokens", got.TotalTokens, p(9))
}

func TestExtractUsage_Responses_ModelComesFromRequestNotResponse(t *testing.T) {
	// Contract: responses.Model = req.body.model. In real OpenAI traffic
	// the response body echoes the RESOLVED model (e.g. request "gpt-4o"
	// → response "gpt-4o-2024-11-20"). The contract pins it to the
	// request value; the resolved string in the response must NOT win.
	reqBody := `{"model":"gpt-4o","input":"hi"}`
	respBody := `{"model":"gpt-4o-2024-11-20","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	got := ExtractUsage(mkTrace("/v1/responses", reqBody, respBody, nil))
	strEq(t, "Model", got.Model, sp("gpt-4o"))

	// Same constraint on the streaming shape: req.body.model wins over
	// the .response.model echoed back in response.completed.
	events := []sse.Event{
		{Name: "response.completed", Data: json.RawMessage(`{
			"type":"response.completed",
			"response":{"model":"gpt-4o-2024-11-20","status":"completed",
			"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}
		}`)},
	}
	gotS := ExtractUsage(mkTrace("/v1/responses", reqBody, "", events))
	strEq(t, "Model (streaming)", gotS.Model, sp("gpt-4o"))
}

func TestExtractUsage_Responses_StreamingLastEvent(t *testing.T) {
	// Streaming /v1/responses ends in `event: response.completed` whose
	// data wraps the usage block under `.response`. Shape mirrors the
	// SSE parser test in internal/sse/parser_test.go.
	reqBody := `{"model":"gpt-4o","input":"hi"}`
	events := []sse.Event{
		{Name: "response.created", Data: json.RawMessage(`{"type":"response.created","response":{"id":"resp_1"}}`)},
		{Name: "response.output_text.delta", Data: json.RawMessage(`{"type":"response.output_text.delta","delta":"hi"}`)},
		{Name: "response.completed", Data: json.RawMessage(`{
			"type":"response.completed",
			"response":{
				"id":"resp_1",
				"model":"gpt-4o",
				"status":"completed",
				"usage":{
					"input_tokens":120,
					"output_tokens":40,
					"total_tokens":160,
					"input_tokens_details":{"cached_tokens":80},
					"output_tokens_details":{"reasoning_tokens":12}
				}
			}
		}`)},
	}
	got := ExtractUsage(mkTrace("/v1/responses", reqBody, "", events))

	strEq(t, "Model", got.Model, sp("gpt-4o"))
	strEq(t, "FinishReason", got.FinishReason, sp("completed"))
	ptrEq(t, "PromptTokens", got.PromptTokens, p(120))
	ptrEq(t, "CompletionTokens", got.CompletionTokens, p(40))
	ptrEq(t, "TotalTokens", got.TotalTokens, p(160))
	ptrEq(t, "CachedTokens", got.CachedTokens, p(80))
	ptrEq(t, "ReasoningTokens", got.ReasoningTokens, p(12))
}

func TestExtractUsage_Responses_StreamCutMidFlight(t *testing.T) {
	// Last event is NOT response.completed → nested usage absent.
	// Fields stay nil; we still pick up model from the request body.
	reqBody := `{"model":"gpt-4o","input":"hi"}`
	events := []sse.Event{
		{Name: "response.output_text.delta", Data: json.RawMessage(`{"type":"response.output_text.delta","delta":"partial"}`)},
	}
	got := ExtractUsage(mkTrace("/v1/responses", reqBody, "", events))
	strEq(t, "Model", got.Model, sp("gpt-4o"))
	ptrEq(t, "PromptTokens", got.PromptTokens, nil)
	ptrEq(t, "CompletionTokens", got.CompletionTokens, nil)
	ptrEq(t, "TotalTokens", got.TotalTokens, nil)
	strEq(t, "FinishReason", got.FinishReason, nil)
}

// ----- Google Gemini -----------------------------------------------------

func TestExtractUsage_Gemini_HappyPath(t *testing.T) {
	// Contract-derived shape (no on-disk sample in this repo).
	// Path embeds the model: /v1beta/models/<model>:<action>.
	respBody := `{
		"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"ok"}]}}],
		"usageMetadata":{
			"promptTokenCount":15,
			"candidatesTokenCount":3,
			"totalTokenCount":18,
			"cachedContentTokenCount":5
		}
	}`
	got := ExtractUsage(mkTrace(
		"/v1beta/models/gemini-2.0-flash:generateContent",
		"", respBody, nil,
	))

	strEq(t, "Model", got.Model, sp("gemini-2.0-flash"))
	strEq(t, "FinishReason", got.FinishReason, sp("STOP"))
	ptrEq(t, "PromptTokens", got.PromptTokens, p(15))
	ptrEq(t, "CompletionTokens", got.CompletionTokens, p(3))
	ptrEq(t, "TotalTokens", got.TotalTokens, p(18))
	ptrEq(t, "CachedTokens", got.CachedTokens, p(5))
	ptrEq(t, "ReasoningTokens", got.ReasoningTokens, nil)
}

func TestExtractUsage_Gemini_StreamGenerateContent(t *testing.T) {
	// The :streamGenerateContent surface still gets detected as gemini
	// and the model regex matches the same slot.
	respBody := `{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`
	got := ExtractUsage(mkTrace(
		"/v1beta/models/gemini-1.5-pro:streamGenerateContent",
		"", respBody, nil,
	))
	strEq(t, "Model", got.Model, sp("gemini-1.5-pro"))
	ptrEq(t, "TotalTokens", got.TotalTokens, p(3))
}

func TestExtractUsage_Gemini_MissingUsage(t *testing.T) {
	respBody := `{"candidates":[]}`
	got := ExtractUsage(mkTrace(
		"/v1beta/models/gemini-pro:generateContent",
		"", respBody, nil,
	))
	strEq(t, "Model", got.Model, sp("gemini-pro"))
	ptrEq(t, "PromptTokens", got.PromptTokens, nil)
	ptrEq(t, "TotalTokens", got.TotalTokens, nil)
}

// ----- Body_b64 fallback / general robustness ----------------------------

func TestExtractUsage_BodyB64Fallback_ReturnsZero(t *testing.T) {
	// When JSON parsing failed at finalize, t.Resp.Body is empty
	// (bytes live in BodyB64). ExtractUsage must NOT try to parse
	// body_b64 — parsed body is the only source of truth.
	for _, path := range []string{
		"/v1/chat/completions",
		"/v1/messages",
		"/v1/responses",
		"/v1beta/models/gemini-2.0-flash:generateContent",
	} {
		t.Run(path, func(t *testing.T) {
			tr := trace.Trace{Path: path}
			tr.Resp.BodyB64 = "c29tZS1yYXctYnl0ZXM=" // "some-raw-bytes"
			tr.Resp.ParseError = "invalid JSON"
			got := ExtractUsage(tr)
			ptrEq(t, "PromptTokens", got.PromptTokens, nil)
			ptrEq(t, "CompletionTokens", got.CompletionTokens, nil)
			ptrEq(t, "TotalTokens", got.TotalTokens, nil)
			ptrEq(t, "CachedTokens", got.CachedTokens, nil)
			// Gemini path embeds model -> regex still finds it; that's
			// fine, it's a named protocol field.
			if path != "/v1beta/models/gemini-2.0-flash:generateContent" {
				strEq(t, "Model", got.Model, nil)
			}
		})
	}
}

func TestExtractUsage_UnknownPath_ReturnsZero(t *testing.T) {
	got := ExtractUsage(mkTrace("/some/unknown/path", "", `{"usage":{"foo":1}}`, nil))
	if got.Model != nil || got.FinishReason != nil ||
		got.PromptTokens != nil || got.CompletionTokens != nil ||
		got.TotalTokens != nil || got.CachedTokens != nil ||
		got.CacheCreationTokens != nil || got.ReasoningTokens != nil {
		t.Errorf("unknown path must return zero UsageInfo, got %+v", got)
	}
}

func TestExtractUsage_EmptyTrace_NoPanic(t *testing.T) {
	// Defensive: an entirely empty trace must not panic.
	got := ExtractUsage(trace.Trace{})
	if got.Model != nil || got.PromptTokens != nil {
		t.Errorf("empty trace must return zero UsageInfo, got %+v", got)
	}
}

func TestExtractUsage_InvalidJSONInBody_NoPanic(t *testing.T) {
	// trace.Body.Body is json.RawMessage — if a caller hands us bytes
	// that aren't valid JSON we must swallow the error.
	tr := trace.Trace{Path: "/v1/chat/completions"}
	tr.Resp.Body = json.RawMessage(`{not valid json}`)
	got := ExtractUsage(tr)
	if got.PromptTokens != nil {
		t.Errorf("invalid JSON must yield nil tokens")
	}
}

func TestExtractUsage_ZeroVsAbsent(t *testing.T) {
	// PHILOSOPHY: nil ≠ zero. A protocol that reports zero tokens must
	// surface as *int64 = &0, not nil.
	respBody := `{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`
	got := ExtractUsage(mkTrace("/v1/chat/completions", "", respBody, nil))
	if got.PromptTokens == nil || *got.PromptTokens != 0 {
		t.Errorf("explicit zero must round-trip as &0, got %v", got.PromptTokens)
	}
	if got.TotalTokens == nil || *got.TotalTokens != 0 {
		t.Errorf("explicit zero total must round-trip as &0, got %v", got.TotalTokens)
	}
}

func TestExtractUsage_ProvidedTotalNotOverwrittenByComputed(t *testing.T) {
	// Synthetic: protocol reports a total that disagrees with
	// prompt+completion. We must keep the PROVIDED total verbatim —
	// PHILOSOPHY §1 forbids synthesizing over named fields.
	respBody := `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":99}}`
	got := ExtractUsage(mkTrace("/v1/chat/completions", "", respBody, nil))
	ptrEq(t, "TotalTokens (provided wins)", got.TotalTokens, p(99))
}
