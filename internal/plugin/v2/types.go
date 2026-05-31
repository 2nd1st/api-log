package v2

import (
	"encoding/json"
	"net/http"

	"github.com/2nd1st/api-log/internal/parser"
	"github.com/2nd1st/api-log/internal/sse"
)

// Action is the verdict a plugin returns from a hook call.
//
// See spec §2.2 for the contract; see ActionXxx constants below for the
// per-action semantics.
type Action int

const (
	// ActionContinue means "I have nothing to do." The next plugin sees
	// the same parsed object the framework handed me.
	ActionContinue Action = iota

	// ActionMutate means "I produced a replacement." The framework
	// replaces the parsed object (request on BEFORE, response on AFTER
	// non-streaming) and passes it to the next plugin.
	ActionMutate

	// ActionIntercept means "I produced the final response." The
	// framework short-circuits the rest of the pipeline:
	//   - BEFORE intercept: skip upstream forwarding; serve the intercept
	//     body to the client. The full AFTER chain still runs on the
	//     synthesized response so decorators (watermark, etc.) still
	//     get a chance.
	//   - AFTER  intercept: remaining AFTER plugins are skipped; the
	//     intercept body replaces what gets streamed to the client.
	ActionIntercept
)

// Protocol is the parser package's protocol discriminator surfaced to
// plugins. Plugins switch on this to decide whether the shape is one
// they understand.
type Protocol int

const (
	ProtocolUnknown   Protocol = iota
	ProtocolChat               // OpenAI /v1/chat/completions
	ProtocolMessages           // Anthropic /v1/messages
	ProtocolResponses          // OpenAI /v1/responses
	ProtocolGemini             // Google /v1beta/.../:(stream)generateContent
)

// String makes Protocol log-friendly. Stable strings; do not change.
func (p Protocol) String() string {
	switch p {
	case ProtocolChat:
		return "chat"
	case ProtocolMessages:
		return "messages"
	case ProtocolResponses:
		return "responses"
	case ProtocolGemini:
		return "gemini"
	default:
		return "unknown"
	}
}

// Message is the normalized turn shape. Built by the request-parser pass
// that runs BEFORE the hook chain; identical across all four protocols.
//
// "Normalized" means the parser has:
//   - lifted system prompts into a separate field (ParsedRequest.SystemPrompt),
//     so Messages contains only user / assistant / tool turns
//   - flattened multi-part content blocks into one ordered slice of
//     ContentPart per turn; text parts are exposed as ContentPart{Type:"text"}
//   - kept tool calls / tool results as their own ContentPart kinds
type Message struct {
	// Role is "user" | "assistant" | "tool". System role is lifted out
	// to ParsedRequest.SystemPrompt and never appears here.
	Role string

	// Content is the ordered list of parts for this turn. A text-only
	// user turn is a single ContentPart{Type:"text"}. Multi-modal
	// content (image + text + image) keeps wire order.
	Content []ContentPart

	// Name is the tool name when Role == "tool" (Chat protocol). Empty
	// otherwise.
	Name string

	// ToolCallID is the matching call id when Role == "tool". Empty
	// otherwise.
	ToolCallID string
}

// ContentPart is one fragment of a Message. Type tells the consumer
// which sibling field carries the payload; unknown types preserve their
// original JSON in Raw so a plugin can pass them through unchanged.
type ContentPart struct {
	// Type is one of: "text" | "image" | "audio" | "tool_use" | "tool_result".
	// Stable strings; v1 sees no others.
	Type string

	// Text is set when Type == "text".
	Text string

	// MediaType is the MIME of an inline image/audio part, e.g.
	// "image/png" or "audio/mpeg". Empty when the part is URL-only.
	MediaType string

	// DataB64 is base64-encoded bytes for inline media. Empty when
	// the part is URL-only.
	DataB64 string

	// URL is set when the media part references an external URL.
	URL string

	// ToolUse is set when Type == "tool_use" (assistant requests a tool).
	ToolUse *ToolUse

	// ToolResult is set when Type == "tool_result" (Anthropic shape:
	// the tool result lives in the message stream).
	ToolResult *ToolResult

	// Raw is the original part JSON, preserved verbatim. Plugins that
	// want to inspect a part the v1 normalizer didn't decode can read
	// from here; mutating Raw without also updating the typed fields is
	// undefined behavior (the builder uses the typed fields).
	Raw json.RawMessage
}

// Tool is one tool definition the client offered to the model.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON Schema for the tool's arguments object.
	Schema json.RawMessage
	// Raw is the original tool definition JSON (escape hatch for
	// protocol-specific fields the normalizer didn't lift).
	Raw json.RawMessage
}

