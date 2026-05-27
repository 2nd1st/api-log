package ids

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// KeyHashLen is the canonical length of key_hash in SQLite (16 hex chars = 64 bits).
// File names use only the first 8 chars (key_hash[:8]) for brevity. See
// ARCHITECTURE § 2 for the full definition.
const KeyHashLen = 16

// AllZeroKeyHash is the key_hash for requests with no Authorization/x-api-key header.
// File names like "data/<date>/00000000.jsonl" come from this.
const AllZeroKeyHash = "0000000000000000"

// KeyHashFromHeaders computes the canonical key_hash for a request's auth
// headers. Preference order (per ARCHITECTURE § 2):
//   1. Authorization header value (including any "Bearer " prefix)
//   2. x-api-key header value
//   3. empty string → AllZeroKeyHash
//
// The hash is the lowercase hex of sha256 truncated to KeyHashLen chars.
func KeyHashFromHeaders(h http.Header) string {
	canon := canonicalAuth(h)
	if canon == "" {
		return AllZeroKeyHash
	}
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:])[:KeyHashLen]
}

// KeyHashShort returns the first 8 hex chars of a key_hash. Used for file naming.
func KeyHashShort(keyHash string) string {
	if len(keyHash) < 8 {
		return keyHash
	}
	return keyHash[:8]
}

func canonicalAuth(h http.Header) string {
	if v := h.Get("Authorization"); v != "" {
		return v
	}
	if v := h.Get("x-api-key"); v != "" {
		return v
	}
	return ""
}
