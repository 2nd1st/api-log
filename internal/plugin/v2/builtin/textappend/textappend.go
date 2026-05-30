// Package textappend implements the BUILD-phase "text-append" plugin
// described in uiux-research/plugin-b-c-spec.md §7.2.
//
// One plugin instance can serve BOTH directions: Up.Suffix is appended
// to the last user message (or merged into the system prompt) before
// forwarding; Down.Suffix is appended to the assistant content (or
// reasoning) before the client sees it. Either side may be empty.
//
// Target semantics:
//
//	Up.Target = "last_user_message"  → append to the trailing user turn's
//	                                   text content
//	Up.Target = "system_prompt"      → append to ParsedRequest.SystemPrompt
//	Down.Target = "content"          → append to ParsedResponse.Content
//	Down.Target = "reasoning"        → append to ParsedResponse.Reasoning
//
// Streaming-AFTER for Down.Target = "content" rides the W1 framework's
// EmitBeforeFinish hook: one synthesized content-delta event is written
// at end-of-stream so the suffix lands after the model's final token.
// Streaming-AFTER for Down.Target = "reasoning" is documented as a v1
// limitation — the W1 dispatcher has no reasoning-delta synthesizer, so
// the suffix only lands on non-streaming responses. Operators wanting a
// reasoning-suffix in streaming should use the non-streaming response
// path or wait for a follow-up extension.
package textappend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v2 "github.com/leoyun/api-log/internal/plugin/v2"
)

// Type is the operator-facing type name registered with v2.RegisterBuiltin.
const Type = "text-append"

// Target constants are stable wire strings. New values land here once
// the spec freezes them; do not invent variants ad-hoc.
const (
	TargetLastUserMessage = "last_user_message"
	TargetSystemPrompt    = "system_prompt"
	TargetContent         = "content"
	TargetReasoning       = "reasoning"
)

// UpRule controls what BEFORE appends and where.
type UpRule struct {
	Suffix string `json:"suffix"`
	Target string `json:"target"`
}

// DownRule controls what AFTER appends and where.
type DownRule struct {
	Suffix string `json:"suffix"`
	Target string `json:"target"`
}

// Config is the parsed instance config.
type Config struct {
	Routes []string `json:"routes"`
	Up     UpRule   `json:"up"`
	Down   DownRule `json:"down"`
}

// Plugin implements both v2.BeforePlugin and v2.AfterPlugin. Either side
// is a no-op when its Suffix is empty.
type Plugin struct {
	cfg    Config
	routes routeMatcher
}

// New parses cfg, applies target defaults, validates enums, and returns
// a ready Plugin.
func New(cfg map[string]any) (*Plugin, error) {
	parsed, err := decodeConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("text-append: %w", err)
	}
	rm, err := compileRoutes(parsed.Routes)
	if err != nil {
		return nil, fmt.Errorf("text-append: %w", err)
	}
	return &Plugin{cfg: parsed, routes: rm}, nil
}

// Name returns the stable type identifier.
func (p *Plugin) Name() string { return Type }

