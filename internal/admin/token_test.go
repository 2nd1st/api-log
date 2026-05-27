package admin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureTokenGeneratesOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	tok, gen, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !gen {
		t.Errorf("expected generated=true on first run")
	}
	if len(tok) != 64 {
		t.Errorf("token len = %d, want 64", len(tok))
	}
	// File should exist with 0600 perms.
	st, err := os.Stat(filepath.Join(dir, "admin_token"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("file perms = %v, want 0600", st.Mode().Perm())
	}
}

func TestEnsureTokenReadsExisting(t *testing.T) {
	dir := t.TempDir()
	want := "preexisting-token-value"
	if err := os.WriteFile(filepath.Join(dir, "admin_token"), []byte(want+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, gen, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gen {
		t.Errorf("expected generated=false when token exists")
	}
	if tok != want {
		t.Errorf("tok = %q, want %q", tok, want)
	}
}

func TestEnsureTokenRegeneratesAfterDeletion(t *testing.T) {
	dir := t.TempDir()
	tok1, _, _ := EnsureToken(dir)
	// Delete file then re-call: must regenerate.
	_ = os.Remove(filepath.Join(dir, "admin_token"))
	tok2, gen, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !gen {
		t.Errorf("expected generated=true after deletion")
	}
	if tok1 == tok2 {
		t.Errorf("expected different tokens after regeneration; got same %s", tok2)
	}
}

func TestEnsureTokenRejectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin_token"), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureToken(dir); err == nil {
		t.Error("expected error for empty token file")
	}
}

func TestCompareConst(t *testing.T) {
	if !CompareConst("abc123", "abc123") {
		t.Errorf("equal strings should return true")
	}
	if CompareConst("abc123", "abc124") {
		t.Errorf("differing strings should return false")
	}
	if CompareConst("abc", "abcd") {
		t.Errorf("different lengths should return false")
	}
	if CompareConst("", "") {
		// Empty == empty is technically true but we never expect to
		// compare empty tokens; should be safe either way.
		t.Logf("note: CompareConst(\"\",\"\") = true (acceptable)")
	}
}
