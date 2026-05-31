package v2

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/2nd1st/api-log/internal/parser"
	"github.com/2nd1st/api-log/internal/trace"
)

// DetectProtocol mirrors parser's internal detection. We duplicate the
// ~6 lines rather than export parser's unexported symbol; W1 must not
// modify another package per the task contract.
func DetectProtocol(path string) Protocol {
	switch {
	case strings.Contains(path, "/v1/chat/completions"):
		return ProtocolChat
	case strings.Contains(path, "/v1/messages"):
		return ProtocolMessages
	case strings.Contains(path, "/v1/responses"):
		return ProtocolResponses
	case strings.Contains(path, ":generateContent"),
		strings.Contains(path, ":streamGenerateContent"),
		strings.Contains(path, "/v1beta/"):
		return ProtocolGemini
	default:
		return ProtocolUnknown
	}
}

// ParsedRequestFromTrace builds a ParsedRequest from a finalized
// trace.Trace. The trace's request body must be parsed JSON
// (trace.Body.Body non-empty); a body that landed in BodyB64 yields a
// ParsedRequest with empty Messages/Tools — there is nothing to
// normalize from raw bytes.
//
// Per spec §2.5 the parsed shape is the source of truth; RawBody is the
// escape hatch for plugins that need original bytes.
func ParsedRequestFromTrace(t trace.Trace) ParsedRequest {
	req := ParsedRequest{
		Protocol: DetectProtocol(t.Path),
		Path:     t.Path,
		Method:   t.Method,
		Headers:  cloneHeader(http.Header(t.Req.Headers)),
		RawBody:  t.Req.Body,
	}
	if len(t.Req.Body) == 0 {
		return req
	}
	switch req.Protocol {
	case ProtocolChat:
		parseChatRequest(t.Req.Body, &req)
	case ProtocolMessages:
		parseMessagesRequest(t.Req.Body, &req)
	case ProtocolResponses:
		parseResponsesRequest(t.Req.Body, &req)
	case ProtocolGemini:
		parseGeminiRequest(t.Req.Body, &req)
	}
	return req
}

// ParsedResponseFromTrace builds a ParsedResponse from a finalized
// trace.Trace. Both streaming (Events) and non-streaming (Body) shapes
// land in the same returned struct so AFTER plugins that inspect the
// buffered post-stream view do not have to dispatch themselves.
//
// Usage is reused from parser.ExtractUsage — there is exactly one
// extractor in the project and the plugin surface does not get to
// re-implement it.
func ParsedResponseFromTrace(t trace.Trace) ParsedResponse {
	resp := ParsedResponse{
		Protocol: DetectProtocol(t.Path),
		Status:   t.Status,
		Headers:  cloneHeader(http.Header(t.Resp.Headers)),
		RawBody:  t.Resp.Body,
		Events:   t.Resp.Events,
		Usage:    parser.ExtractUsage(t),
	}
	switch resp.Protocol {
	case ProtocolChat:
		parseChatResponse(t, &resp)
	case ProtocolMessages:
		parseMessagesResponse(t, &resp)
	case ProtocolResponses:
		parseResponsesResponse(t, &resp)
	case ProtocolGemini:
		parseGeminiResponse(t, &resp)
	}
	return resp
}

// --- OpenAI Chat ----------------------------------------------------

// chatReqShape is the request envelope. Only fields v1 normalizes; the
// rest of the body is preserved in RawBody.
type chatReqShape struct {
	Model    string          `json:"model"`
	Messages []chatReqMsg    `json:"messages"`
	Tools    []chatReqTool   `json:"tools"`
	Stream   *bool           `json:"stream"`
	Raw      json.RawMessage `json:"-"`
}

type chatReqMsg struct {
	Role       string            `json:"role"`
	Content    json.RawMessage   `json:"content"`
	Name       string            `json:"name"`
	ToolCallID string            `json:"tool_call_id"`
	ToolCalls  []chatReqToolCall `json:"tool_calls"`
}

type chatReqToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatReqTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
	Raw json.RawMessage `json:"-"`
}

