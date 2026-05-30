package v2

import (
	"fmt"
	"strings"
)

// RouteMatcher is a precompiled list of route patterns. Identical
// semantics to internal/plugin/builtin/pathfilter:
//
//   - empty input list: match every path.
//   - exact: pattern equals path.
//   - trailing "*": strip the star, HasPrefix against the path.
//   - lone "*" or empty list: match every path.
//
// Built once at plugin Init via CompileRoutes; Matches is called per
// request, allocation-free.
type RouteMatcher struct {
	all      bool
	patterns []routePattern
}

type routePattern struct {
	raw      string
	prefix   string
	isPrefix bool
}

// CompileRoutes parses an operator-supplied pattern list. Empty / nil
// input matches all paths. Empty-string entries are rejected so a YAML
// typo surfaces at Init rather than silently match everything.
func CompileRoutes(raw []string) (RouteMatcher, error) {
	rm := RouteMatcher{}
	if len(raw) == 0 {
		rm.all = true
		return rm, nil
	}
	out := make([]routePattern, 0, len(raw))
	for i, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			return RouteMatcher{}, fmt.Errorf("routes[%d] is empty", i)
		}
		if s == "*" {
			// Lone "*" means "all paths" — collapse to avoid a per-call
			// HasPrefix("") that always returns true.
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

// Matches reports whether path is covered by the compiled list.
func (rm RouteMatcher) Matches(path string) bool {
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

// MatchRoute is a convenience for callers that don't want to hold a
// compiled matcher (one-shot uses, tests). Identical semantics to
// CompileRoutes + Matches; allocates per call.
func MatchRoute(routes []string, path string) bool {
	rm, err := CompileRoutes(routes)
	if err != nil {
		// An empty pattern in the list is a configuration bug. Fail
		// closed (no match) so callers don't get accidental wide
		// matches from a typo.
		return false
	}
	return rm.Matches(path)
}
