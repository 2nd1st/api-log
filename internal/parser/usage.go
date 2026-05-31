// Usage extraction (T3). Pulls token / model / finish-reason fields out
// of a finalized trace.Trace and returns them as UsageInfo.
//
// This is PHILOSOPHY §1 carve-out 1: a deterministic copy of NAMED
// protocol fields, no synthesis, no heuristic sniffing. The only
// "computed" field is TotalTokens, which falls back to
// PromptTokens+CompletionTokens when (and only when) the protocol
// itself did not provide a total — the comment on the struct says
// "provided or computed."
//
// PHILOSOPHY §2: this function MUST NOT panic and MUST NOT block. All
// missing-field branches return nil pointers; all unmarshal errors are
// swallowed; the writer logs WARN at the call site.
//
// PHILOSOPHY §6: SQLite columns derived from this output are
// rebuildable from JSONL by replaying ExtractUsage. That means the
// implementation must read whichever named shape the on-disk trace
// actually carries — for the OpenAI Responses protocol that is BOTH
// streaming (resp.events[-1]) and non-streaming (resp.body) shapes;
// otherwise a replay of a non-streaming /v1/responses trace would
// silently drop real named usage fields.
//
// PHILOSOPHY §7: protocol surface is small and slow — only the four
// protocols listed below. Unknown paths return zero UsageInfo. Adding
// a new protocol is a separate piece of work.
//
// Scope note (streaming chat / messages): the investigation contract
// names BODY field paths for chat / messages / gemini. The Anthropic
// streaming protocol spreads usage across two SSE frames (message_start
// carries input + cache fields; message_delta carries output_tokens),
// and streaming OpenAI Chat usage lives on a final chunk; neither has
// a single named container the contract pins down. Per §1 we do not
// invent paths the contract omits, so streaming chat/messages traces
// return nil token fields. /v1/responses is the lone exception because
// its terminal `response.completed` event carries a complete single-
// location usage block — see extractResponses below.
package parser

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/xiayangzhang/api-log/internal/trace"
)

// UsageInfo is the contract for usage extraction. Each field is a pointer
// because absence (nil) is distinct from zero. Consumers treat nil as
// "the protocol does not define this field" or "the field was absent in
// the actual response."
type UsageInfo struct {
	Model               *string // protocol-extracted model name
	FinishReason        *string // protocol finish_reason / stop_reason / status
	PromptTokens        *int64  // input_tokens / prompt_tokens
	CompletionTokens    *int64  // output_tokens / completion_tokens
	TotalTokens         *int64  // provided or computed as prompt + completion
	CachedTokens        *int64  // sum of cache hits (cache_read for Anthropic, cached_tokens for OpenAI)
	CacheCreationTokens *int64  // Anthropic-only: cache_creation_input_tokens (input cache misses for storage)
	ReasoningTokens     *int64  // OpenAI Responses only: output_tokens_details.reasoning_tokens
}

// ExtractUsage parses usage fields from a Trace. Protocol is detected
// from t.Path (mirrors the viewer's detectProtocol logic in
// api-log-viewer/src/lib/adapters/index.ts — substring match on
// /v1/chat/completions, /v1/messages, /v1/responses, /v1beta/ or
// :generateContent).
//
// Returns a UsageInfo with all fields populated where the protocol
// includes them. Nil pointers indicate either "this protocol does not
// define this field" or "the field was absent in this trace."
//
// Body_b64 fallback: when JSON parsing failed at finalize, t.Resp.Body
// is empty (the bytes live in t.Resp.BodyB64). This function ignores
// body_b64 entirely — the parsed body is the source of truth, and if
// parsing failed there is no usage to extract.
func ExtractUsage(t trace.Trace) UsageInfo {
	switch detectProtocol(t.Path) {
	case protoChat:
		return extractChat(t)
	case protoMessages:
		return extractMessages(t)
	case protoResponses:
		return extractResponses(t)
	case protoGemini:
		return extractGemini(t)
	default:
		return UsageInfo{}
	}
}