func parseChatRequest(body json.RawMessage, req *ParsedRequest) {
	var shape chatReqShape
	if err := json.Unmarshal(body, &shape); err != nil {
		return
	}
	req.Model = shape.Model
	if shape.Stream != nil {
		req.Streaming = *shape.Stream
	}
	for _, m := range shape.Messages {
		if strings.EqualFold(m.Role, "system") {
			// Chat may concatenate multiple system messages; per spec the
			// SystemPrompt field is "first system role text, computed".
			if req.SystemPrompt == "" {
				req.SystemPrompt = chatExtractText(m.Content)
			}
			continue
		}
		msg := Message{
			Role:       strings.ToLower(m.Role),
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		msg.Content = chatParseContent(m.Content)
		// Tool calls on an assistant turn are appended as tool_use parts
		// so the BEFORE hook can inspect them through one channel.
		for _, tc := range m.ToolCalls {
			msg.Content = append(msg.Content, ContentPart{
				Type: "tool_use",
				ToolUse: &ToolUse{
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(tc.Function.Arguments),
				},
			})
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range shape.Tools {
		// Re-marshal so Raw carries the original blob even though we
		// already unmarshalled a typed view.
		raw, _ := json.Marshal(t)
		req.Tools = append(req.Tools, Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Schema:      t.Function.Parameters,
			Raw:         raw,
		})
	}
}

// chatParseContent decodes the Chat content field, which is either a
// JSON string or a JSON array of typed parts.
func chatParseContent(raw json.RawMessage) []ContentPart {
	if len(raw) == 0 {
		return nil
	}
	// Case 1: plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ContentPart{{Type: "text", Text: s, Raw: raw}}
	}
	// Case 2: array of parts.
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]ContentPart, 0, len(arr))
	for _, item := range arr {
		var probe struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
			InputAudio *struct {
				Data   string `json:"data"`
				Format string `json:"format"`
			} `json:"input_audio"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			out = append(out, ContentPart{Type: "unknown", Raw: item})
			continue
		}
		switch probe.Type {
		case "text":
			out = append(out, ContentPart{Type: "text", Text: probe.Text, Raw: item})
		case "image_url":
			url := ""
			if probe.ImageURL != nil {
				url = probe.ImageURL.URL
			}
			out = append(out, ContentPart{Type: "image", URL: url, Raw: item})
		case "input_audio":
			data, mt := "", ""
			if probe.InputAudio != nil {
				data = probe.InputAudio.Data
				if probe.InputAudio.Format != "" {
					mt = "audio/" + probe.InputAudio.Format
				}
			}
			out = append(out, ContentPart{Type: "audio", DataB64: data, MediaType: mt, Raw: item})
		default:
			out = append(out, ContentPart{Type: probe.Type, Raw: item})
		}
	}
	return out
}

// chatExtractText returns the concatenated text of a Chat content
// value (string or array of parts). Used for the SystemPrompt lift.
func chatExtractText(raw json.RawMessage) string {
	parts := chatParseContent(raw)
	var sb strings.Builder
	for i, p := range parts {
		if p.Type != "text" {
			continue
		}
		if i > 0 && sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// chatRespShape is the Chat response envelope. Streaming is handled by
// concatenating delta events; non-streaming reads choices[0].message.
type chatRespShape struct {
	Choices []chatRespChoice `json:"choices"`
}

type chatRespChoice struct {
	Message *struct {
		Role      string            `json:"role"`
		Content   json.RawMessage   `json:"content"`
		ToolCalls []chatReqToolCall `json:"tool_calls"`
	} `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

func parseChatResponse(t trace.Trace, resp *ParsedResponse) {
	if len(t.Resp.Body) > 0 {
		var shape chatRespShape
		if err := json.Unmarshal(t.Resp.Body, &shape); err == nil && len(shape.Choices) > 0 {
			c := shape.Choices[0]
			if c.Message != nil {
				resp.Content = chatExtractText(c.Message.Content)
				for _, tc := range c.Message.ToolCalls {
					resp.ToolCalls = append(resp.ToolCalls, ToolCall{
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					})
				}
			}
		}
		return
	}
	if len(t.Resp.Events) == 0 {
		return
	}
	// Streaming: concat delta.content across events; track tool_call
	// fragments by index.
	type tcAccum struct {
		ID, Name string
		Args     strings.Builder
	}
	tcByIndex := map[int]*tcAccum{}
	var content strings.Builder
	for _, ev := range t.Resp.Events {
		if len(ev.Data) == 0 {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta *struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(ev.Data, &frame); err != nil {
			continue
		}
		for _, ch := range frame.Choices {
			if ch.Delta == nil {
				continue
			}
			if ch.Delta.Content != "" {
				content.WriteString(ch.Delta.Content)
			}
			for _, tc := range ch.Delta.ToolCalls {
				a := tcByIndex[tc.Index]
				if a == nil {
					a = &tcAccum{}
					tcByIndex[tc.Index] = a
				}
				if tc.ID != "" {
					a.ID = tc.ID
				}
				if tc.Function.Name != "" {
					a.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					a.Args.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
	resp.Content = content.String()
	// Emit tool_calls in ascending index order.
	if len(tcByIndex) > 0 {
		keys := sortedIntKeys(tcByIndex)
		for _, k := range keys {
			a := tcByIndex[k]
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID: a.ID, Name: a.Name, Arguments: a.Args.String(),
			})
		}
	}
}

// --- Anthropic Messages ---------------------------------------------

type messagesReqShape struct {
	Model    string            `json:"model"`
	System   json.RawMessage   `json:"system"`
	Messages []messagesReqMsg  `json:"messages"`
	Tools    []messagesReqTool `json:"tools"`
	Stream   *bool             `json:"stream"`
}

type messagesReqMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type messagesReqTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Raw         json.RawMessage `json:"-"`
}

func parseMessagesRequest(body json.RawMessage, req *ParsedRequest) {
	var shape messagesReqShape
	if err := json.Unmarshal(body, &shape); err != nil {
		return
	}
	req.Model = shape.Model
	if shape.Stream != nil {
		req.Streaming = *shape.Stream
	}
	req.SystemPrompt = messagesExtractText(shape.System)
	for _, m := range shape.Messages {
		msg := Message{Role: strings.ToLower(m.Role)}
		msg.Content = messagesParseContent(m.Content)
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range shape.Tools {
		raw, _ := json.Marshal(t)
		req.Tools = append(req.Tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.InputSchema,
			Raw:         raw,
		})
	}
}

// messagesParseContent decodes the Anthropic content field — string or
// array of blocks (text, image, tool_use, tool_result).
func messagesParseContent(raw json.RawMessage) []ContentPart {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ContentPart{{Type: "text", Text: s, Raw: raw}}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]ContentPart, 0, len(arr))
	for _, item := range arr {
		var probe struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Source *struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
				URL       string `json:"url"`
			} `json:"source"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			out = append(out, ContentPart{Type: "unknown", Raw: item})
			continue
		}
		switch probe.Type {
		case "text":
			out = append(out, ContentPart{Type: "text", Text: probe.Text, Raw: item})
		case "image":
			cp := ContentPart{Type: "image", Raw: item}
			if probe.Source != nil {
				cp.MediaType = probe.Source.MediaType
				cp.DataB64 = probe.Source.Data
				cp.URL = probe.Source.URL
			}
			out = append(out, cp)
		case "tool_use":
			out = append(out, ContentPart{
				Type:    "tool_use",
				ToolUse: &ToolUse{ID: probe.ID, Name: probe.Name, Input: probe.Input},
				Raw:     item,
			})
		case "tool_result":
			out = append(out, ContentPart{
				Type: "tool_result",
				ToolResult: &ToolResult{
					ToolUseID: probe.ToolUseID,
					Content:   messagesExtractText(probe.Content),
					IsError:   probe.IsError,
				},
				Raw: item,
			})
		default:
			out = append(out, ContentPart{Type: probe.Type, Raw: item})
		}
	}
	return out
}

func messagesExtractText(raw json.RawMessage) string {
	parts := messagesParseContent(raw)
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

type messagesRespShape struct {
	Content []json.RawMessage `json:"content"`
}

func parseMessagesResponse(t trace.Trace, resp *ParsedResponse) {
	if len(t.Resp.Body) > 0 {
		var shape messagesRespShape
		if err := json.Unmarshal(t.Resp.Body, &shape); err == nil {
			collectMessagesBlocks(shape.Content, resp)
		}
		return
	}
	if len(t.Resp.Events) == 0 {
		return
	}
	// Streaming: walk content_block_* events, building blocks by index.
	type blockAccum struct {
		Kind     string // "text" | "thinking" | "tool_use"
		Text     strings.Builder
		ToolID   string
		ToolName string
		ToolArgs strings.Builder
	}
	byIdx := map[int]*blockAccum{}
	for _, ev := range t.Resp.Events {
		if len(ev.Data) == 0 {
			continue
		}
		switch ev.Name {
		case "content_block_start":
			var frame struct {
				Index        int `json:"index"`
				ContentBlock *struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(ev.Data, &frame); err != nil || frame.ContentBlock == nil {
				continue
			}
			b := &blockAccum{Kind: frame.ContentBlock.Type}
			if frame.ContentBlock.Type == "tool_use" {
				b.ToolID = frame.ContentBlock.ID
				b.ToolName = frame.ContentBlock.Name
			}
			byIdx[frame.Index] = b
		case "content_block_delta":
			var frame struct {
				Index int `json:"index"`
				Delta *struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(ev.Data, &frame); err != nil || frame.Delta == nil {
				continue
			}
			b := byIdx[frame.Index]
			if b == nil {
				continue
			}
			switch frame.Delta.Type {
			case "text_delta":
				b.Text.WriteString(frame.Delta.Text)
			case "thinking_delta":
				b.Text.WriteString(frame.Delta.Thinking)
			case "input_json_delta":
				b.ToolArgs.WriteString(frame.Delta.PartialJSON)
			}
		}
	}
	// Emit in index order.
	keys := sortedIntKeys(byIdx)
	var content, reasoning strings.Builder
	for _, k := range keys {
		b := byIdx[k]
		switch b.Kind {
		case "text":
			content.WriteString(b.Text.String())
		case "thinking":
			reasoning.WriteString(b.Text.String())
		case "tool_use":
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID: b.ToolID, Name: b.ToolName, Arguments: b.ToolArgs.String(),
			})
		}
	}
	resp.Content = content.String()
	resp.Reasoning = reasoning.String()
}

func collectMessagesBlocks(blocks []json.RawMessage, resp *ParsedResponse) {
	var content, reasoning strings.Builder
	for _, item := range blocks {
		var probe struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "text":
			content.WriteString(probe.Text)
		case "thinking":
			reasoning.WriteString(probe.Thinking)
		case "tool_use":
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID: probe.ID, Name: probe.Name, Arguments: string(probe.Input),
			})
		}
	}
	resp.Content = content.String()
	resp.Reasoning = reasoning.String()
}

// --- OpenAI Responses -----------------------------------------------
//
// /v1/responses request shape: { model, input, instructions, tools, stream }.
// `input` may be a plain string, an array of message objects, or an
// array of typed input parts. We normalize to Messages similar to Chat.

type responsesReqShape struct {
	Model        string             `json:"model"`
	Instructions string             `json:"instructions"`
	Input        json.RawMessage    `json:"input"`
	Tools        []responsesReqTool `json:"tools"`
	Stream       *bool              `json:"stream"`
}

type responsesReqTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Raw         json.RawMessage `json:"-"`
}

func parseResponsesRequest(body json.RawMessage, req *ParsedRequest) {
	var shape responsesReqShape
	if err := json.Unmarshal(body, &shape); err != nil {
		return
	}
	req.Model = shape.Model
	req.SystemPrompt = shape.Instructions
	if shape.Stream != nil {
		req.Streaming = *shape.Stream
	}
	// Input variants.
	if len(shape.Input) > 0 {
		var s string
		if err := json.Unmarshal(shape.Input, &s); err == nil {
			if s != "" {
				req.Messages = append(req.Messages, Message{
					Role:    "user",
					Content: []ContentPart{{Type: "text", Text: s}},
				})
			}
		} else {
			var arr []json.RawMessage
			if err := json.Unmarshal(shape.Input, &arr); err == nil {
				for _, item := range arr {
					var probe struct {
						Type    string          `json:"type"`
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					}
					if err := json.Unmarshal(item, &probe); err != nil {
						continue
					}
					if probe.Role == "" {
						probe.Role = "user"
					}
					if strings.EqualFold(probe.Role, "system") {
						if req.SystemPrompt == "" {
							req.SystemPrompt = chatExtractText(probe.Content)
						}
						continue
					}
					msg := Message{
						Role:    strings.ToLower(probe.Role),
						Content: responsesParseContent(probe.Content),
					}
					req.Messages = append(req.Messages, msg)
				}
			}
		}
	}
	for _, t := range shape.Tools {
		raw, _ := json.Marshal(t)
		req.Tools = append(req.Tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Parameters,
			Raw:         raw,
		})
	}
}

// responsesParseContent: array of typed parts, similar enough to Chat.
func responsesParseContent(raw json.RawMessage) []ContentPart {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ContentPart{{Type: "text", Text: s, Raw: raw}}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]ContentPart, 0, len(arr))
	for _, item := range arr {
		var probe struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			ImageURL  string `json:"image_url"`
			InputText string `json:"input_text"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			out = append(out, ContentPart{Type: "unknown", Raw: item})
			continue
		}
		switch probe.Type {
		case "input_text", "output_text", "text":
			text := probe.Text
			if text == "" {
				text = probe.InputText
			}
			out = append(out, ContentPart{Type: "text", Text: text, Raw: item})
		case "input_image":
			out = append(out, ContentPart{Type: "image", URL: probe.ImageURL, Raw: item})
		default:
			out = append(out, ContentPart{Type: probe.Type, Raw: item})
		}
	}
	return out
}

