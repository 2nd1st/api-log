package v2

import (
	"encoding/json"
	"fmt"
)

// BuildRequestBody serializes a ParsedRequest back to raw JSON for
// the forward path. Used when a BEFORE plugin returns ActionMutate
// and the framework needs to forward the rewritten request upstream.
//
// Per-protocol writers, mirroring the parsed.go dispatchers. Gemini
// is deferred (less streaming demand for v1; the request shape is
// preserved verbatim from RawBody when no Mutate landed).
//
// Equivalence target: the output JSON when round-tripped (Unmarshal
// to map[string]any) is semantically equal to the input for fields v1
// normalizes. Bytes are NOT promised equal — field ordering, optional
// whitespace, and shapes the normalizer does not lift are not
// reconstructed.
func BuildRequestBody(req *ParsedRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("v2.BuildRequestBody: nil request")
	}
	switch req.Protocol {
	case ProtocolChat:
		return buildChatRequest(req)
	case ProtocolMessages:
		return buildMessagesRequest(req)
	case ProtocolResponses:
		return buildResponsesRequest(req)
	case ProtocolGemini:
		// Gemini build deferred — return raw body verbatim when present
		// so the forward path still works; plugins that mutate Gemini
		// requests get an explicit error so they don't silently fail.
		if len(req.RawBody) > 0 {
			return append([]byte(nil), req.RawBody...), nil
		}
		return nil, fmt.Errorf("v2.BuildRequestBody: gemini build deferred (W1)")
	default:
		return nil, fmt.Errorf("v2.BuildRequestBody: unknown protocol %s", req.Protocol)
	}
}

// BuildResponseBody serializes a ParsedResponse back to raw JSON.
// Used in the non-streaming AFTER-mutation path. Streaming responses
// are mutated event-by-event through the stream dispatcher and do not
// go through this function.
func BuildResponseBody(resp *ParsedResponse) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("v2.BuildResponseBody: nil response")
	}
	switch resp.Protocol {
	case ProtocolChat:
		return buildChatResponse(resp)
	case ProtocolMessages:
		return buildMessagesResponse(resp)
	case ProtocolResponses:
		return buildResponsesResponse(resp)
	case ProtocolGemini:
		if len(resp.RawBody) > 0 {
			return append([]byte(nil), resp.RawBody...), nil
		}
		return nil, fmt.Errorf("v2.BuildResponseBody: gemini build deferred (W1)")
	default:
		return nil, fmt.Errorf("v2.BuildResponseBody: unknown protocol %s", resp.Protocol)
	}
}

// --- OpenAI Chat ----------------------------------------------------

func buildChatRequest(req *ParsedRequest) ([]byte, error) {
	// Start from RawBody so we preserve fields v1 does not lift
	// (temperature, top_p, response_format, etc.).
	base := mapFromRaw(req.RawBody)
	base["model"] = req.Model
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		msgs = append(msgs, map[string]any{
			"role":    "system",
			"content": req.SystemPrompt,
		})
	}
	for _, m := range req.Messages {
		entry := map[string]any{"role": m.Role}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		text, parts, tcs := splitChatParts(m.Content)
		switch {
		case len(parts) > 0:
			entry["content"] = parts
		case text != "":
			entry["content"] = text
		default:
			entry["content"] = ""
		}
		if len(tcs) > 0 {
			entry["tool_calls"] = tcs
		}
		msgs = append(msgs, entry)
	}
	base["messages"] = msgs
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  rawOrNull(t.Schema),
				},
			})
		}
		base["tools"] = tools
	}
	return json.Marshal(base)
}

func splitChatParts(parts []ContentPart) (text string, arr []map[string]any, toolCalls []map[string]any) {
	hasNonText := false
	for _, p := range parts {
		if p.Type != "text" && p.Type != "tool_use" {
			hasNonText = true
			break
		}
	}
	if !hasNonText {
		// Single-string content path.
		for _, p := range parts {
			switch p.Type {
			case "text":
				text += p.Text
			case "tool_use":
				if p.ToolUse != nil {
					toolCalls = append(toolCalls, map[string]any{
						"id":   p.ToolUse.ID,
						"type": "function",
						"function": map[string]any{
							"name":      p.ToolUse.Name,
							"arguments": string(p.ToolUse.Input),
						},
					})
				}
			}
		}
		return text, nil, toolCalls
	}
	// Multi-part array path.
	for _, p := range parts {
		switch p.Type {
		case "text":
			arr = append(arr, map[string]any{"type": "text", "text": p.Text})
		case "image":
			img := map[string]any{}
			if p.URL != "" {
				img["url"] = p.URL
			} else if p.DataB64 != "" {
				img["url"] = "data:" + p.MediaType + ";base64," + p.DataB64
			}
			arr = append(arr, map[string]any{"type": "image_url", "image_url": img})
		case "tool_use":
			if p.ToolUse != nil {
				toolCalls = append(toolCalls, map[string]any{
					"id":   p.ToolUse.ID,
					"type": "function",
					"function": map[string]any{
						"name":      p.ToolUse.Name,
						"arguments": string(p.ToolUse.Input),
					},
				})
			}
		default:
			if len(p.Raw) > 0 {
				var m map[string]any
				if json.Unmarshal(p.Raw, &m) == nil {
					arr = append(arr, m)
				}
			}
		}
	}
	return "", arr, toolCalls
}