// --- protocol detection ---------------------------------------------

type protocol int

const (
	protoUnknown protocol = iota
	protoChat
	protoMessages
	protoResponses
	protoGemini
)

func detectProtocol(path string) protocol {
	switch {
	case strings.Contains(path, "/v1/chat/completions"):
		return protoChat
	case strings.Contains(path, "/v1/messages"):
		return protoMessages
	case strings.Contains(path, "/v1/responses"):
		return protoResponses
	case strings.Contains(path, ":generateContent"),
		strings.Contains(path, ":streamGenerateContent"),
		strings.Contains(path, "/v1beta/"):
		return protoGemini
	default:
		return protoUnknown
	}
}

// --- OpenAI Chat Completions ----------------------------------------
//
// Named fields:
//   model        : resp.body.model (fallback: req.body.model)
//   prompt       : resp.body.usage.prompt_tokens
//   completion   : resp.body.usage.completion_tokens
//   total        : resp.body.usage.total_tokens (provided)
//   cached       : resp.body.usage.prompt_tokens_details.cached_tokens
//   reasoning    : resp.body.usage.completion_tokens_details.reasoning_tokens
//   finish       : resp.body.choices[0].finish_reason
//
// Streaming chat completions usually do NOT carry usage in the events
// (per the on-disk sample in deploy/dev-stack/data) — that's a body-
// only shape per the contract, so no event scan is added here.

type chatUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	TotalTokens         *int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type chatChoice struct {
	FinishReason *string `json:"finish_reason"`
}

type chatBody struct {
	Model   *string      `json:"model"`
	Usage   *chatUsage   `json:"usage"`
	Choices []chatChoice `json:"choices"`
}

type chatReqBody struct {
	Model *string `json:"model"`
}

func extractChat(t trace.Trace) UsageInfo {
	var out UsageInfo

	var resp chatBody
	if len(t.Resp.Body) > 0 {
		_ = json.Unmarshal(t.Resp.Body, &resp)
	}

	out.Model = resp.Model
	// Fallback: req.body.model.
	if out.Model == nil && len(t.Req.Body) > 0 {
		var req chatReqBody
		if json.Unmarshal(t.Req.Body, &req) == nil {
			out.Model = req.Model
		}
	}

	if resp.Usage != nil {
		out.PromptTokens = resp.Usage.PromptTokens
		out.CompletionTokens = resp.Usage.CompletionTokens
		out.TotalTokens = resp.Usage.TotalTokens
		if resp.Usage.PromptTokensDetails != nil {
			out.CachedTokens = resp.Usage.PromptTokensDetails.CachedTokens
		}
		if resp.Usage.CompletionTokensDetails != nil {
			out.ReasoningTokens = resp.Usage.CompletionTokensDetails.ReasoningTokens
		}
	}

	if len(resp.Choices) > 0 {
		out.FinishReason = resp.Choices[0].FinishReason
	}

	out.TotalTokens = computeTotal(out.TotalTokens, out.PromptTokens, out.CompletionTokens)
	return out
}