func parseResponsesResponse(t trace.Trace, resp *ParsedResponse) {
	if len(t.Resp.Body) > 0 {
		var shape struct {
			Output []json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(t.Resp.Body, &shape); err == nil {
			collectResponsesOutput(shape.Output, resp)
		}
		return
	}
	if len(t.Resp.Events) == 0 {
		return
	}
	var content, reasoning strings.Builder
	type tcAccum struct {
		ID, Name string
		Args     strings.Builder
	}
	tcByID := map[string]*tcAccum{}
	var tcOrder []string
	for _, ev := range t.Resp.Events {
		if len(ev.Data) == 0 {
			continue
		}
		switch ev.Name {
		case "response.output_text.delta":
			var frame struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &frame) == nil {
				content.WriteString(frame.Delta)
			}
		case "response.reasoning_summary_text.delta", "response.reasoning.delta":
			var frame struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &frame) == nil {
				reasoning.WriteString(frame.Delta)
			}
		case "response.function_call_arguments.delta":
			var frame struct {
				ItemID string `json:"item_id"`
				Delta  string `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &frame) == nil {
				a := tcByID[frame.ItemID]
				if a == nil {
					a = &tcAccum{ID: frame.ItemID}
					tcByID[frame.ItemID] = a
					tcOrder = append(tcOrder, frame.ItemID)
				}
				a.Args.WriteString(frame.Delta)
			}
		case "response.output_item.added":
			var frame struct {
				Item *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"item"`
			}
			if err := json.Unmarshal(ev.Data, &frame); err != nil || frame.Item == nil {
				continue
			}
			if frame.Item.Type == "function_call" {
				a := tcByID[frame.Item.ID]
				if a == nil {
					a = &tcAccum{ID: frame.Item.ID}
					tcByID[frame.Item.ID] = a
					tcOrder = append(tcOrder, frame.Item.ID)
				}
				a.Name = frame.Item.Name
			}
		}
	}
	resp.Content = content.String()
	resp.Reasoning = reasoning.String()
	for _, id := range tcOrder {
		a := tcByID[id]
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{
			ID: a.ID, Name: a.Name, Arguments: a.Args.String(),
		})
	}
}

