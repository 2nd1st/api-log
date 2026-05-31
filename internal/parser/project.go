// Project-context extraction. Pulls the project name out of an L2
// system prompt by mirroring the viewer's promptSource.extractProjectContext
// (api-log-viewer/src/lib/promptSource.ts) so the backend-derived column
// stays in lockstep with what the UI computes at render time.
//
// PHILOSOPHY § 1 carve-out 1: deterministic copy of NAMED protocol body
// fields (Chat messages[role=system], Messages system, Responses
// instructions / input items[role=system]) followed by regex over the
// resulting text. No header sniffing, no synthesized identifiers — the
// regexes match against the operator-authored injection format Claude
// Code / codex actually emit.
//
// PHILOSOPHY § 2: ExtractProjectContext + ExtractSystemPrompt MUST NOT
// panic and MUST NOT block. Empty / unknown inputs return the zero
// ProjectContext (Name == "") — the caller treats that as "no project."
//
// PHILOSOPHY § 6: SQLite columns derived from this output are
// rebuildable from JSONL by replaying these extractors against the
// recorded request body.
//
// PHILOSOPHY § 7: small, slow surface — only the three protocols listed
// below (Chat / Messages / Responses). Gemini and unknown paths return
// "" because the contract names no project-injection path for them.
package parser

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/xiayangzhang/api-log/internal/trace"
)

// ProjectContext is the result of ExtractProjectContext. Name == ""
// means "no project signal found"; the Source sentinel distinguishes
// where the name came from when Name is non-empty.
//
// Source values:
//   - "agents-md"     — AGENTS.md injection reference adjacent to a heading
//   - "claude-md"     — CLAUDE.md injection reference adjacent to a heading
//   - "first-heading" — text simply starts with `# …` (no file ref)
//   - ""              — no match (paired with empty Name)
type ProjectContext struct {
	Name   string
	Source string
}

// fileRefRE matches an injection-style AGENTS.md / CLAUDE.md reference.
// Mirror of the viewer's FILE_REF_RE: require either the "Contents of"
// preamble OR a path separator immediately before the filename so a
// generic prose mention ("durable instructions like CLAUDE.md") does
// NOT match. Group 1 captures the filename literal so callers can
// branch agents-md vs claude-md off the same match.
//
// Slash is not special in Go regex syntax — no `\/` escape needed.
var fileRefRE = regexp.MustCompile(`(?:Contents of\s+\S*?|[/\\][^\s]*?)\b(AGENTS\.md|CLAUDE\.md|agents\.md|claude\.md)\b`)

// headingAtLineStartRE matches the first markdown heading after a given
// offset — anchored at start-of-text OR newline, capturing the heading
// body up to the next newline.
var headingAtLineStartRE = regexp.MustCompile(`(?:^|\n)#{1,6}\s+([^\n]+)`)

// leadingXmlCloseRE matches a `</tag>` close at the very start of the
// text (with optional whitespace before/after). Used to step past the
// end of a leading L1 XML scaffold so a mixed-prompt's L2 first
// heading is still picked.
var leadingXmlCloseRE = regexp.MustCompile(`^\s*</[a-zA-Z][\w-]*>\s*`)

// startHeadingRE matches a markdown heading at the very start of the
// remainder (after optional whitespace). The FIRST non-empty line must
// be a heading for strategy-2 to fire.
var startHeadingRE = regexp.MustCompile(`^\s*#{1,6}\s+([^\n]+)`)

// ExtractProjectContext returns the project name + source for the L2
// portion of a system prompt. Strategy mirrors the viewer:
//
//  1. AGENTS.md / CLAUDE.md injection-style file reference (path-prefixed
//     OR "Contents of …" preamble — NOT a bare prose mention) followed
//     by a markdown heading within a tight window → "agents-md" or
//     "claude-md".
//  2. Text starts with a `# …` heading (after skipping an optional
//     leading XML close tag) → "first-heading".
//  3. Otherwise zero value (Name == "").
//
// Edge cases:
//   - Inline backticks around the name are stripped
//   - Empty / whitespace-only text → zero value
//   - HTML entities (lt / gt / amp) in the name are decoded
//
// The function is deliberately tolerant of vendor-harness "# System"
// style headings on the first-heading path — that's the documented
// best-effort behavior the viewer ships and the source sentinel makes
// the weakness visible to consumers.
func ExtractProjectContext(systemPrompt string) ProjectContext {
	text := strings.TrimSpace(systemPrompt)
	if text == "" {
		return ProjectContext{}
	}

	// ----- strategy 1: file reference + adjacent heading -----
	// FindStringSubmatchIndex gives us both group-1 (filename, for the
	// agents-vs-claude branch) and the match-end offset for the window.
	if loc := fileRefRE.FindStringSubmatchIndex(text); loc != nil {
		refEnd := loc[1]
		fileName := strings.ToLower(text[loc[2]:loc[3]])
		// 400-char window is generous but caps blast radius if the
		// operator wrote prose between the ref and the next heading.
		// Clamp: Go's slice panics if the upper bound exceeds len(text).
		windowEnd := refEnd + 400
		if windowEnd > len(text) {
			windowEnd = len(text)
		}
		window := text[refEnd:windowEnd]
		if m := headingAtLineStartRE.FindStringSubmatch(window); m != nil {
			name := cleanHeadingText(m[1])
			if name != "" {
				src := "claude-md"
				if strings.HasPrefix(fileName, "agents") {
					src = "agents-md"
				}
				return ProjectContext{Name: name, Source: src}
			}
		}
		// Fall through to strategy 2 if the ref had no following heading.
	}

	// ----- strategy 2: text starts with a heading (skip leading XML) -----
	// Step past any leading "</tag>" close so a mixed L1+L2 prompt that
	// ends its XML scaffold immediately before a markdown heading still
	// identifies the project.
	searchFrom := 0
	if x := leadingXmlCloseRE.FindString(text); x != "" {
		searchFrom = len(x)
	}
	if m := startHeadingRE.FindStringSubmatch(text[searchFrom:]); m != nil {
		name := cleanHeadingText(m[1])
		if name != "" {
			return ProjectContext{Name: name, Source: "first-heading"}
		}
	}

	return ProjectContext{}
}