// --- Anthropic Messages ---------------------------------------------
//
// Two on-disk shapes, both covered (PHILOSOPHY §6 — same extractor must
// rebuild SQLite from any recorded trace, regardless of streaming vs
// non-streaming):
//
//   NON-STREAMING (resp.body present):
//     model      : resp.body.model
//     prompt     : resp.body.usage.input_tokens
//     completion : resp.body.usage.output_tokens
//     cached     : resp.body.usage.cache_read_input_tokens
//     cacheCre.  : resp.body.usage.cache_creation_input_tokens (Anthropic-only)
//     finish     : resp.body.stop_reason
//
//   STREAMING (resp.events present):
//     model      : first message_start.data.message.model
//     prompt     : first message_start.data.message.usage.input_tokens
//     cached     : first message_start.data.message.usage.cache_read_input_tokens
//     cacheCre.  : first message_start.data.message.usage.cache_creation_input_tokens
//     completion : LAST message_delta.data.usage.output_tokens
//     finish     : LAST message_delta.data.delta.stop_reason
//
//     The streaming usage block is split across two event kinds by
//     protocol design. message_start carries the canonical input-side
//     counts; subsequent message_delta events emit cumulative output_tokens
//     and (eventually) the stop_reason. message_stop carries no usage.
//
//     This is *not* synthesis — every field above is a named protocol
//     field whose path is fixed by the Anthropic Messages spec.
//     PHILOSOPHY §1 carve-out 1 covers it because the on-the-wire trace
//     in SSE form has these names verbatim; the contract just chooses
//     which event to read each named field from.
//
// total is not provided by protocol — computed from prompt + completion
// when both are present. reasoning_tokens is not provided by protocol.

type messagesUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

type messagesBody struct {
	Model      *string        `json:"model"`
	Usage      *messagesUsage `json:"usage"`
	StopReason *string        `json:"stop_reason"`
}

// messagesStartEventData mirrors the `data` block of a message_start SSE
// event:
//
//	{"type":"message_start","message":{...,"model":"...","usage":{...}}}
type messagesStartEventData struct {
	Message *messagesBody `json:"message"`
}

// messagesDeltaEventData mirrors the `data` block of a message_delta SSE
// event:
//
//	{"type":"message_delta","delta":{"stop_reason":"..."},"usage":{"output_tokens":N,...}}
//
// Note: message_delta's `usage.input_tokens` is a *delta* (not cumulative)
// per protocol — we deliberately ignore it and take input_tokens from
// message_start only.
type messagesDeltaEventData struct {
	Delta *struct {
		StopReason *string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens *int64 `json:"output_tokens"`
	} `json:"usage"`
}

func extractMessages(t trace.Trace) UsageInfo {
	var out UsageInfo

	// Non-streaming shape.
	if len(t.Resp.Body) > 0 {
		var resp messagesBody
		if err := json.Unmarshal(t.Resp.Body, &resp); err != nil {
			return out
		}
		out.Model = resp.Model
		out.FinishReason = resp.StopReason
		if resp.Usage != nil {
			out.PromptTokens = resp.Usage.InputTokens
			out.CompletionTokens = resp.Usage.OutputTokens
			out.CachedTokens = resp.Usage.CacheReadInputTokens
			out.CacheCreationTokens = resp.Usage.CacheCreationInputTokens
		}
		out.TotalTokens = computeTotal(nil, out.PromptTokens, out.CompletionTokens)
		return out
	}

	// Streaming shape: walk events. Single O(N) pass that captures:
	//   - the first message_start (model + prompt-side usage)
	//   - the most recent message_delta (stop_reason + cumulative
	//     output_tokens — Anthropic emits multiple delta events as the
	//     stream proceeds; the last one carries the final counts).
	if len(t.Resp.Events) == 0 {
		return out
	}
	for _, ev := range t.Resp.Events {
		if len(ev.Data) == 0 {
			continue
		}
		switch ev.Name {
		case "message_start":
			if out.Model != nil {
				continue // first wins
			}
			var data messagesStartEventData
			if json.Unmarshal(ev.Data, &data) != nil || data.Message == nil {
				continue
			}
			out.Model = data.Message.Model
			if u := data.Message.Usage; u != nil {
				out.PromptTokens = u.InputTokens
				out.CachedTokens = u.CacheReadInputTokens
				out.CacheCreationTokens = u.CacheCreationInputTokens
			}
		case "message_delta":
			var data messagesDeltaEventData
			if json.Unmarshal(ev.Data, &data) != nil {
				continue
			}
			if data.Delta != nil && data.Delta.StopReason != nil {
				out.FinishReason = data.Delta.StopReason
			}
			if data.Usage != nil && data.Usage.OutputTokens != nil {
				out.CompletionTokens = data.Usage.OutputTokens
			}
		}
	}
	out.TotalTokens = computeTotal(nil, out.PromptTokens, out.CompletionTokens)
	return out
}

