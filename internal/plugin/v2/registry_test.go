package v2

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// ----- Test doubles -----------------------------------------------
//
// Reuse a single mutable double across before/after for the multi-
// instance and ordering tests. The double is a value type so the
// Registry holds it through the BeforePlugin / AfterPlugin
// interfaces by reference (we wrap it in *fakePlugin to keep one
// shared call log).

type fakePlugin struct {
	name           string
	beforeAction   Action
	beforeMutator  func(req *ParsedRequest) *ParsedRequest
	beforeIntercept *InterceptResponse
	afterAction    Action
	afterMutator   func(req *ParsedRequest, ac *AfterContext) *ParsedResponse
	afterIntercept *InterceptResponse
	afterRegister  func(req *ParsedRequest, ac *AfterContext)
	panicOnBefore  bool
	panicOnAfter   bool

	mu        sync.Mutex
	beforeLog []string
	afterLog  []string
}

func (f *fakePlugin) Name() string { return f.name }

func (f *fakePlugin) OnBefore(ctx context.Context, req *ParsedRequest, cfg map[string]any) BeforeResult {
	f.mu.Lock()
	f.beforeLog = append(f.beforeLog, req.Model)
	f.mu.Unlock()
	if f.panicOnBefore {
		panic("intentional before panic")
	}
	res := BeforeResult{Action: f.beforeAction}
	if f.beforeAction == ActionMutate && f.beforeMutator != nil {
		res.Mutated = f.beforeMutator(req)
	}
	if f.beforeAction == ActionIntercept {
		res.Intercept = f.beforeIntercept
	}
	return res
}

func (f *fakePlugin) OnAfter(ctx context.Context, req *ParsedRequest, ac *AfterContext, cfg map[string]any) AfterResult {
	f.mu.Lock()
	f.afterLog = append(f.afterLog, req.Model)
	f.mu.Unlock()
	if f.panicOnAfter {
		panic("intentional after panic")
	}
	if f.afterRegister != nil {
		f.afterRegister(req, ac)
	}
	res := AfterResult{Action: f.afterAction}
	if f.afterAction == ActionMutate && f.afterMutator != nil {
		res.Mutated = f.afterMutator(req, ac)
	}
	if f.afterAction == ActionIntercept {
		res.Intercept = f.afterIntercept
	}
	return res
}

// withBuiltins registers ctors for the duration of a sub-test and
// resets afterwards so the global registry stays clean.
func withBuiltins(t *testing.T, ctors map[string]Ctor) {
	t.Helper()
	ResetBuiltinsForTest()
	for name, c := range ctors {
		RegisterBuiltin(name, c)
	}
	t.Cleanup(func() { ResetBuiltinsForTest() })
}

// ----- NewRegistry ------------------------------------------------

func TestNewRegistry_HappyPath(t *testing.T) {
	p1 := &fakePlugin{name: "p1", beforeAction: ActionContinue, afterAction: ActionContinue}
	p2 := &fakePlugin{name: "p2", beforeAction: ActionContinue, afterAction: ActionContinue}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
		"p2": func(_ map[string]any) (any, error) { return p2, nil },
	})

	r, errs := NewRegistry([]InstanceConfig{
		{Type: "p1", ID: "p1-inst", Enabled: true},
		{Type: "p2", ID: "p2-inst", Enabled: true},
	})
	if len(errs) > 0 {
		t.Fatalf("unexpected errs: %v", errs)
	}
	if len(r.Instances()) != 2 {
		t.Fatalf("instances = %d", len(r.Instances()))
	}
	if r.Instances()[0].ID != "p1-inst" {
		t.Errorf("ordering broken: %+v", r.Instances())
	}
}

func TestNewRegistry_UnknownType(t *testing.T) {
	ResetBuiltinsForTest()
	t.Cleanup(ResetBuiltinsForTest)
	r, errs := NewRegistry([]InstanceConfig{
		{Type: "nonesuch", ID: "x", Enabled: true},
	})
	if len(errs) == 0 {
		t.Errorf("expected an error for unknown type")
	}
	if len(r.Instances()) != 0 {
		t.Errorf("registry should be empty on unknown type")
	}
}

