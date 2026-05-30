package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/leoyun/api-log/internal/trace"
)

// stubBefore is a test Plugin that implements ObserveBeforeRecord.
// Each call appends the plugin's name to *order so tests can assert
// iteration sequencing + short-circuit behavior.
type stubBefore struct {
	name         string
	shouldRecord bool
	err          error
	order        *[]string
}

func (s *stubBefore) Name() string                  { return s.name }
func (s *stubBefore) Init(_ map[string]any) error   { return nil }
func (s *stubBefore) Close() error                  { return nil }
func (s *stubBefore) BeforeRecord(_ context.Context, _ trace.Trace) (bool, error) {
	*s.order = append(*s.order, s.name)
	return s.shouldRecord, s.err
}

// stubAfter implements ObserveAfterRecord only (no BeforeRecord).
// Used to assert that the BeforeRecord iterator skips plugins that
// haven't opted into the hook.
type stubAfter struct {
	name  string
	order *[]string
}

func (s *stubAfter) Name() string                                  { return s.name }
func (s *stubAfter) Init(_ map[string]any) error                   { return nil }
func (s *stubAfter) Close() error                                  { return nil }
func (s *stubAfter) AfterRecord(_ context.Context, _ trace.Trace)  { *s.order = append(*s.order, s.name) }

// stubInitErr fails Init, used to assert Registry.Init surfaces errors.
type stubInitErr struct {
	name    string
	initErr error
}

func (s *stubInitErr) Name() string                  { return s.name }
func (s *stubInitErr) Init(_ map[string]any) error   { return s.initErr }
func (s *stubInitErr) Close() error                  { return nil }

func TestRegistry_Register_DuplicateName(t *testing.T) {
	r := NewRegistry()
	a := &stubBefore{name: "alpha", shouldRecord: true}
	b := &stubBefore{name: "alpha", shouldRecord: true}
	if err := r.Register(a); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(b); err == nil {
		t.Fatal("expected duplicate-name error, got nil")
	}
}

func TestRegistry_Register_NilOrEmptyName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("expected error for nil plugin, got nil")
	}
	if err := r.Register(&stubBefore{name: ""}); err == nil {
		t.Fatal("expected error for empty Name(), got nil")
	}
}

func TestRegistry_Init_PropagatesError(t *testing.T) {
	r := NewRegistry()
	want := errors.New("init boom")
	if err := r.Register(&stubInitErr{name: "boom", initErr: want}); err != nil {
		t.Fatalf("register: %v", err)
	}
	err := r.Init(nil)
	if err == nil {
		t.Fatal("expected init error, got nil")
	}
	if !errors.Is(err, want) {
		t.Fatalf("init error chain missing %v, got %v", want, err)
	}
}

func TestRegistry_Init_PerPluginConfig(t *testing.T) {
	r := NewRegistry()
	captured := map[string]map[string]any{}
	cap := &cfgCapturePlugin{name: "cap", got: captured}
	if err := r.Register(cap); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfgs := map[string]map[string]any{
		"cap":   {"k": "v"},
		"other": {"unused": true},
	}
	if err := r.Init(cfgs); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := captured["cap"]; got == nil || got["k"] != "v" {
		t.Fatalf("plugin did not receive its config subtree, got %+v", got)
	}
	if _, ok := captured["other"]; ok {
		t.Fatalf("plugin received another plugin's config, captured=%+v", captured)
	}
}

type cfgCapturePlugin struct {
	name string
	got  map[string]map[string]any
}

func (c *cfgCapturePlugin) Name() string { return c.name }
func (c *cfgCapturePlugin) Init(cfg map[string]any) error {
	c.got[c.name] = cfg
	return nil
}
func (c *cfgCapturePlugin) Close() error { return nil }

