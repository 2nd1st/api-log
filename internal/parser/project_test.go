package parser

import (
	"encoding/json"
	"testing"

	"github.com/2nd1st/api-log/internal/trace"
)

// TestExtractProjectContext ports the 8 viewer cases from
// api-log-viewer/src/lib/promptSource.test.ts so backend extraction
// stays bit-for-bit aligned with what the UI used to compute at render
// time.
func TestExtractProjectContext(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  ProjectContext
	}{
		{
			// Shape: Claude Code user-turn system reminder (the actual
			// injection format the harness uses to drop CLAUDE.md
			// content into the prompt).
			name: "CLAUDE.md path-prefixed ref + heading on next line",
			input: "Contents of /Users/example/.claude/CLAUDE.md (project instructions, checked into the codebase):\n\n" +
				"# example-project\n\n## Section",
			want: ProjectContext{Name: "example-project", Source: "claude-md"},
		},
		{
			// Shape: codex variant with AGENTS.md absolute-path mention.
			name:  "AGENTS.md path-prefixed ref + adjacent heading",
			input: "Loaded /home/leif/proj/AGENTS.md (governs this repo):\n\n# acme-billing\n\nProject overview.",
			want:  ProjectContext{Name: "acme-billing", Source: "agents-md"},
		},
		{
			// Restraint check: a *prose* mention of CLAUDE.md must NOT
			// trigger the claude-md branch (FILE_REF_RE requires a
			// path-sep or "Contents of"). Strategy 1 misses; strategy 2
			// fires and picks "# System" — the viewer documents this
			// vendor-heading pick as accepted best-effort.
			name: "prose mention 'like CLAUDE.md' does not trigger claude-md",
			input: "# System\n\nyou are claude code.\n\n" +
				"you should respect durable instructions like CLAUDE.md files, but only when authorized.",
			want: ProjectContext{Name: "System", Source: "first-heading"},
		},
		{
			// First-heading fallback: a plain L2-only AGENTS.md style
			// prompt that starts directly with a `# Name`. No file ref.
			name:  "first heading only",
			input: "# my-cool-project\n\nDo not run `rm -rf /`.",
			want:  ProjectContext{Name: "my-cool-project", Source: "first-heading"},
		},
		{
			name:  "empty string → zero value",
			input: "",
			want:  ProjectContext{},
		},
		{
			name:  "whitespace-only → zero value",
			input: "   \n\t  \n",
			want:  ProjectContext{},
		},
		{
			name:  "no heading anywhere → zero value",
			input: "just a prose paragraph with no markdown headings at all.",
			want:  ProjectContext{},
		},
		{
			// Codex inline-code project name. `# \`my-project\`` should
			// surface as "my-project" — bare, not wrapped in backticks.
			name:  "inline backticks stripped",
			input: "# `my-project`\n\nReadme body.",
			want:  ProjectContext{Name: "my-project", Source: "first-heading"},
		},
		{
			// Mixed L1+L2: codex XML wrapper ends, then L2 heading
			// begins. The leading `</personality>` close should be
			// skipped so the first-heading branch still picks
			// "edge-router".
			name:  "heading after leading XML close tag",
			input: "</personality>\n\n# edge-router\n\nProject context body.",
			want:  ProjectContext{Name: "edge-router", Source: "first-heading"},
		},
		// Extra case beyond the viewer ports: HTML entities decoded.
		// The decode is an addition over the viewer (the viewer only
		// decodes inside extractSkills), but the contract change is
		// scoped to project names so the existing ports stay unaffected.
		{
			name:  "HTML entity decode in heading",
			input: "# diff&lt;file&gt;\n\nbody.",
			want:  ProjectContext{Name: "diff<file>", Source: "first-heading"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractProjectContext(c.input)
			if got != c.want {
				t.Errorf("ExtractProjectContext(%q)\n got  %+v\n want %+v", c.input, got, c.want)
			}
		})
	}
}

