// Package textreplace implements the text-replace plugin described in
// docs/specs/plugin-b-c-spec.md.
//
// One plugin instance can serve BOTH directions: rules under "up" rewrite
// inbound request content before forwarding, rules under "down" rewrite
// outbound response content before it reaches the client. Either side
// may be empty — that side becomes a no-op while the other still runs.
//
// Matching is literal substring (strings.ReplaceAll). Regex / case-
// insensitive variants are deferred per spec §7.1 "Out of scope for MVP."
//
// Carve-out (spec §10.6) enforcement:
//   - BEFORE only touches Message ContentPart text payloads (Type=="text").
//     tool_use / tool_result / image / audio parts are left alone.
//   - AFTER streaming registers OnContentDelta only. tool_use
//     input_json_delta events are routed to a distinct class by the W1
//     classifier and never reach the registered content transform —
//     this plugin neither inspects nor mutates tool-call argument JSON.
package textreplace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v2 "github.com/2nd1st/api-log/internal/plugin/v2"
)

// Type is the operator-facing type name registered with v2.RegisterBuiltin.
// Stable wire string; do not change without a config migration.
const Type = "text-replace"

// Rule is one literal-substring replacement.
type Rule struct {
	Match   string `json:"match"`
	Replace string `json:"replace"`
}

// Config is the parsed instance config. The viewer Settings form (W4)
// reads ConfigSchema() to render this shape; YAML / runtime_overrides
// JSON encode into the same field names.
type Config struct {
	// Routes is a list of path patterns the plugin applies to. Empty list
	// means "all paths." Pattern semantics mirror pathfilter:
	//   - exact match: "/v1/messages"
	//   - trailing-"*" prefix: "/v1/*"
	Routes []string `json:"routes"`

	// Up is applied to request content before forwarding.
	Up []Rule `json:"up"`

	// Down is applied to response content before client send.
	Down []Rule `json:"down"`
}

// Plugin implements both v2.BeforePlugin and v2.AfterPlugin. A single
// instance can mutate request content (Up), response content (Down), or
// both — empty rule slices make the corresponding direction a no-op.
type Plugin struct {
	cfg    Config
	routes v2.RouteMatcher
}

// New parses cfg, validates it, and returns a ready Plugin. Used by the
// builtin Ctor and directly by tests (tests bypass the global registry
// so the package-level init's RegisterBuiltin call does not get hit
// twice).
func New(cfg map[string]any) (*Plugin, error) {
	parsed, err := decodeConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("text-replace: %w", err)
	}
	rm, err := v2.CompileRoutes(parsed.Routes)
	if err != nil {
		return nil, fmt.Errorf("text-replace: %w", err)
	}
	return &Plugin{cfg: parsed, routes: rm}, nil
}

// Name returns the stable type identifier.
func (p *Plugin) Name() string { return Type }

