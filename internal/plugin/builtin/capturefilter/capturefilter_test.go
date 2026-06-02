package capturefilter

import "testing"

func TestNew_NoPatternsReturnsNil(t *testing.T) {
	cases := []Config{
		{},
		{Patterns: nil},
		{Patterns: []string{}},
	}
	for i, cfg := range cases {
		f, err := New(cfg)
		if err != nil {
			t.Errorf("case %d: unexpected err: %v", i, err)
		}
		if f != nil {
			t.Errorf("case %d: expected nil Filter, got %+v", i, f)
		}
	}
}

func TestNew_RejectsEmptyPattern(t *testing.T) {
	_, err := New(Config{Patterns: []string{"/api/v1/*", ""}})
	if err == nil {
		t.Error("empty pattern should error")
	}
}

func TestNew_RejectsLoneStar(t *testing.T) {
	_, err := New(Config{Patterns: []string{"*"}})
	if err == nil {
		t.Error("lone '*' should error")
	}
}

func TestShouldDrop_NilReceiverIsNoop(t *testing.T) {
	var f *Filter
	if f.ShouldDrop("/anything") {
		t.Error("nil Filter should return false")
	}
}

func TestShouldDrop_ExactMatch(t *testing.T) {
	f, err := New(Config{Patterns: []string{"/api/v1/auth/me"}})
	if err != nil {
		t.Fatal(err)
	}
	if !f.ShouldDrop("/api/v1/auth/me") {
		t.Error("exact match should drop")
	}
	if f.ShouldDrop("/api/v1/auth/me/extra") {
		t.Error("non-matching path should not drop")
	}
	if f.ShouldDrop("/api/v1/auth/ME") {
		t.Error("case mismatch should not drop (paths are case-sensitive)")
	}
}

func TestShouldDrop_PrefixMatch(t *testing.T) {
	f, err := New(Config{Patterns: []string{"/api/v1/auth/*"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"/api/v1/auth/":           true,
		"/api/v1/auth/me":         true,
		"/api/v1/auth/sessions/x": true,
		"/api/v1":                 false,
		"/api/v1/auth":            false,
		"/api/v2/auth/me":         false,
		"/v1/messages":            false,
	}
	for path, want := range cases {
		got := f.ShouldDrop(path)
		if got != want {
			t.Errorf("ShouldDrop(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestShouldDrop_MultiplePatterns(t *testing.T) {
	f, err := New(Config{Patterns: []string{
		"/api/v1/auth/*",
		"/api/v1/subscriptions/*",
		"/healthz",
	}})
	if err != nil {
		t.Fatal(err)
	}
	dropped := []string{
		"/api/v1/auth/me",
		"/api/v1/subscriptions/active",
		"/healthz",
	}
	kept := []string{
		"/v1/messages",
		"/v1/chat/completions",
		"/api/v1/models",
	}
	for _, p := range dropped {
		if !f.ShouldDrop(p) {
			t.Errorf("expected drop on %q", p)
		}
	}
	for _, p := range kept {
		if f.ShouldDrop(p) {
			t.Errorf("expected keep on %q", p)
		}
	}
}

func TestPatterns_RoundTrip(t *testing.T) {
	original := []string{"/api/v1/auth/*", "/api/v1/subscriptions/*"}
	f, err := New(Config{Patterns: original})
	if err != nil {
		t.Fatal(err)
	}
	got := f.Patterns()
	if len(got) != len(original) {
		t.Fatalf("got %d patterns, want %d", len(got), len(original))
	}
	for i, want := range original {
		if got[i] != want {
			t.Errorf("Patterns()[%d] = %q, want %q", i, got[i], want)
		}
	}
}

func TestPatterns_NilReceiver(t *testing.T) {
	var f *Filter
	if got := f.Patterns(); got != nil {
		t.Errorf("nil receiver Patterns() = %v, want nil", got)
	}
}