func TestNewRegistry_EmptyID(t *testing.T) {
	withBuiltins(t, map[string]Ctor{
		"x": func(_ map[string]any) (any, error) { return &fakePlugin{name: "x"}, nil },
	})
	_, errs := NewRegistry([]InstanceConfig{
		{Type: "x", ID: "", Enabled: true},
	})
	if len(errs) == 0 {
		t.Errorf("empty ID should error")
	}
}

func TestNewRegistry_CtorError(t *testing.T) {
	withBuiltins(t, map[string]Ctor{
		"bad": func(_ map[string]any) (any, error) { return nil, errors.New("boom") },
	})
	r, errs := NewRegistry([]InstanceConfig{
		{Type: "bad", ID: "bad-1", Enabled: true},
	})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "boom") {
		t.Errorf("err missing 'boom': %v", errs[0])
	}
	if len(r.Instances()) != 0 {
		t.Errorf("failed instance should not be registered")
	}
}

func TestNewRegistry_ZeroHooksError(t *testing.T) {
	withBuiltins(t, map[string]Ctor{
		"plain": func(_ map[string]any) (any, error) { return struct{ Name string }{Name: "plain"}, nil },
	})
	_, errs := NewRegistry([]InstanceConfig{
		{Type: "plain", ID: "plain-1", Enabled: true},
	})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "implements neither") {
		t.Errorf("expected 'implements neither' error, got %v", errs)
	}
}

// ----- IterateBefore ---------------------------------------------

func TestIterateBefore_RegistrationOrder(t *testing.T) {
	var order []string
	p1 := &fakePlugin{
		name:          "p1",
		beforeAction:  ActionContinue,
		beforeMutator: func(r *ParsedRequest) *ParsedRequest { order = append(order, "p1"); return nil },
	}
	p2 := &fakePlugin{
		name:          "p2",
		beforeAction:  ActionContinue,
		beforeMutator: func(r *ParsedRequest) *ParsedRequest { order = append(order, "p2"); return nil },
	}
	// Hook the order capture via the call log; mutator only runs on
	// ActionMutate, but the call log records every call.
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
		"p2": func(_ map[string]any) (any, error) { return p2, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{
		{Type: "p1", ID: "p1-1", Enabled: true},
		{Type: "p2", ID: "p2-1", Enabled: true},
	})
	req := &ParsedRequest{Protocol: ProtocolChat, Model: "m"}
	r.IterateBefore(context.Background(), req)
	if len(p1.beforeLog) != 1 || len(p2.beforeLog) != 1 {
		t.Errorf("both plugins should be called once: p1=%d p2=%d", len(p1.beforeLog), len(p2.beforeLog))
	}
}

func TestIterateBefore_MutateCascades(t *testing.T) {
	// p1 mutates Model to "X"; p2's call log should show "X".
	p1 := &fakePlugin{
		name:         "p1",
		beforeAction: ActionMutate,
		beforeMutator: func(r *ParsedRequest) *ParsedRequest {
			cp := *r
			cp.Model = "X"
			return &cp
		},
	}
	p2 := &fakePlugin{name: "p2", beforeAction: ActionContinue}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
		"p2": func(_ map[string]any) (any, error) { return p2, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{
		{Type: "p1", ID: "p1", Enabled: true},
		{Type: "p2", ID: "p2", Enabled: true},
	})
	req := &ParsedRequest{Model: "original"}
	out, intercept := r.IterateBefore(context.Background(), req)
	if intercept != nil {
		t.Fatalf("unexpected intercept")
	}
	if out.Model != "X" {
		t.Errorf("p1's mutation did not cascade: %q", out.Model)
	}
	if len(p2.beforeLog) != 1 || p2.beforeLog[0] != "X" {
		t.Errorf("p2 did not see mutated request: %v", p2.beforeLog)
	}
}

