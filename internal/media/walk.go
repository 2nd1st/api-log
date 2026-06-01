package media

import (
	"encoding/json"
	"strconv"
)

// candidate is one media-bearing value the walker has identified inside a
// trace body. The decoder turns it into either a written file (and a
// MediaFile record) or a silent skip (URL-only, empty, bad base64).
type candidate struct {
	// side is "req" or "resp"; passed through to MediaFile.Side.
	side string

	// path is the dotted/bracketed JSON path inside the side body, exactly
	// as it appears in the protocol field tables (e.g.
	// "messages[0].content[0].image_url.url"). Used for MediaFile.SourceField
	// — never parsed by downstream code, just human-readable provenance.
	path string

	// payload is the raw string the protocol carried: either a data: URL,
	// an http(s):// URL, or a bare base64 blob. The decoder decides what
	// to do with it (bare base64 vs. data URL vs. URL-only skip).
	payload string

	// declaredMime is whatever sibling mime field the protocol provided
	// (media_type, mimeType, mime_type, ...). Empty when the protocol
	// doesn't carry one (e.g. plain image_url.url with no metadata) — in
	// that case the decoder falls back to data-URL sniff or octet-stream.
	declaredMime string
}

// findCandidates parses the body as JSON (best-effort), then walks it with
// the protocol shape detectors and returns every media-bearing field it
// finds, in document order. A nil / empty / non-JSON body yields zero
// candidates with no error; body_b64 fallback is intentionally invisible to
// extraction.
func findCandidates(rawBody json.RawMessage, side string) []candidate {
	if len(rawBody) == 0 {
		return nil
	}
	var root any
	if err := json.Unmarshal(rawBody, &root); err != nil {
		// Body wasn't valid JSON (rare; capture would normally have placed
		// it in body_b64 instead). Walking a non-JSON body is meaningless;
		// silently return zero — this is the "noise; skip silently" branch
		// of Phase K § 2.
		return nil
	}
	w := &walker{side: side}
	w.walk(root, "")
	return w.found
}

// walker is the recursive-descent context. Path is built up by string
// concatenation as we descend (object key → ".key", array index → "[i]").
// At each step we offer the current (path, value) to every shape detector;
// detectors that match emit candidates into w.found and may also tell the
// walker to stop descending into a subtree (so we don't double-count a
// gemini inlineData object as both an inlineData blob AND a bare base64
// field inside it).
type walker struct {
	side  string
	found []candidate
}

func (w *walker) walk(v any, path string) {
	switch node := v.(type) {
	case map[string]any:
		// Offer the object to every shape detector first. If a detector
		// claims this node, it may have already extracted media from
		// inside it and we should not double-walk its children.
		if w.tryObjectShapes(node, path) {
			return
		}
		// Otherwise, descend into every key in stable order. Stable order
		// is not guaranteed by Go's map iteration, but JSON object key
		// order is not semantically meaningful in any of the four supported
		// protocols — idx ordering is driven by array indices, which ARE
		// preserved.
		for k, child := range node {
			childPath := path + "." + k
			if path == "" {
				childPath = k
			}
			w.walk(child, childPath)
		}
	case []any:
		for i, child := range node {
			w.walk(child, path+"["+strconv.Itoa(i)+"]")
		}
	default:
		// Scalars are not candidates on their own — every media-bearing
		// field is reached via its parent object's shape detector.
	}
}

// tryObjectShapes runs the per-protocol shape matchers against this object.
// Returns true if the object was claimed (extracted media + skip-descend),
// false if the walker should keep descending into the children normally.
//
// Each protocol's detector returns (claimed, candidate, hasCandidate). A
// detector that recognizes the shape but produces no candidate (e.g. an
// http(s) URL we deliberately don't fetch) still claims the object so we
// don't accidentally treat its sub-values as new candidates.
//
// Note: detectors are checked in order, but the shapes are disjoint by
// construction (each protocol uses distinct field names: image_url vs.
// inlineData vs. source.type=image), so ordering only matters for
// performance, not correctness.
func (w *walker) tryObjectShapes(obj map[string]any, path string) bool {
	for _, det := range objectDetectors {
		claimed, cands := det(obj, path)
		if claimed {
			for _, c := range cands {
				c.side = w.side
				w.found = append(w.found, c)
			}
			return true
		}
	}
	return false
}