func collectResponsesOutput(items []json.RawMessage, resp *ParsedResponse) {
	var content, reasoning strings.Builder
	for _, item := range items {
		var probe struct {
			Type    string            `json:"type"`
			Content []json.RawMessage `json:"content"`
			Summary []json.RawMessage `json:"summary"`
			ID      string            `json:"id"`
			Name    string            `json:"name"`
			Args    json.RawMessage   `json:"arguments"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "message":
			for _, c := range probe.Content {
				var inner struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(c, &inner) != nil {
					continue
				}
				if inner.Type == "output_text" || inner.Type == "text" {
					content.WriteString(inner.Text)
				}
			}
		case "reasoning":
			for _, c := range probe.Summary {
				var inner struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(c, &inner) != nil {
					continue
				}
				if inner.Type == "summary_text" {
					reasoning.WriteString(inner.Text)
				}
			}
		case "function_call":
			args := ""
			if len(probe.Args) > 0 {
				// Args may be a JSON string or already an object; preserve verbatim.
				args = string(probe.Args)
				var s string
				if err := json.Unmarshal(probe.Args, &s); err == nil {
					args = s
				}
			}
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID: probe.ID, Name: probe.Name, Arguments: args,
			})
		}
	}
	resp.Content = content.String()
	resp.Reasoning = reasoning.String()
}

// --- Google Gemini --------------------------------------------------

type geminiReqShape struct {
	Contents          []geminiReqContent `json:"contents"`
	SystemInstruction *geminiReqContent  `json:"systemInstruction"`
	Tools             []geminiReqTool    `json:"tools"`
}

type geminiReqContent struct {
	Role  string            `json:"role"`
	Parts []json.RawMessage `json:"parts"`
}

type geminiReqTool struct {
	FunctionDeclarations []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"functionDeclarations"`
	Raw json.RawMessage `json:"-"`
}

func parseGeminiRequest(body json.RawMessage, req *ParsedRequest) {
	var shape geminiReqShape
	if err := json.Unmarshal(body, &shape); err != nil {
		return
	}
	if shape.SystemInstruction != nil {
		req.SystemPrompt = geminiExtractText(shape.SystemInstruction.Parts)
	}
	// Gemini path detection sets Model in ParsedRequest from path —
	// mirror parser.ExtractUsage's regex semantics, but to avoid coupling
	// we leave req.Model empty here. The request body itself has no
	// "model" field for v1beta generateContent (the model is in the URL).
	// req.Model can be set by the caller using DetectGeminiModel(path).
	for _, c := range shape.Contents {
		role := strings.ToLower(c.Role)
		if role == "" {
			role = "user"
		}
		// Gemini uses "model" for assistant.
		if role == "model" {
			role = "assistant"
		}
		msg := Message{Role: role, Content: geminiParseParts(c.Parts)}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range shape.Tools {
		raw, _ := json.Marshal(t)
		for _, fd := range t.FunctionDeclarations {
			req.Tools = append(req.Tools, Tool{
				Name:        fd.Name,
				Description: fd.Description,
				Schema:      fd.Parameters,
				Raw:         raw,
			})
		}
	}
}

func geminiParseParts(parts []json.RawMessage) []ContentPart {
	out := make([]ContentPart, 0, len(parts))
	for _, item := range parts {
		var probe struct {
			Text       string `json:"text"`
			InlineData *struct {
				MimeType string `json:"mimeType"`
				Data     string `json:"data"`
			} `json:"inlineData"`
			FileData *struct {
				MimeType string `json:"mimeType"`
				FileURI  string `json:"fileUri"`
			} `json:"fileData"`
			FunctionCall *struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			} `json:"functionCall"`
			FunctionResponse *struct {
				Name     string          `json:"name"`
				Response json.RawMessage `json:"response"`
			} `json:"functionResponse"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			out = append(out, ContentPart{Type: "unknown", Raw: item})
			continue
		}
		switch {
		case probe.Text != "":
			out = append(out, ContentPart{Type: "text", Text: probe.Text, Raw: item})
		case probe.InlineData != nil:
			out = append(out, ContentPart{
				Type: "image", MediaType: probe.InlineData.MimeType,
				DataB64: probe.InlineData.Data, Raw: item,
			})
		case probe.FileData != nil:
			out = append(out, ContentPart{
				Type: "image", MediaType: probe.FileData.MimeType,
				URL: probe.FileData.FileURI, Raw: item,
			})
		case probe.FunctionCall != nil:
			out = append(out, ContentPart{
				Type: "tool_use",
				ToolUse: &ToolUse{
					Name:  probe.FunctionCall.Name,
					Input: probe.FunctionCall.Args,
				},
				Raw: item,
			})
		case probe.FunctionResponse != nil:
			out = append(out, ContentPart{
				Type: "tool_result",
				ToolResult: &ToolResult{
					Content: string(probe.FunctionResponse.Response),
				},
				Raw: item,
			})
		default:
			out = append(out, ContentPart{Type: "unknown", Raw: item})
		}
	}
	return out
}

func geminiExtractText(parts []json.RawMessage) string {
	cps := geminiParseParts(parts)
	var sb strings.Builder
	for _, p := range cps {
		if p.Type == "text" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func parseGeminiResponse(t trace.Trace, resp *ParsedResponse) {
	if len(t.Resp.Body) == 0 {
		return
	}
	var shape struct {
		Candidates []struct {
			Content *geminiReqContent `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(t.Resp.Body, &shape); err != nil {
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

// --- helpers --------------------------------------------------------

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

func sortedIntKeys[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Simple insertion sort; the maps are tiny (a handful of tool
	// indices). Avoids dragging in sort just to slice four ints.
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1] > out[j] {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}
