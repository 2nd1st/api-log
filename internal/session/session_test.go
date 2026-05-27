package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildChatCompletions(t *testing.T) {
	body := json.RawMessage(`{
		"model": "gpt-test",
		"messages": [
			{"role":"system","content":"be helpful"},
			{"role":"user","content":"hi"}
		]
	}`)
	turns, ok := Build("/v1/chat/completions", body)
	if !ok {
		t.Fatal("Build returned ok=false")
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns))
	}
}

func TestBuildAnthropicWithSystem(t *testing.T) {
	body := json.RawMessage(`{
		"model": "claude",
		"system": "you are a pirate",
		"messages": [{"role":"user","content":"hi"}]
	}`)
	turns, ok := Build("/v1/messages", body)
	if !ok {
		t.Fatal("Build returned ok=false")
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (system + user)", len(turns))
	}
	// First turn must be the virtual system turn.
	var firstTurn map[string]any
	if err := json.Unmarshal(turns[0], &firstTurn); err != nil {
		t.Fatal(err)
	}
	if firstTurn["role"] != VirtualSystemRole {
		t.Errorf("first turn role = %v, want %s", firstTurn["role"], VirtualSystemRole)
	}
}

func TestBuildAnthropicDifferentSystemsProduceDistinctHashes(t *testing.T) {
	// THE point of the virtual system turn — same messages, different
	// systems must hash differently so they're identified as separate sessions.
	turnsA, _ := Build("/v1/messages", json.RawMessage(`{
		"system": "pirate",
		"messages": [{"role":"user","content":"hi"}]
	}`))
	turnsB, _ := Build("/v1/messages", json.RawMessage(`{
		"system": "doctor",
		"messages": [{"role":"user","content":"hi"}]
	}`))
	hA := HashHex(turnsA)
	hB := HashHex(turnsB)
	if hA == hB {
		t.Errorf("system A and B produce same hash: %s", hA)
	}
}

func TestBuildAnthropicNoSystem(t *testing.T) {
	body := json.RawMessage(`{"messages": [{"role":"user","content":"hi"}]}`)
	turns, ok := Build("/v1/messages", body)
	if !ok {
		t.Fatal("ok=false")
	}
	if len(turns) != 1 {
		t.Errorf("turns = %d, want 1 (no system)", len(turns))
	}
}

func TestBuildResponsesStringInput(t *testing.T) {
	body := json.RawMessage(`{
		"model": "gpt-test",
		"instructions": "be helpful",
		"input": "what is rust"
	}`)
	turns, ok := Build("/v1/responses", body)
	if !ok {
		t.Fatal("ok=false")
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (system + user)", len(turns))
	}
}

func TestBuildResponsesMessageArrayInput(t *testing.T) {
	body := json.RawMessage(`{
		"input": [
			{"type":"message","role":"user","content":"first"},
			{"type":"message","role":"assistant","content":"reply"},
			{"type":"message","role":"user","content":"follow-up"}
		]
	}`)
	turns, ok := Build("/v1/responses", body)
	if !ok {
		t.Fatal("ok=false")
	}
	if len(turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(turns))
	}
}

func TestBuildResponsesOpaqueFallback(t *testing.T) {
	// Heterogeneous array → falls back to opaque single-turn hash.
	body := json.RawMessage(`{
		"input": [
			{"type":"function_call","name":"do_stuff"},
			{"type":"message","role":"user","content":"hi"}
		]
	}`)
	turns, ok := Build("/v1/responses", body)
	if !ok {
		t.Fatal("ok=false")
	}
	if len(turns) != 1 {
		t.Errorf("turns = %d, want 1 (opaque fallback)", len(turns))
	}
}

func TestBuildUnknownPathReturnsNoSession(t *testing.T) {
	_, ok := Build("/v1/embeddings", json.RawMessage(`{"input":["text"]}`))
	if ok {
		t.Errorf("unknown path should return ok=false")
	}
}

func TestStrictPrefixHashesDescendingOrder(t *testing.T) {
	turns := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"q1"}`),
		json.RawMessage(`{"role":"assistant","content":"a1"}`),
		json.RawMessage(`{"role":"user","content":"q2"}`),
	}
	hs := StrictPrefixHashes(turns)
	if len(hs) != 2 {
		t.Fatalf("strict hashes len = %d, want 2", len(hs))
	}
	// Verify it's the hash of the first 2 turns, then the first 1.
	wantLong := HashHex(turns[:2])
	wantShort := HashHex(turns[:1])
	if hs[0] != wantLong {
		t.Errorf("hs[0] = %s, want %s", hs[0], wantLong)
	}
	if hs[1] != wantShort {
		t.Errorf("hs[1] = %s, want %s", hs[1], wantShort)
	}
}

func TestStrictPrefixHashesEmpty(t *testing.T) {
	if hs := StrictPrefixHashes(nil); hs != nil {
		t.Errorf("nil turns should give nil strict hashes, got %v", hs)
	}
	one := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}
	if hs := StrictPrefixHashes(one); len(hs) != 0 {
		t.Errorf("1-turn should give 0 strict prefix hashes, got %d", len(hs))
	}
}

func TestHashHexDeterministic(t *testing.T) {
	t1 := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}
	t2 := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}
	if HashHex(t1) != HashHex(t2) {
		t.Errorf("same input hashes differently")
	}
}

func TestHashHexCanonicalizesKeyOrder(t *testing.T) {
	// Same data, different key order in JSON. Canonicalization should
	// make their hashes match.
	a := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}
	b := []json.RawMessage{json.RawMessage(`{"content":"hi","role":"user"}`)}
	if HashHex(a) != HashHex(b) {
		t.Errorf("canonicalization failed: differs on key order")
	}
}

func TestHashHexLength(t *testing.T) {
	turns := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}
	h := HashHex(turns)
	if len(h) != HashLen {
		t.Errorf("HashHex length = %d, want %d", len(h), HashLen)
	}
	// Should be hex.
	if strings.ToLower(h) != h {
		t.Errorf("HashHex not lowercase hex: %s", h)
	}
}
