// Client classification per PHILOSOPHY § 1 (named header fields only,
// no body sniff) and § 7 (small protocol surface, new kinds add a row
// to the taxonomy table — no general-purpose UA parser).
//
// First match wins. ClientInfo zero value means "no rule matched".

package parser

import (
	"net/http"
	"strings"

	"github.com/2nd1st/api-log/internal/trace"
)

// ClientInfo is the result of ExtractClient. Both fields nil when no
// rule matches; Version nil when the matched rule's discriminator
// doesn't expose a version.
type ClientInfo struct {
	Kind    *string
	Version *string
}

// ExtractClient runs the taxonomy classifier against h. It never
// panics — a nil or empty header map yields the zero ClientInfo.
func ExtractClient(h trace.Headers) ClientInfo {
	hh := http.Header(h)

	// 1. claude-code-desktop: Anthropic-Client-Platform=desktop_app
	//    AND Anthropic-Beta contains a claude-code-* token.
	if strings.EqualFold(hh.Get("Anthropic-Client-Platform"), "desktop_app") &&
		hasClaudeCodeBeta(hh.Get("Anthropic-Beta")) {
		ver := strings.TrimSpace(hh.Get("Anthropic-Client-Version"))
		return clientInfo("claude-code-desktop", ver)
	}

	ua := hh.Get("User-Agent")

	// 2. claude-cli: UA ^claude-cli/
	if ver, ok := uaSuffix(ua, "claude-cli/"); ok {
		return clientInfo("claude-cli", ver)
	}

	// 3-6. x-stainless-* SDK rules.
	pkgVer := hh.Get("x-stainless-package-version")
	runtime := strings.ToLower(strings.TrimSpace(hh.Get("x-stainless-runtime")))
	if pkgVer != "" {
		pkg, ver := splitPackageVersion(pkgVer)
		switch pkg {
		case "anthropic":
			switch runtime {
			case "python":
				return clientInfo("anthropic-sdk-python", ver)
			case "node":
				return clientInfo("anthropic-sdk-ts", ver)
			}
		case "openai":
			switch runtime {
			case "python":
				return clientInfo("openai-sdk-python", ver)
			case "node":
				return clientInfo("openai-sdk-ts", ver)
			}
		}
	}

	// 7a. codex-tui: UA ^codex-tui/  (OpenAI's terminal UI for codex)
	//      Must come BEFORE the codex-cli rule so the more specific prefix
	//      wins.
	if ver, ok := uaSuffix(ua, "codex-tui/"); ok {
		return clientInfo("codex-tui", ver)
	}

	// 7b. codex-cli: UA ^codex/  (the older CLI; also catches alternative
	//      packagings that didn't pick a more specific prefix).
	if ver, ok := uaSuffix(ua, "codex/"); ok {
		return clientInfo("codex-cli", ver)
	}

	// 8a. opencode-tui: UA ^opencode-tui/  (parallel to codex-tui shape).
	if ver, ok := uaSuffix(ua, "opencode-tui/"); ok {
		return clientInfo("opencode-tui", ver)
	}

	// 8b. opencode-cli: UA ^opencode/
	if ver, ok := uaSuffix(ua, "opencode/"); ok {
		return clientInfo("opencode-cli", ver)
	}

	// 9. browser: UA starts with Mozilla/ (no version captured).
	if strings.HasPrefix(ua, "Mozilla/") {
		kind := "browser"
		return ClientInfo{Kind: &kind}
	}

	// 10. go-http-client: UA ^Go-http-client/
	if ver, ok := uaSuffix(ua, "Go-http-client/"); ok {
		return clientInfo("go-http-client", ver)
	}

	return ClientInfo{}
}

// uaSuffix returns the trimmed version substring after prefix (which
// must end with "/") when ua starts with prefix. The boolean is true
// when the prefix matched, regardless of whether the version is empty —
// the caller decides whether an empty version is acceptable.
//
// When the trimmed suffix is empty we still return ok=true so that a
// bare "claude-cli/" UA still classifies as claude-cli (Version nil).
func uaSuffix(ua, prefix string) (string, bool) {
	if !strings.HasPrefix(ua, prefix) {
		return "", false
	}
	rest := strings.TrimSpace(ua[len(prefix):])
	// Cut at first whitespace so " 1.2.3 (extra)" -> "1.2.3".
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		rest = rest[:i]
	}
	return rest, true
}

// hasClaudeCodeBeta reports whether the comma-separated Anthropic-Beta
// header contains any token starting with "claude-code-".
func hasClaudeCodeBeta(beta string) bool {
	if beta == "" {
		return false
	}
	for _, tok := range strings.Split(beta, ",") {
		if strings.HasPrefix(strings.TrimSpace(tok), "claude-code-") {
			return true
		}
	}
	return false
}

// splitPackageVersion splits "<pkg>@<ver>" on the first "@". Returns
// ("", "") when the input has no "@" or starts with "@".
func splitPackageVersion(v string) (pkg, ver string) {
	i := strings.IndexByte(v, '@')
	if i <= 0 {
		return "", ""
	}
	return v[:i], v[i+1:]
}

// clientInfo builds a ClientInfo with a non-nil Kind and a Version
// pointer that is nil when ver is empty.
func clientInfo(kind, ver string) ClientInfo {
	out := ClientInfo{Kind: &kind}
	if ver != "" {
		out.Version = &ver
	}
	return out
}