func TestIterateBefore_InterceptShortCircuits(t *testing.T) {
	p1 := &fakePlugin{
		name:           "p1",
		beforeAction:   ActionIntercept,
		beforeIntercept: &InterceptResponse{Status: 429, Body: []byte("rate-limited")},
	}
	p2 := &fakePlugin{name: "p2", beforeAction: ActionContinue}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
		"p2": func(_ map[string]any) (any, error) { return p2, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{
		{Type: "p1", ID: "p1", Enabled: true},
		{Type: "p2", ID: "p2", Enabled: true},
	})
	req := &ParsedRequest{Model: "m"}
	_, intercept := r.IterateBefore(context.Background(), req)
	if intercept == nil {
		t.Fatalf("expected intercept")
	}
	if intercept.Response.Status != 429 {
		t.Errorf("status = %d", intercept.Response.Status)
	}
	if intercept.Type != "p1" || intercept.ID != "p1" || intercept.Hook != "before" {
		t.Errorf("intercept marker = %+v, want p1/p1/before", intercept)
	}
	if len(p2.beforeLog) != 0 {
		t.Errorf("p2 should not have run after p1 intercepted")
	}
}

func TestIterateBefore_DisabledSkipped(t *testing.T) {
	p1 := &fakePlugin{name: "p1", beforeAction: ActionContinue}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{
		{Type: "p1", ID: "p1", Enabled: false},
	})
	r.IterateBefore(context.Background(), &ParsedRequest{})
	if len(p1.beforeLog) != 0 {
		t.Errorf("disabled instance should not run")
	}
}

func TestIterateBefore_PanicFailsOpen(t *testing.T) {
	p1 := &fakePlugin{name: "p1", panicOnBefore: true}
	p2 := &fakePlugin{name: "p2", beforeAction: ActionContinue}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
		"p2": func(_ map[string]any) (any, error) { return p2, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{
		{Type: "p1", ID: "p1", Enabled: true},
		{Type: "p2", ID: "p2", Enabled: true},
	})
	out, intercept := r.IterateBefore(context.Background(), &ParsedRequest{Model: "m"})
	if intercept != nil {
		t.Errorf("panic should not intercept")
	}
	if out == nil {
		t.Errorf("request should still flow")
	}
	// p2 must still have run (fail-open semantics).
	if len(p2.beforeLog) != 1 {
		t.Errorf("p2 should run despite p1 panic: %v", p2.beforeLog)
	}
	// Breadcrumb recorded.
	errs := r.DrainErrors()
	if len(errs) != 1 {
		t.Fatalf("expected 1 plugin_error, got %d", len(errs))
	}
	if errs[0].Type != "p1" || errs[0].Hook != "before" {
		t.Errorf("breadcrumb = %+v", errs[0])
	}
	if !strings.Contains(errs[0].Msg, "intentional before panic") {
		t.Errorf("breadcrumb msg = %q", errs[0].Msg)
	}
}

func TestIterateBefore_InterceptWithNilBodyTreatedAsContinue(t *testing.T) {
	p1 := &fakePlugin{name: "p1", beforeAction: ActionIntercept, beforeIntercept: nil}
	p2 := &fakePlugin{name: "p2", beforeAction: ActionContinue}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
		"p2": func(_ map[string]any) (any, error) { return p2, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{
		{Type: "p1", ID: "p1", Enabled: true},
		{Type: "p2", ID: "p2", Enabled: true},
	})
	_, intercept := r.IterateBefore(context.Background(), &ParsedRequest{})
	if intercept != nil {
		t.Errorf("nil intercept body should be defensively skipped")
	}
	if len(p2.beforeLog) != 1 {
		t.Errorf("p2 should run since p1 was defensively treated as Continue")
	}
	if errs := r.DrainErrors(); len(errs) != 1 {
		t.Errorf("expected breadcrumb for defensive intercept")
	}
}

// ----- IterateAfter ----------------------------------------------

func TestIterateAfter_RegisterCallback(t *testing.T) {
	p1 := &fakePlugin{
		name:        "p1",
		afterAction: ActionContinue,
		afterRegister: func(req *ParsedRequest, ac *AfterContext) {
			ac.OnContentDelta(func(s string) string { return s + "!" })
		},
	}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{{Type: "p1", ID: "p1", Enabled: true}})
	d, intercept := r.WrapStreamDispatcher(context.Background(), &ParsedRequest{Protocol: ProtocolMessages})
	if intercept != nil {
		t.Fatalf("unexpected intercept")
	}
	if d == nil {
		t.Fatal("dispatcher nil")
	}
	if got := d.After.ContentDeltaTransforms(); len(got) != 1 {
		t.Errorf("expected one registered transform; got %d", len(got))
	}
}

