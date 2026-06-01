package parser

import (
	"net/http"
	"testing"

	"github.com/2nd1st/api-log/internal/trace"
)

func strPtr(s string) *string { return &s }

func TestExtractClient(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		want    ClientInfo
	}{
		{
			name: "claude-code-desktop",
			headers: http.Header{
				"Anthropic-Client-Platform": {"desktop_app"},
				"Anthropic-Beta":            {"claude-code-20250219,fine-grained-tool-streaming-2025-05-14"},
				"Anthropic-Client-Version":  {"1.9659.2"},
			},
			want: ClientInfo{Kind: strPtr("claude-code-desktop"), Version: strPtr("1.9659.2")},
		},
		{
			name: "claude-cli",
			headers: http.Header{
				"User-Agent": {"claude-cli/0.5.13"},
			},
			want: ClientInfo{Kind: strPtr("claude-cli"), Version: strPtr("0.5.13")},
		},
		{
			name: "anthropic-sdk-python",
			headers: http.Header{
				"X-Stainless-Package-Version": {"anthropic@0.39.0"},
				"X-Stainless-Runtime":         {"python"},
			},
			want: ClientInfo{Kind: strPtr("anthropic-sdk-python"), Version: strPtr("0.39.0")},
		},
		{
			name: "anthropic-sdk-ts",
			headers: http.Header{
				"X-Stainless-Package-Version": {"anthropic@0.27.0"},
				"X-Stainless-Runtime":         {"node"},
			},
			want: ClientInfo{Kind: strPtr("anthropic-sdk-ts"), Version: strPtr("0.27.0")},
		},
		{
			name: "openai-sdk-python",
			headers: http.Header{
				"X-Stainless-Package-Version": {"openai@1.55.0"},
				"X-Stainless-Runtime":         {"python"},
			},
			want: ClientInfo{Kind: strPtr("openai-sdk-python"), Version: strPtr("1.55.0")},
		},
		{
			name: "openai-sdk-ts",
			headers: http.Header{
				"X-Stainless-Package-Version": {"openai@4.73.1"},
				"X-Stainless-Runtime":         {"node"},
			},
			want: ClientInfo{Kind: strPtr("openai-sdk-ts"), Version: strPtr("4.73.1")},
		},
		{
			name: "codex-cli",
			headers: http.Header{
				"User-Agent": {"codex/0.42.0"},
			},
			want: ClientInfo{Kind: strPtr("codex-cli"), Version: strPtr("0.42.0")},
		},
		{
			// Representative codex-tui UA shape:
			// codex-tui/0.134.0 (Windows 10.0.26200; x86_64) WindowsTerminal (codex-tui; 0.134.0)
			// The codex-tui rule MUST come before codex-cli so the more
			// specific prefix wins.
			name: "codex-tui",
			headers: http.Header{
				"User-Agent": {"codex-tui/0.134.0 (Windows 10.0.26200; x86_64) WindowsTerminal (codex-tui; 0.134.0)"},
			},
			want: ClientInfo{Kind: strPtr("codex-tui"), Version: strPtr("0.134.0")},
		},
		{
			name: "opencode-cli",
			headers: http.Header{
				"User-Agent": {"opencode/0.2.3"},
			},
			want: ClientInfo{Kind: strPtr("opencode-cli"), Version: strPtr("0.2.3")},
		},
		{
			name: "opencode-tui",
			headers: http.Header{
				"User-Agent": {"opencode-tui/0.2.3"},
			},
			want: ClientInfo{Kind: strPtr("opencode-tui"), Version: strPtr("0.2.3")},
		},
		{
			name: "browser",
			headers: http.Header{
				"User-Agent": {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15.7) AppleWebKit/537.36"},
			},
			want: ClientInfo{Kind: strPtr("browser")},
		},
		{
			name: "go-http-client",
			headers: http.Header{
				"User-Agent": {"Go-http-client/2.0"},
			},
			want: ClientInfo{Kind: strPtr("go-http-client"), Version: strPtr("2.0")},
		},
		{
			name:    "none-empty",
			headers: http.Header{},
			want:    ClientInfo{},
		},
		{
			name: "none-unknown-ua",
			headers: http.Header{
				"User-Agent": {"curl/8.5.0"},
			},
			want: ClientInfo{},
		},
		{
			name: "defensive-desktop-without-beta",
			headers: http.Header{
				"Anthropic-Client-Platform": {"desktop_app"},
				"Anthropic-Client-Version":  {"1.9659.2"},
			},
			want: ClientInfo{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractClient(trace.Headers(tc.headers))
			if !strPtrEq(got.Kind, tc.want.Kind) {
				t.Errorf("Kind = %v, want %v", strPtrDeref(got.Kind), strPtrDeref(tc.want.Kind))
			}
			if !strPtrEq(got.Version, tc.want.Version) {
				t.Errorf("Version = %v, want %v", strPtrDeref(got.Version), strPtrDeref(tc.want.Version))
			}
		})
	}
}

func strPtrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func strPtrDeref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
