// Package admin manages the read API's bearer-token file.
//
// On first run we create data/admin_token with a fresh random token
// (256 bits, hex-encoded). On subsequent runs we read it. ARCHITECTURE
// § 6 says: "auto-generated on first run; print to stdout once at
// startup; if deleted, regenerate on next startup and print again".
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// EnsureToken returns the admin bearer token at data/admin_token,
// creating one if absent. Returns (token, generated, error) — caller
// uses `generated` to decide whether to print the token to stdout.
func EnsureToken(dataDir string) (token string, generated bool, err error) {
	path := filepath.Join(dataDir, "admin_token")

	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Existing token file: trim whitespace, validate non-empty.
		t := strings.TrimSpace(string(b))
		if t == "" {
			return "", false, fmt.Errorf("admin_token file at %s is empty", path)
		}
		return t, false, nil
	case errors.Is(err, fs.ErrNotExist):
		// Generate, write, return.
		t, err := generate()
		if err != nil {
			return "", false, err
		}
		if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
			return "", false, fmt.Errorf("write admin_token: %w", err)
		}
		return t, true, nil
	default:
		return "", false, fmt.Errorf("read admin_token at %s: %w", path, err)
	}
}

// generate returns a fresh 64-char hex (256 bits) random token.
func generate() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("admin_token: rand: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// CompareConst constant-time compares two tokens. Use this in the
// authentication middleware to avoid timing leaks per
// SECURITY.md threat model.
func CompareConst(want, got string) bool {
	// subtle.ConstantTimeCompare returns 1 if equal, else 0. It assumes
	// equal-length inputs to avoid leaking length. Pad to equal length
	// (xor with 0 is a no-op on the equality check) by using the lesser
	// of the two and forcing inequality if lengths differ.
	if len(want) != len(got) {
		// Still walk through subtle.Compare on one slice's-worth of bytes
		// so the rejection time is data-independent.
		_ = subtle.ConstantTimeCompare([]byte(want), []byte(want))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