func TestIterateAfter_InterceptDuringRegistration(t *testing.T) {
	p1 := &fakePlugin{
		name:           "p1",
		afterAction:    ActionIntercept,
		afterIntercept: &InterceptResponse{Status: 502, Body: []byte("nope")},
	}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{{Type: "p1", ID: "p1", Enabled: true}})
	d, intercept := r.WrapStreamDispatcher(context.Background(), &ParsedRequest{Protocol: ProtocolMessages})
	if d != nil {
		t.Errorf("dispatcher should be nil on intercept")
	}
	if intercept == nil || intercept.Response == nil || intercept.Response.Status != 502 {
		t.Errorf("intercept = %+v", intercept)
	}
	if intercept != nil && (intercept.Type != "p1" || intercept.Hook != "after") {
		t.Errorf("intercept marker = %+v, want p1/after", intercept)
	}
}

func TestIterateAfter_PanicFailsOpen(t *testing.T) {
	p1 := &fakePlugin{name: "p1", panicOnAfter: true}
	p2 := &fakePlugin{
		name:        "p2",
		afterAction: ActionContinue,
		afterRegister: func(req *ParsedRequest, ac *AfterContext) {
			ac.OnContentDelta(func(s string) string { return s + "!" })
		},
	}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
		"p2": func(_ map[string]any) (any, error) { return p2, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{
		{Type: "p1", ID: "p1", Enabled: true},
		{Type: "p2", ID: "p2", Enabled: true},
	})
	d, intercept := r.WrapStreamDispatcher(context.Background(), &ParsedRequest{Protocol: ProtocolMessages})
	if intercept != nil {
		t.Errorf("after-panic should not intercept")
	}
	if d == nil {
		t.Fatal("dispatcher nil after panic — should be fail-open")
	}
	// p2's callback must be registered even though p1 panicked.
	if got := d.After.ContentDeltaTransforms(); len(got) != 1 {
		t.Errorf("p2 should still register: got %d", len(got))
	}
	if errs := r.DrainErrors(); len(errs) != 1 {
		t.Errorf("expected 1 panic breadcrumb, got %d", len(errs))
	}
}

func TestIterateAfter_NonStreamingMutate(t *testing.T) {
	p1 := &fakePlugin{
		name:        "p1",
		afterAction: ActionMutate,
		afterMutator: func(req *ParsedRequest, ac *AfterContext) *ParsedResponse {
			cp := *ac.Response
			cp.Content = "MUTATED"
			return &cp
		},
	}
	withBuiltins(t, map[string]Ctor{
		"p1": func(_ map[string]any) (any, error) { return p1, nil },
	})
	r, _ := NewRegistry([]InstanceConfig{{Type: "p1", ID: "p1", Enabled: true}})
	ac := &AfterContext{Response: &ParsedResponse{Content: "original"}}
	intercept := r.IterateAfter(context.Background(), &ParsedRequest{}, ac)
	if intercept != nil {
		t.Fatalf("unexpected intercept")
	}
	if ac.Response.Content != "MUTATED" {
		t.Errorf("mutation not applied: %q", ac.Response.Content)
	}
}

// ----- Error breadcrumb cap --------------------------------------

func TestRegistry_ErrorBreadcrumbCap(t *testing.T) {
	r := &Registry{}
	for i := 0; i < maxPluginErrors*2; i++ {
		r.recordError("x", "x", "before", "msg")
	}
	if got := r.Errors(); len(got) != maxPluginErrors {
		t.Errorf("breadcrumb cap = %d, expected %d", len(got), maxPluginErrors)
	}
}