// --- OpenAI Responses -----------------------------------------------
//
// The /v1/responses protocol arrives in TWO shapes on disk and we read
// BOTH (PHILOSOPHY §6 — same extractor must rebuild SQLite from any
// recorded trace, regardless of streaming vs non-streaming):
//
//   STREAMING (resp.events present):
//     The terminal `response.completed` SSE frame carries
//     `data.response.usage` — note the `.response` wrapper around the
//     usage block. We take resp.events[-1] (the last event) verbatim;
//     if it lacks the nested shape (mid-stream cut), fields stay nil.
//
//   NON-STREAMING (resp.body present):
//     `resp.body.usage.*` lives at the top level, no `.response`
//     wrapper. Status is at `resp.body.status`.
//
// Per trace.Body's contract exactly one of Body/Events is set. We
// dispatch on which is non-empty.
//
// Named fields:
//   model        : req.body.model (no fallback — /v1/responses path
//                  carries no model, so a path regex would never
//                  match and we don't add a synthetic one)
//   prompt       : usage.input_tokens
//   completion   : usage.output_tokens
//   total        : usage.total_tokens (provided)
//   cached       : usage.input_tokens_details.cached_tokens
//   reasoning    : usage.output_tokens_details.reasoning_tokens
//   finish       : status (verbatim — usually "completed", but
//                  "incomplete" / "failed" are also legal)

