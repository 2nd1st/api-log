package session

import (
	"encoding/json"
	"strings"
)

// Build returns the session-prefix turns array for one trace, per
// ARCHITECTURE § 5.1's per-protocol table.
//
// path is the request path (e.g. "/v1/messages"). reqBody is the
// parsed JSON object from trace.Req.Body (json.RawMessage). If reqBody
// is nil or empty, or the path is not one of the three supported
// session-bearing protocols, Build returns (nil, false) — the trace
// has no session concept and the writer treats it as a self-root.
func Build(path string, reqBody json.RawMessage) (turns []json.RawMessage, ok bool) {
	if len(reqBody) == 0 {
		return nil, false
	}

	switch protocolForPath(path) {
	case protocolChat:
		return fromChat(reqBody)
	case protocolMessages:
		return fromAnthropic(reqBody)
	case protocolResponses:
		return fromResponses(reqBody)
	default:
		return nil, false
	}
}

type protocol int

const (
	protocolUnknown protocol = iota
	protocolChat
	protocolMessages
	protocolResponses
)

func protocolForPath(path string) protocol {
	// Strip query string; we match on path only.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	switch path {
	case "/v1/chat/completions":
		return protocolChat
	case "/v1/messages":
		return protocolMessages
	case "/v1/responses":
		return protocolResponses
	default:
		return protocolUnknown
	}
}

// fromChat: OpenAI Chat Completions uses `messages` directly.
// System message is already messages[0] (if present) by the protocol's
// own convention; no synthetic turn needed.
func fromChat(body json.RawMessage) ([]json.RawMessage, bool) {
	var s struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, false
	}
	if len(s.Messages) == 0 {
		return nil, false
	}
	return s.Messages, true
}

// fromAnthropic: Anthropic Messages keeps `system` OUTSIDE messages.
// We prepend a synthetic VirtualSystemRole turn so two conversations
// sharing messages[0] but with different system prompts are correctly
// identified as separate sessions.
func fromAnthropic(body json.RawMessage) ([]json.RawMessage, bool) {
	var s struct {
		System   json.RawMessage   `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, false
	}
	if len(s.Messages) == 0 {
		return nil, false
	}
	turns := s.Messages
	if len(s.System) > 0 && !isJSONNull(s.System) {
		sysTurn, ok := buildVirtualSystem(s.System)
		if ok {
			turns = append([]json.RawMessage{sysTurn}, turns...)
		}
	}
	return turns, true
}

// fromResponses: OpenAI Responses uses `input` (string or typed-array)
// instead of `messages`. v0 normalizes narrow scope:
//   - string input → one user message
//   - array of {type:"message", role, content} items → one turn per item
//   - anything else → fall back to hashing the raw `input` value as a
//     single turn (so two byte-identical raw inputs collide; coarser
//     than chat-shaped traffic, but correct in the structural sense).
//
// `instructions` (if set) becomes a virtual system turn 0.
func fromResponses(body json.RawMessage) ([]json.RawMessage, bool) {
	var s struct {
		Instructions json.RawMessage `json:"instructions"`
		Input        json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, false
	}

	var turns []json.RawMessage

	if len(s.Instructions) > 0 && !isJSONNull(s.Instructions) {
		if sysTurn, ok := buildVirtualSystem(s.Instructions); ok {
			turns = append(turns, sysTurn)
		}
	}

	if len(s.Input) == 0 || isJSONNull(s.Input) {
		if len(turns) == 0 {
			return nil, false
		}
		return turns, true
	}

	// Try string form.
	var asString string
	if err := json.Unmarshal(s.Input, &asString); err == nil {
		turn, _ := buildUserTurnFromString(asString)
		turns = append(turns, turn)
		return turns, true
	}

	// Try homogeneous chat-shaped array.
	var asArray []json.RawMessage
	if err := json.Unmarshal(s.Input, &asArray); err == nil {
		homog := true
		for _, item := range asArray {
			var probe struct {
				Type string `json:"type"`
				Role string `json:"role"`
			}
			if err := json.Unmarshal(item, &probe); err != nil || probe.Type != "message" || probe.Role == "" {
				homog = false
				break
			}
		}
		if homog && len(asArray) > 0 {
			turns = append(turns, asArray...)
			return turns, true
		}
	}

	// Fallback: hash the raw input as a single opaque turn. Two
	// byte-identical raw inputs collide; this is the coarse-but-correct
	// case noted in ARCHITECTURE § 5.1.
	opaqueTurn, _ := buildOpaqueTurn("input", s.Input)
	turns = append(turns, opaqueTurn)
	return turns, true
}

// buildVirtualSystem makes one VirtualSystemRole turn from the system
// content (which may be a string or a structured value).
func buildVirtualSystem(content json.RawMessage) (json.RawMessage, bool) {
	out := map[string]any{
		"role":    VirtualSystemRole,
		"content": json.RawMessage(content),
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(b), true
}

func buildUserTurnFromString(content string) (json.RawMessage, bool) {
	b, err := json.Marshal(map[string]any{
		"role":    "user",
		"content": content,
	})
	if err != nil {
		return nil, false
	}
	return json.RawMessage(b), true
}

func buildOpaqueTurn(field string, value json.RawMessage) (json.RawMessage, bool) {
	b, err := json.Marshal(map[string]any{
		"role":  "user",
		field:   json.RawMessage(value),
	})
	if err != nil {
		return nil, false
	}
	return json.RawMessage(b), true
}

func isJSONNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "null"
}