func TestRegistry_IterateBeforeRecord_OrderAndShortCircuit(t *testing.T) {
	type call struct {
		name         string
		shouldRecord bool
		err          error
	}
	tests := []struct {
		name          string
		plugins       []call
		wantRecord    bool
		wantOrder     []string
		wantNumErrors int
	}{
		{
			name: "all_continue_runs_in_order",
			plugins: []call{
				{name: "a", shouldRecord: true},
				{name: "b", shouldRecord: true},
				{name: "c", shouldRecord: true},
			},
			wantRecord: true,
			wantOrder:  []string{"a", "b", "c"},
		},
		{
			name: "second_drops_short_circuits",
			plugins: []call{
				{name: "a", shouldRecord: true},
				{name: "b", shouldRecord: false},
				{name: "c", shouldRecord: true},
			},
			wantRecord: false,
			wantOrder:  []string{"a", "b"}, // c never invoked
		},
		{
			name: "first_drops_short_circuits",
			plugins: []call{
				{name: "a", shouldRecord: false},
				{name: "b", shouldRecord: true},
			},
			wantRecord: false,
			wantOrder:  []string{"a"},
		},
		{
			name: "error_with_continue_records_error_but_keeps_going",
			plugins: []call{
				{name: "a", shouldRecord: true, err: errors.New("non-fatal")},
				{name: "b", shouldRecord: true},
			},
			wantRecord:    true,
			wantOrder:     []string{"a", "b"},
			wantNumErrors: 1,
		},
		{
			name: "error_with_drop_records_error_and_stops",
			plugins: []call{
				{name: "a", shouldRecord: false, err: errors.New("fatal-ish")},
				{name: "b", shouldRecord: true},
			},
			wantRecord:    false,
			wantOrder:     []string{"a"},
			wantNumErrors: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			var order []string
			for _, p := range tc.plugins {
				if err := r.Register(&stubBefore{
					name:         p.name,
					shouldRecord: p.shouldRecord,
					err:          p.err,
					order:        &order,
				}); err != nil {
					t.Fatalf("register %s: %v", p.name, err)
				}
			}
			gotRecord, errs := r.IterateBeforeRecord(context.Background(), trace.Trace{})
			if gotRecord != tc.wantRecord {
				t.Errorf("shouldRecord = %v, want %v", gotRecord, tc.wantRecord)
			}
			if !stringSliceEqual(order, tc.wantOrder) {
				t.Errorf("invocation order = %v, want %v", order, tc.wantOrder)
			}
			if len(errs) != tc.wantNumErrors {
				t.Errorf("len(errs) = %d, want %d (errs=%v)", len(errs), tc.wantNumErrors, errs)
			}
		})
	}
}

func TestRegistry_IterateBeforeRecord_SkipsNonOptedInPlugins(t *testing.T) {
	r := NewRegistry()
	var order []string
	if err := r.Register(&stubAfter{name: "after-only", order: &order}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register(&stubBefore{name: "before", shouldRecord: true, order: &order}); err != nil {
		t.Fatalf("register: %v", err)
	}
	shouldRecord, errs := r.IterateBeforeRecord(context.Background(), trace.Trace{})
	if !shouldRecord {
		t.Errorf("shouldRecord = false, want true")
	}
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none", errs)
	}
	if !stringSliceEqual(order, []string{"before"}) {
		t.Errorf("only the BeforeRecord plugin should have fired, got order=%v", order)
	}
}

func TestRegistry_IterateAfterRecord_OrderAndSkip(t *testing.T) {
	r := NewRegistry()
	var order []string
	// Register a mix: one before-only, one after-only, one after-only.
	if err := r.Register(&stubBefore{name: "before-only", shouldRecord: true, order: &order}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register(&stubAfter{name: "after-1", order: &order}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register(&stubAfter{name: "after-2", order: &order}); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.IterateAfterRecord(context.Background(), trace.Trace{})
	if !stringSliceEqual(order, []string{"after-1", "after-2"}) {
		t.Errorf("AfterRecord order = %v, want [after-1 after-2]", order)
	}
}

func TestRegistry_Close_ContinuesPastError(t *testing.T) {
	r := NewRegistry()
	want := errors.New("close boom")
	if err := r.Register(&closeErrPlugin{name: "a", err: want}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Register(&closeErrPlugin{name: "b"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	gotErr := r.Close()
	if gotErr == nil || !errors.Is(gotErr, want) {
		t.Fatalf("Close() returned %v, want chain containing %v", gotErr, want)
	}
}

type closeErrPlugin struct {
	name string
	err  error
}

func (c *closeErrPlugin) Name() string                  { return c.name }
func (c *closeErrPlugin) Init(_ map[string]any) error   { return nil }
func (c *closeErrPlugin) Close() error                  { return c.err }

func TestRegistry_Plugins_ReturnsCopy(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&stubBefore{name: "a", shouldRecord: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got := r.Plugins()
	if len(got) != 1 {
		t.Fatalf("len(Plugins()) = %d, want 1", len(got))
	}
	got[0] = nil // mutate the copy
	again := r.Plugins()
	if again[0] == nil {
		t.Fatal("Plugins() returned shared slice; expected a copy")
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
