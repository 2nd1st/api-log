// Package textappend implements the BUILD-phase "text-append" plugin
// described in uiux-research/plugin-b-c-spec.md §7.2, with the R1
// streaming redesign (append-to-last-delta, not new-block-synthesis).
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
// Streaming-AFTER for Down.Target = "content" registers an
// OnLastTextDelta callback. The framework's StreamDispatcher buffers
// one event of lookahead per content block and fires the callback on
// the LAST text_delta of each content block, producing a wire
// sequence that ends ...text_delta(text+suffix), content_block_stop,
// ..., message_stop — with no synthesized event after the protocol's
// terminator. Streaming-AFTER for Down.Target = "reasoning" is a v1
// no-op (the dispatcher has no reasoning-last-delta hook); operators
// wanting reasoning suffixes use non-streaming responses.
//
// Block-index policy: the callback is registered as index-agnostic
// (fires on every text content block). The plain "last block only"
// reading of the task spec was rejected because Anthropic extended-
// thinking responses put thinking at content_block index 0 and the
// assistant text at index 1; a strict `idx == 0` gate would skip
// the suffix on every thinking-enabled response. Consequence:
// responses that emit MULTIPLE text content blocks (rare but legal —
// e.g. a tool_use sandwiched between text blocks) get the suffix
// appended once per text block. Operators who specifically need
// "only the first text block" gate on their own logic via a chained
// OnLastTextDelta (the block index is passed to the callback).
//
// Probability lets operators dial trigger frequency for easter-egg
// modes. Absent (nil) means 1.0 ("always"); 0.0 means "never". Each
// hook call draws a math/rand/v2 float; the suffix lands only when
// the draw is below Probability.
package textappend

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"

	v2 "github.com/xiayangzhang/api-log/internal/plugin/v2"
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
//
// Probability is a pointer so absent in JSON is distinguishable from
// 0.0 (which would mean "never trigger"). Valid range is [0.0, 1.0];
// values outside the range fail Init.
type Config struct {
	Routes      []string `json:"routes"`
	Up          UpRule   `json:"up"`
	Down        DownRule `json:"down"`
	Probability *float64 `json:"probability,omitempty"`
}

// randFunc is the source of the per-call probability draw. Default
// math/rand/v2.Float64 is process-global and goroutine-safe; tests
// inject a deterministic source to make probability=0.5 assertions
// stable.
type randFunc func() float64

// Plugin implements both v2.BeforePlugin and v2.AfterPlugin. Either side
// is a no-op when its Suffix is empty.
type Plugin struct {
	cfg    Config
	routes v2.RouteMatcher
	rand   randFunc
}

// New parses cfg, applies target defaults, validates enums, and returns
// a ready Plugin. The default RNG is math/rand/v2.Float64; tests use
// NewWithRand to inject a deterministic source.
func New(cfg map[string]any) (*Plugin, error) {
	return NewWithRand(cfg, rand.Float64)
}

// NewWithRand is the test seam: lets a unit test supply a deterministic
// rand.Float64 stand-in so probability assertions don't depend on the
// process-global RNG. Production callers use New.
func NewWithRand(cfg map[string]any, r randFunc) (*Plugin, error) {
	parsed, err := decodeConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("text-append: %w", err)
	}
	rm, err := v2.CompileRoutes(parsed.Routes)
	if err != nil {
		return nil, fmt.Errorf("text-append: %w", err)
	}
	if r == nil {
		r = rand.Float64
	}
	return &Plugin{cfg: parsed, routes: rm, rand: r}, nil
}

// shouldFire reports whether this call should mutate. With no
// Probability set (the default), always returns true. With a pointer
// set, returns rand() < *Probability — so Probability=0.0 never fires
// and Probability=1.0 always fires.
func (p *Plugin) shouldFire() bool {
	if p.cfg.Probability == nil {
		return true
	}
	pr := *p.cfg.Probability
	if pr >= 1.0 {
		return true
	}
	if pr <= 0.0 {
		return false
	}
	return p.rand() < pr
}

// Name returns the stable type identifier.
func (p *Plugin) Name() string { return Type }

// OnBefore appends Up.Suffix to either the trailing user turn's text or
// to SystemPrompt, per Up.Target. Returns ActionContinue (no rebuild)
// when:
//   - the route does not match
//   - Up.Suffix is empty
//   - target == last_user_message but no user turn carries text content
//   - the per-call probability draw does not fire (operator-configured
//     easter-egg mode; default Probability nil = always fires)
func (p *Plugin) OnBefore(_ context.Context, req *v2.ParsedRequest, _ map[string]any) v2.BeforeResult {
	if req == nil {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	if !p.routes.Matches(req.Path) {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	if p.cfg.Up.Suffix == "" {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	if !p.shouldFire() {
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
//     register an OnLastTextDelta callback. The dispatcher buffers one
//     event of lookahead per content block and fires the callback on
//     the LAST text_delta of the first text block (block index 0 by
//     default). The wire sequence ends with the mutated delta, then
//     the protocol's per-block terminator, then any tool_use blocks,
//     then the protocol's overall terminal — no synthesized event
//     follows the protocol's terminator (this is the R1 fix vs.
//     EmitBeforeFinish, which appended a new block after the
//     terminator).
//
//   - Streaming with Down.Target == "reasoning": no-op (dispatcher
//     does not expose a reasoning-last-delta hook in v1). Operators
//     wanting reasoning suffixes use non-streaming responses.
//
//   - Non-streaming: append the suffix to ac.Response.Content (or
//     .Reasoning) and return ActionMutate. Shallow clone is safe — only
//     string fields move.
//
// Returns ActionContinue when the route does not match, Down.Suffix
// is empty, or the per-call probability draw does not fire. The
// non-streaming branch always Mutates when those checks pass —
// append is unconditional and operators who chain instances see
// additive suffixes (idempotence is not promised).
//
// Probability note: in the streaming branch the draw happens at hook
// registration time, not per-event — the callback either runs or
// doesn't for the whole stream. This matches operator intent for
// easter-egg mode: a stream either gets the footer or doesn't.
func (p *Plugin) OnAfter(_ context.Context, req *v2.ParsedRequest, ac *v2.AfterContext, _ map[string]any) v2.AfterResult {
	if req == nil || ac == nil {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if !p.routes.Matches(req.Path) {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if p.cfg.Down.Suffix == "" {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if !p.shouldFire() {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if ac.Response == nil {
		// Streaming branch.
		if p.cfg.Down.Target == TargetReasoning {
			// No reasoning-last-delta hook in v1; document and skip.
			return v2.AfterResult{Action: v2.ActionContinue}
		}
		suffix := p.cfg.Down.Suffix
		ac.OnLastTextDelta(func(_ int, text string) string {
			return text + suffix
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
	if c.Probability != nil {
		v := *c.Probability
		// NaN fails every numeric comparison; explicit NaN check first
		// so a config typo doesn't silently coerce to "never fire."
		if v != v || v < 0.0 || v > 1.0 {
			return c, fmt.Errorf("probability = %v (must be in [0.0, 1.0])", v)
		}
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

// --- registration ---------------------------------------------------

func init() {
	v2.RegisterBuiltin(Type, func(cfg map[string]any) (any, error) {
		return New(cfg)
	})
}