// TestExtractSystemPrompt verifies the protocol-dispatched
// request-body field reads for the three protocols whose contracts
// name a system-prompt path. Both the string AND array-of-blocks
// shapes are exercised — Claude Code → /v1/messages is overwhelmingly
// array-shaped in production, so a string-only handler would silently
// drop the most important real-world fixture.
func TestExtractSystemPrompt(t *testing.T) {
	mkReq := func(path string, body any) trace.Trace {
		b, _ := json.Marshal(body)
		return trace.Trace{Path: path, Req: trace.Body{Body: b}}
	}

	t.Run("chat string content", func(t *testing.T) {
		tr := mkReq("/v1/chat/completions", map[string]any{
			"messages": []map[string]any{
				{"role": "system", "content": "# sysproj\n\nbody"},
				{"role": "user", "content": "hi"},
			},
		})
		got := ExtractSystemPrompt(tr)
		want := "# sysproj\n\nbody"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("chat array-of-parts content", func(t *testing.T) {
		tr := mkReq("/v1/chat/completions", map[string]any{
			"messages": []map[string]any{
				{"role": "system", "content": []map[string]any{
					{"type": "text", "text": "L1 vendor"},
					{"type": "text", "text": "# proj-x"},
				}},
			},
		})
		got := ExtractSystemPrompt(tr)
		want := "L1 vendor\n\n# proj-x"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("messages system as string", func(t *testing.T) {
		tr := mkReq("/v1/messages", map[string]any{
			"system":   "# direct-proj\n\nbody",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		got := ExtractSystemPrompt(tr)
		want := "# direct-proj\n\nbody"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("messages system as array of blocks (canonical Claude Code shape)", func(t *testing.T) {
		// The dominant real-world fixture: Claude Code sends `system`
		// as an array of {type:"text",text:...,cache_control:...}
		// blocks. A string-only reader would extract "" here.
		tr := mkReq("/v1/messages", map[string]any{
			"system": []map[string]any{
				{"type": "text", "text": "You are Claude Code...",
					"cache_control": map[string]any{"type": "ephemeral"}},
				{"type": "text", "text": "Contents of /Users/example/.claude/CLAUDE.md (project instructions):\n\n# my-repo\n"},
			},
		})
		got := ExtractSystemPrompt(tr)
		// Both blocks join with "\n\n", and the project regex will
		// then find "my-repo" via the CLAUDE.md ref.
		if !contains(got, "# my-repo") {
			t.Errorf("system text did not include project block; got %q", got)
		}
		// End-to-end pipe through ExtractProjectContext:
		pc := ExtractProjectContext(got)
		if pc.Name != "my-repo" || pc.Source != "claude-md" {
			t.Errorf("pipeline: got %+v, want {my-repo claude-md}", pc)
		}
	})

	t.Run("responses instructions only", func(t *testing.T) {
		tr := mkReq("/v1/responses", map[string]any{
			"instructions": "# resp-proj\n\nbody",
		})
		got := ExtractSystemPrompt(tr)
		want := "# resp-proj\n\nbody"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("responses input items with system role", func(t *testing.T) {
		tr := mkReq("/v1/responses", map[string]any{
			"input": []map[string]any{
				{"role": "system", "content": []map[string]any{
					{"type": "input_text", "text": "# resp-input-proj"},
				}},
				{"role": "user", "content": "go"},
			},
		})
		got := ExtractSystemPrompt(tr)
		want := "# resp-input-proj"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("unknown protocol returns empty", func(t *testing.T) {
		tr := mkReq("/v1beta/models/gemini-2.0-flash:generateContent", map[string]any{
			"contents": []map[string]any{{"parts": []map[string]any{{"text": "# x"}}}},
		})
		if got := ExtractSystemPrompt(tr); got != "" {
			t.Errorf("gemini path should be empty, got %q", got)
		}
	})

	t.Run("empty body returns empty", func(t *testing.T) {
		tr := trace.Trace{Path: "/v1/chat/completions"}
		if got := ExtractSystemPrompt(tr); got != "" {
			t.Errorf("empty body should return empty, got %q", got)
		}
	})
}

func contains(haystack, needle string) bool {
	// minimal inline contains check so this test file imports no extra
	// packages beyond what the production file already pulls in
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