// OnBefore applies p.cfg.Up rules to text-content parts of every
// Message. Returns ActionContinue (without rebuilding the request) when:
//   - the route does not match
//   - there are no Up rules
//   - no text payload changed after substitution
//
// On match the returned ParsedRequest is a deep-enough clone that the
// caller's original is untouched (see spec §2.3 "treat ParsedRequest as
// logically read-only").
func (p *Plugin) OnBefore(_ context.Context, req *v2.ParsedRequest, _ map[string]any) v2.BeforeResult {
	if req == nil {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	if !p.routes.Matches(req.Path) {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	if len(p.cfg.Up) == 0 {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	mutated, changed := rewriteMessages(req.Messages, p.cfg.Up)
	if !changed {
		return v2.BeforeResult{Action: v2.ActionContinue}
	}
	out := *req
	out.Messages = mutated
	return v2.BeforeResult{Action: v2.ActionMutate, Mutated: &out}
}

// OnAfter has two branches:
//
//   - Streaming (ac.Response == nil): register an OnContentDelta
//     transform that applies each Down rule in declared order to the
//     content delta text. Tool-use deltas are routed to a separate class
//     by W1's classifier and never reach this transform — the §10.6
//     carve-out is enforced at the framework layer; no defensive guard
//     is needed here.
//
//   - Non-streaming (ac.Response != nil): walk the rules over
//     ParsedResponse.Content. If anything changed return ActionMutate
//     with a shallow clone (Content / Reasoning are strings; no slice
//     aliasing is at risk).
//
// Returns ActionContinue when the route does not match, Down is empty,
// or no text changed.
func (p *Plugin) OnAfter(_ context.Context, req *v2.ParsedRequest, ac *v2.AfterContext, _ map[string]any) v2.AfterResult {
	if req == nil || ac == nil {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if !p.routes.Matches(req.Path) {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if len(p.cfg.Down) == 0 {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	if ac.Response == nil {
		// Streaming branch. Register a content-delta transform.
		rules := p.cfg.Down
		ac.OnContentDelta(func(text string) string {
			return applyRules(text, rules)
		})
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	// Non-streaming branch. Buffered response — replace Content in place.
	newContent := applyRules(ac.Response.Content, p.cfg.Down)
	if newContent == ac.Response.Content {
		return v2.AfterResult{Action: v2.ActionContinue}
	}
	resp := *ac.Response
	resp.Content = newContent
	return v2.AfterResult{Action: v2.ActionMutate, Mutated: &resp}
}

// ConfigSchema returns the form descriptor consumed by the viewer Settings UI.
// The field set here is plugin-specific and stable.
func (p *Plugin) ConfigSchema() Schema {
	return Schema{
		Fields: []SchemaField{
			{
				Name:        "routes",
				Label:       "Routes",
				Type:        "string_array",
				Description: "Path patterns to apply rules to. Empty = all paths. Trailing '*' = prefix match.",
			},
			{
				Name:        "up",
				Label:       "Request rules (upstream)",
				Type:        "rule_array",
				Description: "Literal substring replacements applied to inbound request message text.",
			},
			{
				Name:        "down",
				Label:       "Response rules (downstream)",
				Type:        "rule_array",
				Description: "Literal substring replacements applied to outbound response content text.",
			},
		},
	}
}

// Schema and SchemaField are the per-plugin form descriptor used by the
// viewer Settings panel (spec §8.6). The canonical project-wide shape
// is owned by W3; this local pair is the minimum surface W4 needs while
// W3's API package is still in flight. Once W3 publishes the canonical
// type, plugins migrate to it.
type Schema struct {
	Fields []SchemaField `json:"fields"`
}

type SchemaField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// --- internals ------------------------------------------------------

// decodeConfig round-trips the cfg map through json to populate the
// typed Config. This avoids hand-walking the map and gives us free
// type validation for the nested Rule structs.
func decodeConfig(cfg map[string]any) (Config, error) {
	var c Config
	if cfg == nil {
		return c, nil
	}
	buf, err := json.Marshal(cfg)
	if err != nil {
		return c, fmt.Errorf("bad config: %w", err)
	}
	if err := json.Unmarshal(buf, &c); err != nil {
		return c, fmt.Errorf("bad config: %w", err)
	}
	// Reject zero-length match. strings.ReplaceAll with match="" inserts
	// the replacement at every char boundary — almost certainly an
	// operator typo, not intent.
	for i, r := range c.Up {
		if r.Match == "" {
			return c, fmt.Errorf("up[%d].match is empty", i)
		}
	}
	for i, r := range c.Down {
		if r.Match == "" {
			return c, fmt.Errorf("down[%d].match is empty", i)
		}
	}
	return c, nil
}

// applyRules runs every rule in order over the given text. Returns the
// same string when no rule produced a change.
func applyRules(text string, rules []Rule) string {
	for _, r := range rules {
		if !strings.Contains(text, r.Match) {
			continue
		}
		text = strings.ReplaceAll(text, r.Match, r.Replace)
	}
	return text
}

// rewriteMessages produces a possibly-mutated copy of msgs with every
// text ContentPart run through applyRules. The original slice and its
// backing ContentPart slices are NOT aliased into the result; the
// caller's ParsedRequest is safe to retain unchanged.
//
// changed is true iff at least one text payload was modified. When
// false, the returned slice is the input slice (no copy made).
func rewriteMessages(msgs []v2.Message, rules []Rule) (out []v2.Message, changed bool) {
	for i := range msgs {
		// Pre-scan this turn's text parts; clone only when we know we
		// will mutate something. Avoids paying the copy cost on the
		// common case where the needle is absent.
		dirty := false
		for _, p := range msgs[i].Content {
			if p.Type != "text" {
				continue
			}
			if anyMatches(p.Text, rules) {
				dirty = true
				break
			}
		}
		if !dirty {
			continue
		}
		// Lazy-init the output slice on first dirty turn.
		if !changed {
			out = make([]v2.Message, len(msgs))
			copy(out, msgs)
			changed = true
		}
		// Clone this turn's content slice and rewrite text parts.
		parts := make([]v2.ContentPart, len(msgs[i].Content))
		copy(parts, msgs[i].Content)
		for j := range parts {
			if parts[j].Type != "text" {
				continue
			}
			parts[j].Text = applyRules(parts[j].Text, rules)
		}
		out[i].Content = parts
	}
	if !changed {
		return msgs, false
	}
	return out, true
}

func anyMatches(text string, rules []Rule) bool {
	for _, r := range rules {
		if strings.Contains(text, r.Match) {
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