// cleanHeadingText strips backticks + decodes the three HTML entities
// we realistically encounter, then trims whitespace.
//
// Codex / claude code occasionally inline the project name as
// `# Project: \`my-project\`` or `# \`my-proj\``. We peel a wrapping
// backtick pair AND strip backticks anywhere — names like `# \`my\``
// should display as `my`, not with the inline-code wrapper.
//
// HTML entity decode (amp last so pre-encoded "&amp;lt;" round-trips
// to "&lt;" not "<") is an addition over the viewer; the cost is one
// replace chain on a short string and none of the ported test cases
// exercise an entity in the name, so behavior is unchanged for them.
func cleanHeadingText(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "\r")
	// Peel a wrapping backtick pair.
	if len(s) >= 2 && strings.HasPrefix(s, "`") && strings.HasSuffix(s, "`") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	// Strip any remaining backticks.
	s = strings.ReplaceAll(s, "`", "")
	s = strings.TrimSpace(s)
	// Decode HTML entities: lt / gt then amp (amp last so pre-encoded
	// "&amp;lt;" round-trips correctly).
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

// ExtractSystemPrompt pulls the system / instructions text out of a
// finalized Trace's REQUEST body, dispatched by protocol. Empty when
// no system text is present or the body did not parse.
//
// Per PHILOSOPHY § 1 these are named protocol fields; per § 7 the
// surface is small — only the three protocols whose contracts name a
// dedicated system-prompt path:
//
//	Chat       /v1/chat/completions  : req.body.messages[role=system].content
//	Messages   /v1/messages          : req.body.system  (string OR array of {type,text})
//	Responses  /v1/responses         : req.body.instructions + input items[role=system]
//
// Anthropic Messages and OpenAI Responses both ship the "system" /
// "instructions" / "input" fields in TWO shapes — a plain string or an
// array of content blocks. The dominant Claude Code → /v1/messages
// fixture is array-shaped (each block carries cache_control), so a
// string-only handler would silently drop the most important case.
// Both shapes are accepted; text from multiple system blocks is
// joined with "\n\n".
//
// Gemini and unknown paths return "" — there is no operator-injected
// project file path for those protocols on this surface.
func ExtractSystemPrompt(t trace.Trace) string {
	if len(t.Req.Body) == 0 {
		return ""
	}
	switch detectProtocol(t.Path) {
	case protoChat:
		return extractChatSystem(t.Req.Body)
	case protoMessages:
		return extractMessagesSystem(t.Req.Body)
	case protoResponses:
		return extractResponsesSystem(t.Req.Body)
	default:
		return ""
	}
}

// --- per-protocol system extractors ---

// chatMessage is a single entry in Chat's req.body.messages array.
// content is RawMessage because it may be either a string or an array
// of parts ({type:"text", text:"..."}, ...). Both are accepted.
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatReqSystem struct {
	Messages []chatMessage `json:"messages"`
}

func extractChatSystem(body json.RawMessage) string {
	var req chatReqSystem
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	var out []string
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		if s := contentToString(m.Content); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n\n")
}

// messagesReqSystem mirrors the Messages request body's system field.
// The field is RawMessage because the spec allows either a string or an
// array of content blocks ({type:"text", text:"..."}); both shapes are
// observed in production.
type messagesReqSystem struct {
	System json.RawMessage `json:"system"`
}

func extractMessagesSystem(body json.RawMessage) string {
	var req messagesReqSystem
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	if len(req.System) == 0 {
		return ""
	}
	return contentToString(req.System)
}

// responsesReqSystem mirrors the Responses request body's two
// system-prompt-bearing fields. `instructions` is a top-level string
// per the OpenAI Responses contract; `input` is a union of (string |
// items[]) where each item may carry role:"system" with content
// blocks. When both are present they are joined.
type responsesReqSystem struct {
	Instructions *string         `json:"instructions"`
	Input        json.RawMessage `json:"input"`
}

type responsesInputItem struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func extractResponsesSystem(body json.RawMessage) string {
	var req responsesReqSystem
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	var parts []string
	if req.Instructions != nil && *req.Instructions != "" {
		parts = append(parts, *req.Instructions)
	}
	if len(req.Input) > 0 {
		// Try array-of-items shape; if not an array, ignore. The
		// string shape of `input` carries no system signal.
		var items []responsesInputItem
		if json.Unmarshal(req.Input, &items) == nil {
			for _, it := range items {
				if it.Role != "system" {
					continue
				}
				if s := contentToString(it.Content); s != "" {
					parts = append(parts, s)
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// contentToString accepts the polymorphic content/text shape used by
// all three protocols and returns a concatenated string:
//   - JSON string                  → the string itself
//   - JSON array of {type,text,…}  → join text fields with "\n\n"
//   - anything else                → ""
//
// Non-"text" types in an array are skipped — image / tool-use blocks
// carry no project-injection signal.
func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// String shape.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Array of parts shape.
	type part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	var parts []part
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		// Accept any block whose text is non-empty. The Anthropic
		// Messages spec emits {type:"text"} for system blocks; the
		// OpenAI Responses spec emits {type:"input_text"} for input
		// items. Both ship the text under the same "text" field.
		if p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n\n")
}
