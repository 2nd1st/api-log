package pathfilter

import (
	"context"
	"testing"

	"github.com/2nd1st/api-log/internal/trace"
)

func TestInit_NilCfg(t *testing.T) {
	p := New()
	if err := p.Init(nil); err != nil {
		t.Fatalf("Init(nil): %v", err)
	}
	got, err := p.OnFinalize(context.Background(), trace.Trace{Path: "/anything"})
	if err != nil {
		t.Fatalf("OnFinalize: %v", err)
	}
	if !got {
		t.Error("no patterns should record every trace; got shouldRecord=false")
	}
}

func TestInit_MissingPatternsKey(t *testing.T) {
	p := New()
	if err := p.Init(map[string]any{}); err != nil {
		t.Fatalf("Init({}): %v", err)
	}
	got, _ := p.OnFinalize(context.Background(), trace.Trace{Path: "/v1/messages"})
	if !got {
		t.Error("missing patterns key should record every trace")
	}
}

func TestInit_RejectsBadShape(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]any
	}{
		{"patterns not a list", map[string]any{"patterns": "not-a-list"}},
		{"list element not a string", map[string]any{"patterns": []any{42}}},
		{"empty pattern", map[string]any{"patterns": []any{""}}},
		{"lone star", map[string]any{"patterns": []any{"*"}}},
		{"whitespace becomes empty", map[string]any{"patterns": []any{"   "}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := New()
			if err := p.Init(tc.cfg); err == nil {
				t.Fatalf("Init(%v) returned nil error, want non-nil", tc.cfg)
			}
		})
	}
}

func TestOnFinalize_Matching(t *testing.T) {
	tests := []struct {
		name             string
		patterns         []any
		path             string
		wantShouldRecord bool
	}{
		// Exact match
		{
			name:             "exact_match_drops",
			patterns:         []any{"/api/v1/auth/me"},
			path:             "/api/v1/auth/me",
			wantShouldRecord: false,
		},
		{
			name:             "exact_pattern_does_not_prefix_match",
			patterns:         []any{"/api/v1"},
			path:             "/api/v1/auth/me",
			wantShouldRecord: true,
		},
		{
			name:             "exact_non_match_records",
			patterns:         []any{"/api/v1/auth/me"},
			path:             "/api/v1/auth/other",
			wantShouldRecord: true,
		},
		// Prefix match (trailing *)
		{
			name:             "prefix_match_drops_subpath",
			patterns:         []any{"/api/v1/*"},
			path:             "/api/v1/auth/me",
			wantShouldRecord: false,
		},
		{
			name:             "prefix_match_drops_exact_prefix_body",
			patterns:         []any{"/api/v1/*"},
			path:             "/api/v1/",
			wantShouldRecord: false,
		},
		{
			name:             "prefix_match_drops_prefix_root_without_slash",
			patterns:         []any{"/api/v1/*"},
			path:             "/api/v1", // strings.HasPrefix("/api/v1", "/api/v1/") is false; should record
			wantShouldRecord: true,
		},
		{
			name:             "prefix_non_match_records",
			patterns:         []any{"/api/v1/*"},
			path:             "/v1/messages",
			wantShouldRecord: true,
		},
		// Multiple patterns: any match drops
		{
			name:             "second_pattern_matches",
			patterns:         []any{"/never/match", "/api/v1/*"},
			path:             "/api/v1/admin",
			wantShouldRecord: false,
		},
		{
			name:             "no_pattern_matches",
			patterns:         []any{"/never/match", "/also/never/*"},
			path:             "/v1/messages",
			wantShouldRecord: true,
		},
		// Real-world patterns from ROADMAP § 4
		{
			name:             "sub2api_admin_polling_dropped",
			patterns:         []any{"/api/v1/*"},
			path:             "/api/v1/auth/me?timezone=America/Los_Angeles",
			wantShouldRecord: false,
		},
		{
			name:             "anthropic_messages_kept",
			patterns:         []any{"/api/v1/*"},
			path:             "/v1/messages",
			wantShouldRecord: true,
		},
		{
			name:             "openai_responses_kept",
			patterns:         []any{"/api/v1/*"},
			path:             "/v1/responses",
			wantShouldRecord: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := New()
			if err := p.Init(map[string]any{"patterns": tc.patterns}); err != nil {
				t.Fatalf("Init: %v", err)
			}
			got, err := p.OnFinalize(context.Background(), trace.Trace{Path: tc.path})
			if err != nil {
				t.Fatalf("OnFinalize: %v", err)
			}
			if got != tc.wantShouldRecord {
				t.Errorf("shouldRecord = %v, want %v (path=%q, patterns=%v)",
					got, tc.wantShouldRecord, tc.path, tc.patterns)
			}
		})
	}
}

func TestName_Stable(t *testing.T) {
	p := New()
	if p.Name() != Name {
		t.Errorf("Name() = %q, want %q", p.Name(), Name)
	}
	if Name != "path-filter" {
		t.Errorf("Name constant changed to %q; this is a config-breaking change", Name)
	}
}

func TestClose_NoOp(t *testing.T) {
	p := New()
	if err := p.Init(map[string]any{"patterns": []any{"/api/v1/*"}}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