func TestRegistry_DrainResetsErrors(t *testing.T) {
	r := &Registry{}
	r.recordError("x", "x", "before", "a")
	if len(r.Errors()) != 1 {
		t.Fatal("expected 1 error")
	}
	r.DrainErrors()
	if len(r.Errors()) != 0 {
		t.Errorf("drain should reset")
	}
}

// ----- Builtin registry guards -----------------------------------

func TestRegisterBuiltin_DuplicatePanics(t *testing.T) {
	ResetBuiltinsForTest()
	t.Cleanup(ResetBuiltinsForTest)
	RegisterBuiltin("dup", func(_ map[string]any) (any, error) { return &fakePlugin{name: "dup"}, nil })
	defer func() {
		if rec := recover(); rec == nil {
			t.Errorf("duplicate RegisterBuiltin should panic")
		}
	}()
	RegisterBuiltin("dup", func(_ map[string]any) (any, error) { return &fakePlugin{name: "dup"}, nil })
}

func TestRegisterBuiltin_EmptyNamePanics(t *testing.T) {
	defer func() {
		if rec := recover(); rec == nil {
			t.Errorf("empty name should panic")
		}
	}()
	RegisterBuiltin("", func(_ map[string]any) (any, error) { return nil, nil })
}

func TestLookupBuiltin_Missing(t *testing.T) {
	ResetBuiltinsForTest()
	t.Cleanup(ResetBuiltinsForTest)
	if _, ok := LookupBuiltin("nothing"); ok {
		t.Errorf("missing type should return false")
	}
}

// ----- Load: YAML × runtime-overrides merge ----------------------
//
// The three precedence cases from spec §3.3.2. The nil vs. non-nil-
// empty distinction is the bug-prone one and gets the most-explicit
// assertion.

func TestLoad_NilOverrideFallsThroughToYAML(t *testing.T) {
	yaml := []InstanceConfig{
		{Type: "watermark", ID: "wm-1", Enabled: true},
	}
	got := Load(yaml, nil)
	if len(got) != 1 || got[0].ID != "wm-1" {
		t.Errorf("nil override should yield YAML list verbatim: %+v", got)
	}
}

func TestLoad_NilInstancesFieldFallsThroughToYAML(t *testing.T) {
	// non-nil OverrideList with Instances field == nil ≡ nil override.
	yaml := []InstanceConfig{{Type: "watermark", ID: "wm-1", Enabled: true}}
	got := Load(yaml, &OverrideList{Instances: nil})
	if len(got) != 1 || got[0].ID != "wm-1" {
		t.Errorf("nil Instances field should fall through: %+v", got)
	}
}

func TestLoad_EmptyOverrideMeansAllPluginsOff(t *testing.T) {
	// non-nil OverrideList with Instances == []  ≠  nil override.
	yaml := []InstanceConfig{{Type: "watermark", ID: "wm-1", Enabled: true}}
	got := Load(yaml, &OverrideList{Instances: []InstanceConfig{}})
	if len(got) != 0 {
		t.Errorf("explicit empty override should produce empty list; got %+v", got)
	}
}

func TestLoad_NonEmptyOverrideFullyReplacesYAML(t *testing.T) {
	yaml := []InstanceConfig{
		{Type: "watermark", ID: "wm-yaml", Enabled: true},
		{Type: "prompt_inject", ID: "pi-yaml", Enabled: true},
	}
	overrides := &OverrideList{Instances: []InstanceConfig{
		{Type: "watermark", ID: "wm-override", Enabled: false},
	}}
	got := Load(yaml, overrides)
	if len(got) != 1 {
		t.Fatalf("override should fully replace YAML; got len=%d", len(got))
	}
	if got[0].ID != "wm-override" || got[0].Enabled {
		t.Errorf("override entry not faithful: %+v", got[0])
	}
}

func TestLoad_ReturnsDefensiveCopy(t *testing.T) {
	yaml := []InstanceConfig{{Type: "x", ID: "x-1", Enabled: true}}
	got := Load(yaml, nil)
	// Mutate the returned slice; YAML must be untouched.
	got[0].ID = "tampered"
	if yaml[0].ID != "x-1" {
		t.Errorf("Load did not return a defensive copy; YAML mutated to %q", yaml[0].ID)
	}
}
