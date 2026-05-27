// Package session implements the session-prefix construction and
// canonical-hashing described in ARCHITECTURE § 5.1 and § 5.2.
//
// Three protocols, one algorithm — the parser is dispatch-free in the
// algorithm sense; only the *prefix construction* is per-protocol.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// VirtualSystemRole is the reserved role name used to fold non-messages
// session state (Anthropic `system`, OpenAI Responses `instructions`)
// into the prefix as a virtual turn 0.
//
// The leading double-underscore prefix + apilog namespace makes it
// safe against client-supplied real role values: Anthropic / OpenAI
// reject unknown roles at the API boundary, so a real client cannot
// produce a turn with this role on the wire.
const VirtualSystemRole = "__apilog_system__"

// HashLen is the byte length of canonical hashes used for the SQLite
// prefix_canonical_hash column and the IN-clause lookup.
const HashLen = 16

// HashHex computes the canonical hash of a session prefix.
// canonicalize(x) = json.Marshal with sorted keys, no whitespace.
// Returns lowercase hex truncated to HashLen chars.
func HashHex(prefix []json.RawMessage) string {
	canon := canonicalize(prefix)
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])[:HashLen]
}

// StrictPrefixHashes returns the hashes of all strict prefixes of `turns`,
// ordered from longest (len(turns)-1) down to shortest (1).
//
// Used by the IN-clause session lookup in ARCHITECTURE § 5.2: each hash
// is a "could this be my parent's full prefix?" question. The first
// match SQL returns (ORDER BY prefix_len DESC) is our parent.
//
// Returns empty slice when len(turns) <= 1 (no strict prefix exists).
func StrictPrefixHashes(turns []json.RawMessage) []string {
	if len(turns) <= 1 {
		return nil
	}
	out := make([]string, 0, len(turns)-1)
	// Walk from longest (len-1) down to shortest (1).
	for k := len(turns) - 1; k >= 1; k-- {
		out = append(out, HashHex(turns[:k]))
	}
	return out
}

// canonicalize emits a turns array as JSON with sorted keys, no whitespace.
// Go's encoding/json sorts object keys when marshaling a map, but our
// turns are json.RawMessage — opaque to the encoder. So we round-trip
// through interface{} to force re-marshaling with the encoder's
// canonical-key behavior.
func canonicalize(turns []json.RawMessage) []byte {
	if len(turns) == 0 {
		return []byte("[]")
	}
	// Decode each turn into a generic structure that the encoder will
	// re-marshal with sorted keys.
	decoded := make([]any, len(turns))
	for i, raw := range turns {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			// Fall back to using the raw bytes inline — same string for
			// the same input, which is what we need.
			decoded[i] = json.RawMessage(raw)
			continue
		}
		decoded[i] = v
	}
	b, err := json.Marshal(decoded)
	if err != nil {
		// Extremely unlikely; fall back to a deterministic representation.
		return []byte(quickJoin(turns))
	}
	return b
}

// quickJoin is the no-canonicalization fallback. Joins raw JSONs with
// commas in array brackets. Deterministic given the same input but does
// not normalize key order — only used when json.Marshal itself fails,
// which shouldn't happen for valid input.
func quickJoin(turns []json.RawMessage) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, t := range turns {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.Write(t)
	}
	sb.WriteByte(']')
	return sb.String()
}
