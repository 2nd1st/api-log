// Package pathfilter implements the Phase A "path-filter" plugin:
// an ObserveOnFinalize plugin that drops traces whose Path matches
// any operator-configured pattern.
//
// PHASE A SCAFFOLD — this plugin builds, has tests, and is configurable,
// but is NOT wired into the writer pipeline yet (no Registry is
// constructed in cmd/api-log/main.go). Phase A.1 lands the wiring as
// its own commit; behavior shipped today is identical to behavior
// shipped before this commit.
//
// Pattern semantics match the display-side filter the viewer already
// uses (see ROADMAP § 4 "path noise filter" + commit 5cbdda1):
//
//   - Exact match: pattern "/api/v1/auth/me" matches Path
//     "/api/v1/auth/me" only.
//   - Prefix match via trailing "*": pattern "/api/v1/*" matches
//     "/api/v1", "/api/v1/auth/me", "/api/v1/anything".
//   - The lone pattern "*" disables the plugin (matches everything;
//     would drop every trace — operators almost certainly don't want
//     this, so we treat it as a config bug and reject it at Init).
//
// Why match the display-side semantics: the viewer's path input and the
// capture-side skip both want to express the same operator intent
// ("ignore admin UI polling on /api/v1/*"). Identical syntax means
// the operator copies the same string into both places. Glob libraries
// (path.Match, doublestar) would be over-engineered for the v0 surface
// — two cases is enough.
//
// See uiux-research/plugin.md § 7.1 for the design rationale.
package pathfilter

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiayangzhang/api-log/internal/trace"
)

// Name is the plugin identifier used in config and telemetry.
const Name = "path-filter"

// Config is the YAML subtree under plugins.config.path-filter.
//
// Defaults: zero value (Patterns: nil) is a no-op plugin that records
// every trace. The operator opts in by listing patterns.
type Config struct {
	// Patterns is the list of paths to drop from recording. Exact match
	// or trailing-"*" prefix match per the package doc.
	Patterns []string `yaml:"patterns"`
}

// pattern is a parsed Config.Patterns entry.
type pattern struct {
	raw      string // operator-facing form, kept for log/error context
	prefix   string // body of the pattern when isPrefix=true
	isPrefix bool   // true when the operator pattern ended in "*"
}

func (p pattern) matches(path string) bool {
	if p.isPrefix {
		return strings.HasPrefix(path, p.prefix)
	}
	return path == p.raw
}

// Plugin is the path-filter implementation. It satisfies
// plugin.Plugin and plugin.ObserveOnFinalize. Construction is
// trivial; the patterns are compiled in Init.
type Plugin struct {
	patterns []pattern
}

// New returns an uninitialized Plugin. The caller (Registry) MUST call
// Init before invoking OnFinalize — calling OnFinalize on a fresh
// Plugin yields shouldRecord=true (no patterns means nothing matches).
func New() *Plugin {
	return &Plugin{}
}

// Name returns the constant plugin identifier.
func (p *Plugin) Name() string { return Name }

// Init parses cfg into compiled patterns. cfg is the raw map form
// produced by YAML unmarshaling at the plugin.Registry layer.
//
// Accepted shape:
//
//	{"patterns": ["/api/v1/*", "/api/v1/auth/me"]}
//
// nil cfg = no patterns = the plugin is a no-op (records everything).
// This matches PHILOSOPHY principle 5 ("one process, one config"): an
// absent block is the same as an explicit empty block.
//
// Returns an error on:
//   - Wrong type for "patterns" (not a list or list elements not strings).
//   - The lone pattern "*" (operator config bug — would drop every trace).
func (p *Plugin) Init(cfg map[string]any) error {
	if cfg == nil {
		p.patterns = nil
		return nil
	}
	rawPatterns, ok := cfg["patterns"]
	if !ok {
		p.patterns = nil
		return nil
	}
	list, ok := rawPatterns.([]any)
	if !ok {
		return fmt.Errorf("path-filter: patterns must be a list, got %T", rawPatterns)
	}
	compiled := make([]pattern, 0, len(list))
	for i, entry := range list {
		s, ok := entry.(string)
		if !ok {
			return fmt.Errorf("path-filter: patterns[%d] must be a string, got %T", i, entry)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return fmt.Errorf("path-filter: patterns[%d] is empty", i)
		}
		if s == "*" {
			return fmt.Errorf("path-filter: patterns[%d] = \"*\" would drop every trace; refusing to start", i)
		}
		if strings.HasSuffix(s, "*") {
			compiled = append(compiled, pattern{
				raw:      s,
				prefix:   strings.TrimSuffix(s, "*"),
				isPrefix: true,
			})
		} else {
			compiled = append(compiled, pattern{raw: s})
		}
	}
	p.patterns = compiled
	return nil
}

// Close is a no-op for path-filter; there is no buffered state.
func (p *Plugin) Close() error { return nil }

// OnFinalize returns shouldRecord=false when tr.Path matches any of
// the configured patterns, shouldRecord=true otherwise. The error
// return is always nil for path-filter — pattern matching cannot fail
// at runtime once patterns compile cleanly in Init.
//
// The context is accepted to honor the plugin interface contract but
// is not consulted; matching is non-blocking and cheap.
//
// Renamed from BeforeRecord per plugin-b-c-spec §7.3 (Phase A migration).
func (p *Plugin) OnFinalize(_ context.Context, tr trace.Trace) (bool, error) {
	for _, pat := range p.patterns {
		if pat.matches(tr.Path) {
			return false, nil
		}
	}
	return true, nil
}
