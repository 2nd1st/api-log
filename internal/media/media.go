// Package media implements the deterministic media-file extractor described
// in Phase K (uiux-research/phase-k-media-contract.md) and PHILOSOPHY § 1's
// carve-out 1.
//
// Scope (PHILOSOPHY § 1 + § 6):
//
//   - Per-trace, per-NAMED-FIELD transform of base64 / data-URL payloads
//     into derived files on disk. No synthesis, no inference of media that
//     isn't already explicitly carried by a documented protocol field.
//   - The JSONL line on disk remains the source of truth; extracted files
//     are a redundant cache that exists for fast viewer reads and zip-export
//     bundling.
//
// Non-interference (PHILOSOPHY § 2):
//
//   - Extract() runs after writer.appendOne has flushed the JSONL line.
//     Any failure logs a WARN and returns the partial slice so the writer
//     pipeline keeps moving. We never return a non-nil error from Extract.
//
// Skip rules per Phase K § 2 (and operator clarification 2026-05-30):
//
//   - body_b64 is the unparseable-fallback container, NOT an attachment.
//     Extractor skips it entirely.
//   - http(s):// and gs:// / file: URL-only references are not fetched.
//   - Empty / whitespace base64 strings are silently skipped.
//
// Package owns four files: media.go (public surface), walk.go (generic JSON
// walker), protocols.go (per-protocol field tables), decode.go (base64 +
// data-URL + mime helpers). Tests are walk_test.go and protocols_test.go.
package media

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/2nd1st/api-log/internal/ids"
	"github.com/2nd1st/api-log/internal/trace"
)

// Config is the extractor's runtime configuration. Wired by main from the
// process-level config object; the extractor itself never reads YAML/env.
type Config struct {
	// DataDir is the same `${data_dir}` the writer uses. Extracted files
	// live under ${DataDir}/${YYYY-MM-DD}/${keyhash[:8]}/media/${trace_id}/.
	DataDir string
}

// MediaFile mirrors the struct in Phase K § 3 exactly. JSON tags are the
// shape the SQLite mirror / viewer API expect.
type MediaFile struct {
	// Idx is the 0-based ordinal across BOTH sides of the trace, request
	// media first (document order) then response media.
	Idx int `json:"idx"`

	// Side is "req" or "resp".
	Side string `json:"side"`

	// SourceField is the dotted/bracketed JSON path inside the side body
	// where this media was found. Examples:
	//   "messages[0].content[0].image_url.url"
	//   "contents[1].parts[3].inlineData.data"
	SourceField string `json:"sourceField"`

	// MimeType is the protocol-declared mime if present, else inferred from
	// a data: URL header, else "application/octet-stream".
	MimeType string `json:"mimeType"`

	// Extension is the lower-case extension derived ONLY from MimeType
	// (no leading dot). Never inferred from a filename.
	Extension string `json:"extension"`

	// Size is the decoded byte length of the written file.
	Size int64 `json:"size"`

	// Filename is the path relative to the per-key media dir; full disk
	// path is `${DataDir}/${date}/${keyhash[:8]}/${Filename}`.
	// Format: `media/${trace_id}/${idx}.${ext}`.
	Filename string `json:"filename"`
}

// Extractor is the public type. Construct via New, call Extract per trace.
// Safe for concurrent use across goroutines (no shared mutable state beyond
// the underlying filesystem).
type Extractor struct {
	cfg Config
}

// New constructs an Extractor. cfg.DataDir must be non-empty for files to
// be written; with an empty DataDir Extract() still walks the bodies and
// returns metadata but writes no files (test-friendly).
func New(cfg Config) *Extractor {
	return &Extractor{cfg: cfg}
}

// Extract walks t.Req.Body and t.Resp.Body, identifies media-bearing fields
// per the documented protocols, decodes their base64 payloads, writes the
// files under ${DataDir}/${date}/${keyhash[:8]}/media/${trace_id}/, and
// returns the list of MediaFile records.
//
// Per PHILOSOPHY § 2 the function never returns an error. Per-file failures
// (mkdir denied, ENOSPC, malformed base64, …) are logged at WARN and the
// corresponding MediaFile is omitted from the returned slice. The writer
// pipeline must keep moving regardless.
//
// body_b64 is skipped unconditionally; URL-only image references are
// skipped (no remote fetch). See package doc for the full skip-list.
func (e *Extractor) Extract(t trace.Trace) []MediaFile {
	out := make([]MediaFile, 0, 4)

	// Request side first, then response — matches the idx ordering in
	// Phase K § 3 ("Order: all request media first ..., then response").
	reqCands := findCandidates(t.Req.Body, "req")
	respCands := findCandidates(t.Resp.Body, "resp")

	allCands := make([]candidate, 0, len(reqCands)+len(respCands))
	allCands = append(allCands, reqCands...)
	allCands = append(allCands, respCands...)

	if len(allCands) == 0 {
		return out
	}

	// Derive the on-disk parent directory the same way writer.go does, but
	// using TsStart (the trace's request time) rather than wall-clock. This
	// matches the worked example in Phase K § 4 where the date is the
	// recording date of the trace itself.
	date := t.TsStart.UTC().Format("2006-01-02")
	keyHash := ids.KeyHashFromHeaders(http.Header(t.Req.Headers))
	hashShort := ids.KeyHashShort(keyHash)

	var parentDir string
	if e.cfg.DataDir != "" {
		parentDir = filepath.Join(e.cfg.DataDir, date, hashShort, "media", t.ID)
		if err := os.MkdirAll(parentDir, 0o755); err != nil {
			slog.Warn("media extractor: mkdir failed",
				"trace_id", t.ID, "dir", parentDir, "err", err)
			// Continue: we'll skip writes but still return decoded metadata
			// if individual writes happen to succeed (they won't, but the
			// shape is consistent).
		}
	}

	idx := 0
	for _, c := range allCands {
		data, mimeType, ok := decodePayload(c.payload, c.declaredMime)
		if !ok {
			// Empty / unparseable / URL-only: silently skip per § 2.
			continue
		}
		ext := extensionFor(mimeType)
		filename := "media/" + t.ID + "/" + strconv.Itoa(idx) + "." + ext

		if parentDir != "" {
			fullPath := filepath.Join(parentDir, strconv.Itoa(idx)+"."+ext)
			if err := os.WriteFile(fullPath, data, 0o644); err != nil {
				slog.Warn("media extractor: write failed",
					"trace_id", t.ID, "path", fullPath, "err", err)
				continue
			}
		}

		out = append(out, MediaFile{
			Idx:         idx,
			Side:        c.side,
			SourceField: c.path,
			MimeType:    mimeType,
			Extension:   ext,
			Size:        int64(len(data)),
			Filename:    filename,
		})
		idx++
	}

	return out
}
