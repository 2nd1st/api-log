package v2

import "testing"

func TestCompileRoutes_EmptyMatchesAll(t *testing.T) {
	rm, err := CompileRoutes(nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !rm.Matches("/v1/messages") {
		t.Errorf("empty routes should match every path")
	}
	if !rm.Matches("/anything") {
		t.Errorf("empty routes should match arbitrary path")
	}
}

func TestCompileRoutes_LoneStarMatchesAll(t *testing.T) {
	rm, err := CompileRoutes([]string{"*"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !rm.Matches("/v1/messages") {
		t.Errorf("lone star should match every path")
	}
}

func TestCompileRoutes_ExactMatch(t *testing.T) {
	rm, err := CompileRoutes([]string{"/v1/messages"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !rm.Matches("/v1/messages") {
		t.Errorf("exact match miss")
	}
	if rm.Matches("/v1/messages/123") {
		t.Errorf("exact match should not match longer path")
	}
	if rm.Matches("/v1/chat/completions") {
		t.Errorf("exact match should not cross routes")
	}
}

func TestCompileRoutes_PrefixMatch(t *testing.T) {
	rm, err := CompileRoutes([]string{"/v1/*"})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !rm.Matches("/v1/messages") {
		t.Errorf("prefix match miss")
	}
	if !rm.Matches("/v1/anything/here") {
		t.Errorf("prefix should cover deeper paths")
	}
	if rm.Matches("/v2/messages") {
		t.Errorf("prefix should not match across prefix boundary")
	}
}

func TestCompileRoutes_MixedPatterns(t *testing.T) {
	rm, err := CompileRoutes([]string{
		"/v1/messages",
		"/v1/responses/*",
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !rm.Matches("/v1/messages") {
		t.Errorf("exact should match")
	}
	if !rm.Matches("/v1/responses/abc") {
		t.Errorf("prefix should match nested")
	}
	if rm.Matches("/v1/messages/extra") {
		t.Errorf("exact pattern should not match longer path")
	}
}

func TestCompileRoutes_RejectsEmptyEntry(t *testing.T) {
	if _, err := CompileRoutes([]string{"/v1/messages", ""}); err == nil {
		t.Errorf("empty pattern should fail compile (avoid silent wide match)")
	}
}

func TestMatchRoute_OneShot(t *testing.T) {
	if !MatchRoute(nil, "/anything") {
		t.Errorf("nil routes should match")
	}
	if !MatchRoute([]string{"/v1/*"}, "/v1/messages") {
		t.Errorf("prefix should match")
	}
	if MatchRoute([]string{"/v1/messages"}, "/v1/chat/completions") {
		t.Errorf("exact should not match different path")
	}
	if MatchRoute([]string{""}, "/v1/messages") {
		t.Errorf("invalid routes should fail closed")
	}
}