func buildChatResponse(resp *ParsedResponse) ([]byte, error) {
	base := mapFromRaw(resp.RawBody)
	msg := map[string]any{
		"role":    "assistant",
		"content": resp.Content,
	}
	if len(resp.ToolCalls) > 0 {
		tcs := make([]map[string]any, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			tcs = append(tcs, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Arguments,
				},
			})
		}
		msg["tool_calls"] = tcs
	}
	choice := map[string]any{
		"index":   0,
		"message": msg,
	}
	if resp.Usage.FinishReason != nil {
		choice["finish_reason"] = *resp.Usage.FinishReason
	}
	base["choices"] = []any{choice}
	return json.Marshal(base)
}

// --- Anthropic Messages ---------------------------------------------

func buildMessagesRequest(req *ParsedRequest) ([]byte, error) {
	base := mapFromRaw(req.RawBody)
	base["model"] = req.Model
	if req.SystemPrompt != "" {
		base["system"] = req.SystemPrompt
	} else {
		delete(base, "system")
	}
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		entry := map[string]any{"role": m.Role}
		// Anthropic content is either a string (text-only) or an array.
		if len(m.Content) == 1 && m.Content[0].Type == "text" {
			entry["content"] = m.Content[0].Text
		} else {
			arr := make([]map[string]any, 0, len(m.Content))
			for _, p := range m.Content {
				arr = append(arr, messagesPartToMap(p))
			}
			entry["content"] = arr
		}
		msgs = append(msgs, entry)
	}
	base["messages"] = msgs
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": rawOrNull(t.Schema),
			})
		}
		base["tools"] = tools
	}
	return json.Marshal(base)
}

func messagesPartToMap(p ContentPart) map[string]any {
	switch p.Type {
	case "text":
		return map[string]any{"type": "text", "text": p.Text}
	case "image":
		src := map[string]any{}
		if p.DataB64 != "" {
			src["type"] = "base64"
			src["media_type"] = p.MediaType
			src["data"] = p.DataB64
		} else if p.URL != "" {
			src["type"] = "url"
			src["url"] = p.URL
		}
		return map[string]any{"type": "image", "source": src}
	case "tool_use":
		out := map[string]any{"type": "tool_use"}
		if p.ToolUse != nil {
			out["id"] = p.ToolUse.ID
			out["name"] = p.ToolUse.Name
			out["input"] = rawOrNull(p.ToolUse.Input)
		}
		return out
	case "tool_result":
		out := map[string]any{"type": "tool_result"}
		if p.ToolResult != nil {
			out["tool_use_id"] = p.ToolResult.ToolUseID
			out["content"] = p.ToolResult.Content
			if p.ToolResult.IsError {
				out["is_error"] = true
			}
		}
		return out
	default:
		if len(p.Raw) > 0 {
			var m map[string]any
			if json.Unmarshal(p.Raw, &m) == nil {
				return m
			}
		}
		return map[string]any{"type": p.Type}
	}
}

func buildMessagesResponse(resp *ParsedResponse) ([]byte, error) {
	base := mapFromRaw(resp.RawBody)
	blocks := make([]map[string]any, 0, 2+len(resp.ToolCalls))
	if resp.Reasoning != "" {
		blocks = append(blocks, map[string]any{
			"type":     "thinking",
			"thinking": resp.Reasoning,
		})
	}
	if resp.Content != "" {
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": resp.Content,
		})
	}
	for _, tc := range resp.ToolCalls {
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": json.RawMessage(tc.Arguments),
		})
	}
	base["content"] = blocks
	if resp.Usage.FinishReason != nil {
		base["stop_reason"] = *resp.Usage.FinishReason
	}
	return json.Marshal(base)
}

// --- OpenAI Responses -----------------------------------------------

func buildResponsesRequest(req *ParsedRequest) ([]byte, error) {
	base := mapFromRaw(req.RawBody)
	base["model"] = req.Model
	if req.SystemPrompt != "" {
		base["instructions"] = req.SystemPrompt
	}
	// Build input as array of message objects.
	input := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		input = append(input, map[string]any{
			"type":    "message",
			"role":    m.Role,
			"content": responsesPartsToArray(m.Content),
		})
	}
	base["input"] = input
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  rawOrNull(t.Schema),
			})
		}
		base["tools"] = tools
	}
	return json.Marshal(base)
}

func responsesPartsToArray(parts []ContentPart) []map[string]any {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, map[string]any{"type": "input_text", "text": p.Text})
		case "image":
			out = append(out, map[string]any{"type": "input_image", "image_url": p.URL})
		default:
			if len(p.Raw) > 0 {
				var m map[string]any
				if json.Unmarshal(p.Raw, &m) == nil {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

func buildResponsesResponse(resp *ParsedResponse) ([]byte, error) {
	base := mapFromRaw(resp.RawBody)
	output := make([]map[string]any, 0, 2+len(resp.ToolCalls))
	if resp.Reasoning != "" {
		output = append(output, map[string]any{
			"type": "reasoning",
			"summary": []map[string]any{
				{"type": "summary_text", "text": resp.Reasoning},
			},
		})
	}
	if resp.Content != "" {
		output = append(output, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "output_text", "text": resp.Content},
			},
		})
	}
	for _, tc := range resp.ToolCalls {
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        tc.ID,
			"name":      tc.Name,
			"arguments": tc.Arguments,
		})
	}
	base["output"] = output
	if resp.Usage.FinishReason != nil {
		base["status"] = *resp.Usage.FinishReason
	}
	return json.Marshal(base)
}

// --- helpers --------------------------------------------------------

// mapFromRaw decodes raw JSON into a map. Returns an empty map if raw
// is empty or not a JSON object — the caller always overwrites the
// fields it owns, so a clean slate is safe.
func mapFromRaw(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// rawOrNull returns the raw bytes when non-empty, else JSON null. Used
// for embedded schema fields where "absent" should marshal as null
// rather than {}.
func rawOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
