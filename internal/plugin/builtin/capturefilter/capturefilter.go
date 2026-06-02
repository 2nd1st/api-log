// Package capturefilter implements the v0.1.2 "capture-filter" path
// gate: BEFORE the recording pipeline allocates a trace ID or wires
// the per-trace capture sinks, the proxy handler asks this filter
// whether the request path is in the operator-configured drop list.
// If yes, the request is forwarded upstream WITHOUT capture — no
// JSONL line, no SQLite row, no media extraction, no media tee
// reading bytes off the wire.
//
// Why a separate package from pathfilter (which already exists as an
// observer-class drop plugin):
//
//   - pathfilter runs at finalize (after capture sinks have already
//     run, after the body is in memory) and marks the trace as
//     "do not record". The bytes are still captured + thrown away.
//   - capturefilter runs in the proxy handler BEFORE startTrace().
//     The request flows through the proxy unaltered but the
//     CaptureTransport's tee never fires (no trace ID in context →
//     SinksFor returns nil → bytes pass through unwrapped).
//
// Why both exist (sub2gpt + sub2api already ship a pathfilter
// `patterns` block):
//
//   - Existing operator deployments configured `plugins.path_filter`
//     get the same observer semantics they had pre-v0.1.2. Their YAML
//     is not silently re-interpreted as "drop before capture", which
//     would be a breaking behavioral change.
//   - New `plugins.capture_filter` is opt-in — operators flip it on
//     to skip writes for high-volume polling endpoints (e.g. an admin
//     UI's /api/v1/auth/me + /api/v1/subscriptions/active polls that
//     pollute the data dir without LLM relevance).
//
// Pattern syntax is intentionally identical to pathfilter (exact +
// trailing-`*` prefix). Operators copy the same pattern string
// between the two blocks if they want both display-hide AND
// pre-write drop on the same paths.
package capturefilter

import (
	"github.com/2nd1st/api-log/internal/plugin/builtin/pathfilter"
)

// Config is the YAML subtree under `plugins.capture_filter`. Mirrors
// pathfilter.Config so the YAML shape stays consistent across the
// two filter surfaces.
type Config = pathfilter.Config

// Filter is the standalone gate the proxy handler calls per request.
// Constructed once at startup via New; nil-safe — `(*Filter).ShouldDrop`
// on a nil receiver returns false, so callers can treat "no filter
// configured" identically to "filter configured but empty".
type Filter struct {
	patterns []pathfilter.Pattern
}

// New compiles cfg.Patterns into a Filter. Returns (nil, nil) when no
// patterns are configured — callers wire the same nil-safe Filter
// regardless of whether the operator opted in. Validation matches
// pathfilter (refuses empty strings + the lone `*`).
func New(cfg Config) (*Filter, error) {
	compiled, err := pathfilter.Compile(cfg.Patterns)
	if err != nil {
		return nil, err
	}
	if len(compiled) == 0 {
		return nil, nil
	}
	return &Filter{patterns: compiled}, nil
}

// ShouldDrop returns true when path matches any configured pattern.
// Nil-safe: a nil Filter always returns false (no gate configured).
func (f *Filter) ShouldDrop(path string) bool {
	if f == nil {
		return false
	}
	return pathfilter.MatchAny(f.patterns, path)
}

// Patterns returns a flat slice of the operator-facing pattern strings.
// Used for /healthz + startup log emission so operators can confirm
// the configured drop list without reading the YAML back.
// Nil-safe.
func (f *Filter) Patterns() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.patterns))
	for _, p := range f.patterns {
		out = append(out, p.Raw())
	}
	return out
}