// ToolUse is an assistant-side request to call a tool. Lives on a
// Message ContentPart with Type == "tool_use".
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is the result of a tool invocation, sent back as a user-
// or tool-role message in the next turn.
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// ToolCall is a structured tool invocation on the response side
// (post-stream buffered view). Distinct from ToolUse, which is the
// request-side message-part view.
//
// AFTER plugins may READ ToolCall for inspection in v1, but mutating
// it is undefined behavior — the dispatcher does not re-emit modified
// tool-call arguments. See spec §10.6.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ParsedRequest is what BEFORE plugins (and AFTER plugins, as the
// request side) receive. Built from the raw request body by the parser
// once per request; reused across the entire chain.
//
// Plugins MUST treat ParsedRequest as logically read-only and return
// a Mutated copy when they want to change something — the proxy may
// reuse the underlying buffer for retry/logging.
type ParsedRequest struct {
	Protocol     Protocol
	Path         string      // e.g. "/v1/chat/completions"
	Method       string      // typically "POST"
	Model        string      // copied from req body
	Messages     []Message   // normalized turns; no system role
	Tools        []Tool      // tool definitions
	SystemPrompt string      // first system role text, computed
	Headers      http.Header // a defensive copy; safe to mutate
	ClientIP     string      // smart-XFF resolved
	KeyHash      string      // matches the trace's KeyHash
	// Streaming is true when the client requested a streaming response
	// (e.g. {"stream": true} body field, or Anthropic SSE accept).
	// Plugins inspect this to know whether AfterContext exposes the
	// streaming callbacks or the buffered Response.
	Streaming bool
	// RawBody is the original request JSON. The build path overlays the
	// typed fields (Model, Messages, Tools, SystemPrompt) onto a map
	// decoded from RawBody, so unknown fields (temperature,
	// response_format, custom keys) survive the roundtrip. Plugins
	// MUST leave RawBody intact when they mutate typed fields; clearing
	// it drops every field the v1 normalizer does not lift.
	RawBody json.RawMessage
}

// ParsedResponse is what AFTER plugins receive in the non-streaming
// branch. Built once after the upstream response is fully read.
//
// For streaming responses the framework hands plugins an AfterContext
// (see hook.go) with per-event semantic callbacks; ParsedResponse is
// nil in that branch until W1+W3 wire the buffered-streaming path.
type ParsedResponse struct {
	Protocol  Protocol
	Status    int    // upstream HTTP status; 0 when intercepted before forward
	Content   string // concatenated assistant text content
	Reasoning string // concatenated reasoning text (Anthropic thinking, etc.)
	ToolCalls []ToolCall
	Headers   http.Header
	RawBody   json.RawMessage  // non-streaming case
	Events    []sse.Event      // streaming case (buffered post-stream)
	Usage     parser.UsageInfo // reuses parser's pointer-field shape
}

// InterceptResponse is what a plugin returns when it bypasses the rest
// of the pipeline. The proxy serializes Body directly to the client
// (after the AFTER chain runs, in the BEFORE-intercept case). The
// plugin chooses Status (200, 4xx, 5xx — any HTTP status is legal).
//
// Headers is merged into the response headers; nil is allowed. Body
// is served as-is; the plugin owns its Content-Type header.
type InterceptResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// BeforeResult is the return value of BeforePlugin.OnBefore.
//
// Field meaning by Action:
//   - ActionContinue : Mutated and Intercept are nil; framework keeps
//     the incoming request.
//   - ActionMutate   : Mutated is the replacement request; Intercept
//     is nil. If Mutated is nil despite ActionMutate, the framework
//     treats it as ActionContinue (defensive; logged at WARN).
//   - ActionIntercept: Intercept is the synthesized response; Mutated
//     is nil. If Intercept is nil despite ActionIntercept, the
//     framework treats it as ActionContinue (defensive).
type BeforeResult struct {
	Action    Action
	Mutated   *ParsedRequest
	Intercept *InterceptResponse
}

// AfterResult is the return value of AfterPlugin.OnAfter.
//
// In the streaming branch the plugin typically returns
// {Action: ActionContinue} after registering its callbacks on the
// AfterContext (see hook.go); the dispatcher applies the registered
// transforms as events flow. Mutated has no effect in the streaming
// branch — there is no buffered response to replace.
//
// In the non-streaming branch the plugin returns {Action: ActionMutate,
// Mutated: <new ParsedResponse>} to replace the buffered response.
//
// Intercept works in both branches: the framework drops what it has and
// serves the intercept body instead.
type AfterResult struct {
	Action    Action
	Mutated   *ParsedResponse
	Intercept *InterceptResponse
}
