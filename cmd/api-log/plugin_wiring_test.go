package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	pluginv2 "github.com/leoyun/api-log/internal/plugin/v2"
)

// TestRunAfterChainOnIntercept_AfterPluginDecoratesBeforeIntercept is
// the BEFORE-intercept + AFTER-chain contract from spec §2.2 / §2.4:
// when a BEFORE plugin produces an intercept response, the AFTER chain
// MUST still run on the synthesized response so decorators
// (text-append, watermark, etc.) get to mutate it before the client
// sees it.
func TestRunAfterChainOnIntercept_AfterPluginDecoratesBeforeIntercept(t *testing.T) {
	// Build a registry with one AFTER plugin (text-append) that
	// appends " [decorated]" to assistant content. The BEFORE-side
	// intercept is built by hand below.
	ctor, ok := pluginv2.LookupBuiltin("text-append")
	if !ok {
		t.Fatal("text-append not registered (init order broken?)")
	}
	built, err := ctor(map[string]any{
		"down": map[string]any{"suffix": " [decorated]", "target": "content"},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if _, ok := built.(pluginv2.AfterPlugin); !ok {
		t.Fatal("text-append should implement AfterPlugin")
	}
	reg, errs := pluginv2.NewRegistry([]pluginv2.InstanceConfig{
		{Type: "text-append", ID: "footer", Enabled: true, Config: map[string]any{
			"down": map[string]any{"suffix": " [decorated]", "target": "content"},
		}},
	})
	if len(errs) > 0 {
		t.Fatalf("registry errs: %v", errs)
	}

	// Synthesized intercept response in Anthropic Messages shape so
	// ParsedResponseFromBody decodes Content cleanly.
	interceptBody := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"blocked"}],"model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	src := &pluginv2.InterceptInfo{
		Type: "before-blocker",
		ID:   "test",
		Hook: "before",
		Response: &pluginv2.InterceptResponse{
			Status:  403,
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Body:    interceptBody,
		},
	}
	req := &pluginv2.ParsedRequest{
		Protocol: pluginv2.ProtocolMessages,
		Path:     "/v1/messages",
	}

	final := runAfterChainOnIntercept(context.Background(), reg, req, src)
	if final == nil || final.Response == nil {
		t.Fatal("final intercept nil")
	}
	if final.Response.Status != 403 {
		t.Errorf("status drift: %d, want 403", final.Response.Status)
	}
	if final.Type != "before-blocker" || final.Hook != "before" {
		t.Errorf("intercept marker should preserve originator; got type=%q hook=%q", final.Type, final.Hook)
	}

	// The AFTER chain should have appended the suffix into the
	// re-serialized content[0].text.
	var shape struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(final.Response.Body, &shape); err != nil {
		t.Fatalf("re-serialized body invalid JSON: %v\nbody=%s", err, final.Response.Body)
	}
	if len(shape.Content) == 0 {
		t.Fatalf("content array empty: %s", final.Response.Body)
	}
	if !strings.Contains(shape.Content[0].Text, "blocked [decorated]") {
		t.Errorf("AFTER plugin did not decorate intercept content; got %q", shape.Content[0].Text)
	}
}

// TestRunAfterChainOnIntercept_NoAfterPluginsPassesThrough confirms
// the helper is a no-op when nothing is registered on the AFTER side.
func TestRunAfterChainOnIntercept_NoAfterPluginsPassesThrough(t *testing.T) {
	reg, errs := pluginv2.NewRegistry(nil)
	if len(errs) > 0 {
		t.Fatalf("registry errs: %v", errs)
	}
	src := &pluginv2.InterceptInfo{
		Response: &pluginv2.InterceptResponse{
			Status: 403,
			Body:   []byte(`{"error":"blocked"}`),
		},
	}
	req := &pluginv2.ParsedRequest{Protocol: pluginv2.ProtocolMessages, Path: "/v1/messages"}
	final := runAfterChainOnIntercept(context.Background(), reg, req, src)
	if final != src {
		t.Errorf("with no AFTER plugins, helper should return source intercept verbatim")
	}
}
