package api

// Config handlers for runtime-mutable settings.
//
// Per uiux-research/phase-k-media-contract.md §5.2 / §5.3:
//   GET  /api/config/media → current effective save_attachments + source
//   PUT  /api/config/media → set save_attachments, persist as override
//
// Truth model:
//   - The effective bool lives in deps.MediaEnabled (an *atomic.Bool
//     populated at startup by the YAML/env/override chain and updated
//     in place by the PUT handler). Always read from it, never from
//     the on-disk override file — the file is the persistence layer,
//     not the source of truth at request time.
//   - The override file (`<DataDir>/runtime_overrides.json`) is consulted
//     only to decide whether the current value originated from a runtime
//     override or from the underlying yaml/default/env layer. We cannot
//     distinguish yaml vs default vs env from inside this package; the
//     Integrate phase may refine this if Deps grows a MediaSource field.

import (
	"encoding/json"
	"net/http"

	"github.com/xiayangzhang/api-log/internal/runtime"
)

type mediaConfigJSON struct {
	SaveAttachments bool   `json:"save_attachments"`
	Source          string `json:"source,omitempty"`
}

func getConfigMedia(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mediaConfigJSON{
			SaveAttachments: deps.MediaEnabled.Load(),
			Source:          currentMediaSource(deps),
		})
	})
}

func putConfigMedia(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SaveAttachments *bool `json:"save_attachments"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_body",
				map[string]string{"detail": err.Error()})
			return
		}
		if body.SaveAttachments == nil {
			writeError(w, http.StatusBadRequest, "missing_field",
				map[string]string{"field": "save_attachments"})
			return
		}

		// Persist first, then update the live flag. Order matters: if
		// the disk write fails we don't want the in-memory state to
		// have already drifted from what's recoverable on restart.
		//
		// runtime.SaveOverride uses a mutator so we only touch the
		// media.save_attachments field — sibling overrides (if any are
		// added later) are preserved.
		newVal := *body.SaveAttachments
		err := runtime.SaveOverride(deps.DataDir, func(ov *runtime.Overrides) {
			ov.Media.SaveAttachments = &newVal
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "override_write_failed",
				map[string]string{"detail": err.Error()})
			return
		}
		deps.MediaEnabled.Store(newVal)

		writeJSON(w, http.StatusOK, mediaConfigJSON{
			SaveAttachments: deps.MediaEnabled.Load(),
			Source:          "override",
		})
	})
}

// currentMediaSource consults the override file to decide whether the
// effective value is a runtime override or a config-layer value. We
// report "yaml" for the non-override case — yaml/default/env all flow
// through the same startup chain and are indistinguishable from here.
// Integrate may replace this with a Deps-provided MediaSource string
// captured at config-load time for a more precise answer.
//
// If LoadOverrides errors (malformed file, perms), we treat the override
// as absent rather than surfacing a 500 — the effective flag in
// deps.MediaEnabled is still the truth either way.
func currentMediaSource(deps Deps) string {
	ov, err := runtime.LoadOverrides(deps.DataDir)
	if err != nil {
		return "yaml"
	}
	if ov.Media.SaveAttachments != nil {
		return "override"
	}
	return "yaml"
}