// OnBefore appends Up.Suffix to either the trailing user turn's text or
// to SystemPrompt, per Up.Target. Returns ActionContinue (no rebuild)
// when:
//   - the route does not match
//   - Up.Suffix is empty
//   - target == last_user_message but no user turn carries text content
func (p *Plugin) OnBefore(_ context.Context, req *v2.ParsedRequest, _ map[string]any) v2.BeforeResult {
	if req == nil {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	if !p.routes.matches(req.Path) {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	if p.cfg.Up.Suffix == "" {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	switch p.cfg.Up.Target {
	case TargetSystemPrompt:
		out := *req
		out.SystemPrompt = req.SystemPrompt + p.cfg.Up.Suffix
		return v2.BeforeResult{Action: v2.ActionMutate, Mutated: &out}
	case TargetLastUserMessage:
		msgs, ok := appendLastUserText(req.Messages, p.cfg.Up.Suffix)
		if !ok {
			return v2.BeforeResult{Action: v2.ActionContinue}
		}
		out := *req
		out.Messages = msgs
		return v2.BeforeResult{Action: v2.ActionMutate, Mutated: &out}
	default:
		// decodeConfig rejects unknown targets, so this is unreachable
		// in practice. Fail open if somehow reached.
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
}

// OnAfter behaves per ac.Response branch:
//
//   - Streaming (ac.Response == nil) with Down.Target == "content":
//     register an EmitBeforeFinish callback that synthesizes one final
//     content-delta event carrying Down.Suffix. The W1 dispatcher fires
//     it on stream EOF for all protocols where SynthesizeContentDelta
//     supports the protocol (Messages, Chat, Responses). On Gemini the
//     synth is deferred at the framework layer and the callback is
//     dropped silently — operators see no suffix in Gemini streams.
//
//   - Streaming with Down.Target == "reasoning": skipped with a no-op
//     callback registration (no W1 helper for reasoning synth). Operators
//     wanting reasoning suffixes should rely on non-streaming responses.
//
//   - Non-streaming: append the suffix to ac.Response.Content (or
//     .Reasoning) and return ActionMutate. Shallow clone is safe — only
//     string fields move.
//
// Returns ActionContinue when the route does not match or Down.Suffix
// is empty. The non-streaming branch always Mutates when both checks
// pass — append is unconditional and operators who chain instances see
// additive suffixes (idempotence is not promised).
func (p *Plugin) OnAfter(_ context.Context, req *v2.ParsedRequest, ac *v2.AfterContext, _ map[string]any) v2.AfterResult {
	if req == nil || ac == nil {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if !p.routes.matches(req.Path) {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if p.cfg.Down.Suffix == "" {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if ac.Response == nil {
		// Streaming branch.
		if p.cfg.Down.Target == TargetReasoning {
			// No reasoning synth in W1; document and skip.
			return v2.AfterResult{Action: v2.ActionContinue}
		}
		suffix := p.cfg.Down.Suffix
		ac.EmitBeforeFinish(func(emit func(text string)) {
			emit(suffix)
		})
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	// Non-streaming branch.
	resp := *ac.Response
	switch p.cfg.Down.Target {
	case TargetReasoning:
		resp.Reasoning = ac.Response.Reasoning + p.cfg.Down.Suffix
	default:
		resp.Content = ac.Response.Content + p.cfg.Down.Suffix
	}
	return v2.AfterResult{Action: v2.ActionMutate, Mutated: &resp}
}

// ConfigSchema describes the per-instance form for the viewer Settings
// panel (W4). See textreplace for the W3-canonicalization note.
func (p *Plugin) ConfigSchema() Schema {
	return Schema{
		Fields: []SchemaField{
			{
				Name:        "routes",
				Label:       "Routes",
				Type:        "string_array",
				Description: "Path patterns to apply the rule to. Empty = all paths. Trailing '*' = prefix.",
			},
			{
				Name:        "up.suffix",
				Label:       "Request suffix (upstream)",
				Type:        "string",
				Description: "Text appended to the inbound request before forwarding.",
			},
			{
				Name: "up.target",
				Label: "Request target",
				Type:  "enum",
				Enum: []string{
					TargetLastUserMessage,
					TargetSystemPrompt,
				},
				Default:     TargetLastUserMessage,
				Description: "Where the upstream suffix lands.",
			},
			{
				Name:        "down.suffix",
				Label:       "Response suffix (downstream)",
				Type:        "string",
				Description: "Text appended to the outbound response before client send.",
			},
			{
				Name: "down.target",
				Label: "Response target",
				Type:  "enum",
				Enum: []string{
					TargetContent,
					TargetReasoning,
				},
				Default:     TargetContent,
				Description: "Where the downstream suffix lands. 'reasoning' is non-streaming only in v1.",
			},
		},
	}
}

// Schema and SchemaField mirror the textreplace stand-in. See that
// package for the W3-canonicalization note.
type Schema struct {
	Fields []SchemaField `json:"fields"`
}

type SchemaField struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Default     string   `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description,omitempty"`
}

// --- internals ------------------------------------------------------

func decodeConfig(cfg map[string]any) (Config, error) {
	var c Config
	if cfg == nil {
		// Apply defaults so an empty instance behaves sanely if the
		// operator only sets suffixes later via the viewer.
		c.Up.Target = TargetLastUserMessage
		c.Down.Target = TargetContent
		return c, nil
	}
	buf, err := json.Marshal(cfg)
	if err != nil {
		return c, fmt.Errorf("bad config: %w", err)
	}
	if err := json.Unmarshal(buf, &c); err != nil {
		return c, fmt.Errorf("bad config: %w", err)
	}
	if c.Up.Target == "" {
		c.Up.Target = TargetLastUserMessage
	}
	if c.Down.Target == "" {
		c.Down.Target = TargetContent
	}
	if err := validateUpTarget(c.Up.Target); err != nil {
		return c, err
	}
	if err := validateDownTarget(c.Down.Target); err != nil {
		return c, err
	}
	return c, nil
}

func validateUpTarget(t string) error {
	switch t {
	case TargetLastUserMessage, TargetSystemPrompt:
		return nil
	default:
		return fmt.Errorf("up.target = %q (want %q or %q)",
			t, TargetLastUserMessage, TargetSystemPrompt)
	}
}

func validateDownTarget(t string) error {
	switch t {
	case TargetContent, TargetReasoning:
		return nil
	default:
		return fmt.Errorf("down.target = %q (want %q or %q)",
			t, TargetContent, TargetReasoning)
	}
}

// appendLastUserText returns a cloned msgs slice with the trailing
// user turn's last text part extended by suffix. ok is false when no
// user turn carries a text part — the caller falls back to Continue
// rather than emit an empty mutation.
//
// The original slice and its backing arrays are NOT aliased into the
// returned value; the caller's ParsedRequest is safe to retain.
func appendLastUserText(msgs []v2.Message, suffix string) (out []v2.Message, ok bool) {
	// Find the last user turn with a text part.
	lastMsg, lastPart := -1, -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if !strings.EqualFold(msgs[i].Role, "user") {
			continue
		}
		for j := len(msgs[i].Content) - 1; j >= 0; j-- {
			if msgs[i].Content[j].Type == "text" {
				lastMsg, lastPart = i, j
				break
			}
		}
		if lastMsg >= 0 {
			break
		}
	}
	if lastMsg < 0 {
		return nil, false
	}
	out = make([]v2.Message, len(msgs))
	copy(out, msgs)
	parts := make([]v2.ContentPart, len(msgs[lastMsg].Content))
	copy(parts, msgs[lastMsg].Content)
	parts[lastPart].Text = parts[lastPart].Text + suffix
	out[lastMsg].Content = parts
	return out, true
}

// --- route matching -------------------------------------------------
//
// Same semantics as the textreplace package's matcher (and as pathfilter).
// We duplicate the 30-odd lines rather than introduce a shared sub-package
// for v1 — when W4 / W5 land we can lift the matcher into v2 or a
// neighboring helper. Two copies that read identically is preferable to
// one premature abstraction for the v1 surface.

type routeMatcher struct {
	all      bool
	patterns []routePattern
}

type routePattern struct {
	raw      string
	prefix   string
	isPrefix bool
}

func compileRoutes(raw []string) (routeMatcher, error) {
	rm := routeMatcher{}
	if len(raw) == 0 {
		rm.all = true
		return rm, nil
	}
	out := make([]routePattern, 0, len(raw))
	for i, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			return rm, fmt.Errorf("routes[%d] is empty", i)
		}
		if s == "*" {
			rm.all = true
			return rm, nil
		}
		if strings.HasSuffix(s, "*") {
			out = append(out, routePattern{
				raw:      s,
				prefix:   strings.TrimSuffix(s, "*"),
				isPrefix: true,
			})
			continue
		}
		out = append(out, routePattern{raw: s})
	}
	rm.patterns = out
	return rm, nil
}

func (rm routeMatcher) matches(path string) bool {
	if rm.all {
		return true
	}
	for _, p := range rm.patterns {
		if p.isPrefix {
			if strings.HasPrefix(path, p.prefix) {
				return true
			}
			continue
		}
		if path == p.raw {
			return true
		}
	}
	return false
}

// --- registration ---------------------------------------------------

func init() {
	v2.RegisterBuiltin(Type, func(cfg map[string]any) (any, error) {
		return New(cfg)
	})
}
