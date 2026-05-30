package v2

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ParsedRequestFromHTTPRequest builds a ParsedRequest from a raw inbound
// *http.Request and its already-read body. The hot-path equivalent of
// ParsedRequestFromTrace (which builds from a finalized trace.Trace).
//
// The caller MUST have buffered req.Body BEFORE calling this — Body has
// already been consumed by capture or by the caller's read. body is the
// raw JSON the parser saw.
//
// Streaming is inferred from the request body's `stream` field (Chat /
// Messages / Responses) — the inbound HTTP request itself does not
// carry the streaming bit at the HTTP layer.
//
// Headers are NOT cloned here — the caller passes a snapshot it owns.
// SystemPrompt / Model / Messages / Tools land from the per-protocol
// parser dispatchers shared with the trace builder.
func ParsedRequestFromHTTPRequest(req *http.Request, body []byte) ParsedRequest {
	if req == nil {
		return ParsedRequest{}
	}
	pr := ParsedRequest{
		Protocol: DetectProtocol(req.URL.Path),
		Path:     req.URL.RequestURI(),
		Method:   req.Method,
		Headers:  cloneHeader(req.Header),
		RawBody:  json.RawMessage(append([]byte(nil), body...)),
	}
	if len(body) == 0 {
		return pr
	}
	switch pr.Protocol {
	case ProtocolChat:
		parseChatRequest(pr.RawBody, &pr)
	case ProtocolMessages:
		parseMessagesRequest(pr.RawBody, &pr)
	case ProtocolResponses:
		parseResponsesRequest(pr.RawBody, &pr)
	case ProtocolGemini:
		parseGeminiRequest(pr.RawBody, &pr)
	}
	// Anthropic streaming is also signaled by the `stream: true` body
	// field which parseMessagesRequest already lifts. Headers like
	// `accept: text/event-stream` are NOT consulted — the body field is
	// the source of truth for Streaming.
	return pr
}

// ParsedResponseFromBody builds a ParsedResponse from a non-streaming
// upstream response body. For streaming responses the AFTER-hook
// machinery dispatches per-event through StreamDispatcher; this builder
// is the buffered-branch counterpart.
//
// status / headers come from the *http.Response; body is the bytes the
// caller read. The protocol discriminator is sourced from the request
// (not the response) — the response carries no path of its own.
func ParsedResponseFromBody(reqProto Protocol, status int, headers http.Header, body []byte) ParsedResponse {
	resp := ParsedResponse{
		Protocol: reqProto,
		Status:   status,
		Headers:  cloneHeader(headers),
		RawBody:  json.RawMessage(append([]byte(nil), body...)),
	}
	if len(body) == 0 {
		return resp
	}
	// Build a minimal trace-shaped intermediary so the existing per-
	// protocol response parsers (which take trace.Trace) can be reused
	// without duplicating ~300 LOC. Tools and content sit on the body
	// payload alone for non-streaming responses; Events is empty.
	switch reqProto {
	case ProtocolChat:
		parseChatResponseRaw(body, &resp)
	case ProtocolMessages:
		parseMessagesResponseRaw(body, &resp)
	case ProtocolResponses:
		parseResponsesResponseRaw(body, &resp)
	case ProtocolGemini:
		parseGeminiResponseRaw(body, &resp)
	}
	return resp
}

// IsSSEContentType returns true when the header value indicates an SSE
// stream — anything starting with "text/event-stream", case-insensitive.
// The AFTER-hook bridge in main.go uses this to pick streaming vs
// buffered handling.
func IsSSEContentType(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream")
}

// --- non-streaming response parsers (raw-body variants) ---
//
// The trace-based parseChatResponse etc. dispatch on t.Resp.Body vs
// t.Resp.Events; for the live AFTER hook in the non-streaming branch
// we only ever have raw body bytes. Extract the body-only halves so we
// can call them without synthesizing a trace.

func parseChatResponseRaw(body []byte, resp *ParsedResponse) {
	if len(body) == 0 {
		return
	}
	var shape chatRespShape
	if err := json.Unmarshal(body, &shape); err != nil || len(shape.Choices) == 0 {
		return
	}
	c := shape.Choices[0]
	if c.Message == nil {
		return
	}
	resp.Content = chatExtractText(c.Message.Content)
	for _, tc := range c.Message.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
}

func parseMessagesResponseRaw(body []byte, resp *ParsedResponse) {
	if len(body) == 0 {
		return
	}
	var shape messagesRespShape
	if err := json.Unmarshal(body, &shape); err != nil {
		return
	}
	collectMessagesBlocks(shape.Content, resp)
}

func parseResponsesResponseRaw(body []byte, resp *ParsedResponse) {
	if len(body) == 0 {
		return
	}
	var shape struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return
	}
	collectResponsesOutput(shape.Output, resp)
}

func parseGeminiResponseRaw(body []byte, resp *ParsedResponse) {
	if len(body) == 0 {
		return
	}
	var shape struct {
		Candidates []struct {
			Content *geminiReqContent `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return
	}
	var content strings.Builder
	for _, c := range shape.Candidates {
		if c.Content == nil {
			continue
		}
		for _, cp := range geminiParseParts(c.Content.Parts) {
			switch cp.Type {
			case "text":
				content.WriteString(cp.Text)
			case "tool_use":
				if cp.ToolUse != nil {
					resp.ToolCalls = append(resp.ToolCalls, ToolCall{
						Name:      cp.ToolUse.Name,
						Arguments: string(cp.ToolUse.Input),
					})
				}
			}
		}
	}
	resp.Content = content.String()
}