type responsesUsage struct {
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	TotalTokens        *int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// responsesBodyOrEnvelope is the union of:
//   - non-streaming body shape: { model, status, usage }
//   - streaming event.data shape:  { type, response: { model, status, usage } }
//
// We try the non-streaming shape first (top-level usage); if absent,
// we fall through to the streaming shape (data.response.usage).
type responsesBody struct {
	Model  *string         `json:"model"`
	Status *string         `json:"status"`
	Usage  *responsesUsage `json:"usage"`
}

type responsesEventData struct {
	Response *responsesBody `json:"response"`
}

type responsesReqBody struct {
	Model *string `json:"model"`
}

func extractResponses(t trace.Trace) UsageInfo {
	var out UsageInfo

	// Model comes from request body. No path regex fallback — the
	// /v1/responses path does not embed the model name.
	if len(t.Req.Body) > 0 {
		var req responsesReqBody
		if json.Unmarshal(t.Req.Body, &req) == nil {
			out.Model = req.Model
		}
	}

	// Non-streaming body shape: top-level usage.
	if len(t.Resp.Body) > 0 {
		var body responsesBody
		if json.Unmarshal(t.Resp.Body, &body) == nil {
			fillFromResponsesBody(&out, &body)
		}
		out.TotalTokens = computeTotal(out.TotalTokens, out.PromptTokens, out.CompletionTokens)
		return out
	}

	// Streaming shape: last event's data.response.{model,status,usage}.
	// resp.events[-1] is the literal last frame; if the stream was cut
	// mid-flight and the last frame is not response.completed, the
	// nested usage will be absent and fields stay nil.
	if n := len(t.Resp.Events); n > 0 {
		last := t.Resp.Events[n-1]
		if len(last.Data) > 0 {
			var ev responsesEventData
			if json.Unmarshal(last.Data, &ev) == nil && ev.Response != nil {
				fillFromResponsesBody(&out, ev.Response)
			}
		}
	}
	out.TotalTokens = computeTotal(out.TotalTokens, out.PromptTokens, out.CompletionTokens)
	return out
}

// fillFromResponsesBody copies fields from a parsed responsesBody (the
// inner usage block + status + finish reason) into UsageInfo.
//
// NOTE: model is intentionally NOT copied from the response body here.
// The contract pins responses.Model to req.body.model. In real OpenAI
// traffic the response echoes the RESOLVED model (e.g. request
// "gpt-4o" → response "gpt-4o-2024-11-20"); overwriting from the body
// would silently surface a different string than the contract names.
// extractResponses sets Model from the request body before calling
// this helper.
func fillFromResponsesBody(out *UsageInfo, body *responsesBody) {
	if body == nil {
		return
	}
	out.FinishReason = body.Status
	if body.Usage != nil {
		out.PromptTokens = body.Usage.InputTokens
		out.CompletionTokens = body.Usage.OutputTokens
		out.TotalTokens = body.Usage.TotalTokens
		if body.Usage.InputTokensDetails != nil {
			out.CachedTokens = body.Usage.InputTokensDetails.CachedTokens
		}
		if body.Usage.OutputTokensDetails != nil {
			out.ReasoningTokens = body.Usage.OutputTokensDetails.ReasoningTokens
		}
	}
}

// --- Google Gemini --------------------------------------------------
//
// Named fields:
//   model        : path regex /models/([^/:]+) — gemini paths look
//                  like /v1beta/models/gemini-2.0-flash:generateContent
//   prompt       : resp.body.usageMetadata.promptTokenCount
//   completion   : resp.body.usageMetadata.candidatesTokenCount
//   total        : resp.body.usageMetadata.totalTokenCount (provided)
//   cached       : resp.body.usageMetadata.cachedContentTokenCount
//   reasoning    : not provided by protocol
//   finish       : resp.body.candidates[0].finishReason
//
// Gemini SSE shape is left out: the contract names body fields only,
// and no on-disk gemini sample exists in this repo to validate an
// event variant — adding one would be synthesis (§1 violation).

type geminiUsageMetadata struct {
	PromptTokenCount        *int64 `json:"promptTokenCount"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
	TotalTokenCount         *int64 `json:"totalTokenCount"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`
}

type geminiCandidate struct {
	FinishReason *string `json:"finishReason"`
}

type geminiBody struct {
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
	Candidates    []geminiCandidate    `json:"candidates"`
}

// geminiModelRe matches the model segment of a Gemini path. We
// deliberately match `/models/` rather than `/v1/models/` because the
// real surface is `/v1beta/models/<name>:<action>` (and v1 hasn't been
// observed for the generateContent surface).
var geminiModelRe = regexp.MustCompile(`/models/([^/:]+)`)

func extractGemini(t trace.Trace) UsageInfo {
	var out UsageInfo

	if m := geminiModelRe.FindStringSubmatch(t.Path); len(m) == 2 {
		s := m[1]
		out.Model = &s
	}

	if len(t.Resp.Body) > 0 {
		var resp geminiBody
		if json.Unmarshal(t.Resp.Body, &resp) == nil {
			if resp.UsageMetadata != nil {
				out.PromptTokens = resp.UsageMetadata.PromptTokenCount
				out.CompletionTokens = resp.UsageMetadata.CandidatesTokenCount
				out.TotalTokens = resp.UsageMetadata.TotalTokenCount
				out.CachedTokens = resp.UsageMetadata.CachedContentTokenCount
			}
			if len(resp.Candidates) > 0 {
				out.FinishReason = resp.Candidates[0].FinishReason
			}
		}
	}

	out.TotalTokens = computeTotal(out.TotalTokens, out.PromptTokens, out.CompletionTokens)
	return out
}

// --- shared helpers -------------------------------------------------

// computeTotal returns provided when non-nil; otherwise returns
// prompt+completion when BOTH are non-nil; otherwise nil. Per PHILOSOPHY
// §1 carve-out 1 the only synthesis we allow is the documented "provided
// or computed" total — and only when both inputs are present so the
// result is exact, not a guess.
func computeTotal(provided, prompt, completion *int64) *int64 {
	if provided != nil {
		return provided
	}
	if prompt == nil || completion == nil {
		return nil
	}
	sum := *prompt + *completion
	return &sum
}
