package api

// Retention config API: GET reports current coord state + thresholds;
// PUT validates + applies via coord.UpdateConfig + persists to
// runtime_overrides.json so the next process start picks it up.
//
// Truth model:
//   - coord.Status() is the live truth at request time. The override
//     file is the persistence layer, not the source of truth at runtime.
//   - PUT updates the coord first via UpdateConfig (so reads see the
//     new value immediately), THEN persists to disk. If persistence
//     fails we surface a 500 but the in-memory coord already holds the
//     new value — that's the explicit trade-off matched to the media
//     handler's "in-memory + persistence" pattern. Operators can retry
//     the PUT to re-attempt the disk write; the live behavior is
//     already what they wanted.
//   - Both knobs zero == retention disabled (coord.UpdateConfig clears
//     the retention pointer). PUT accepts that explicitly so adopters
//     can turn the feature OFF without restart.

import (
	"encoding/json"
	"net/http"

	"github.com/2nd1st/api-log/internal/runtime"
	"github.com/2nd1st/api-log/internal/storage"
)

type retentionConfigJSON struct {
	MaxBytes      int64  `json:"max_bytes"`
	MaxAgeDays    int    `json:"max_age_days"`
	WarnAtPercent int    `json:"warn_at_percent"`
	Source        string `json:"source,omitempty"`
}

func getConfigRetention(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deps.StorageCoord == nil {
			writeError(w, http.StatusServiceUnavailable, "storage_disabled",
				map[string]string{"detail": "storage coordinator not wired"})
			return
		}
		s := deps.StorageCoord.Status()
		writeJSON(w, http.StatusOK, retentionConfigJSON{
			MaxBytes:      s.MaxBytes,
			MaxAgeDays:    s.MaxAgeDays,
			WarnAtPercent: currentRetentionWarnAtPercent(deps),
			Source:        currentRetentionSource(deps),
		})
	})
}

func putConfigRetention(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deps.StorageCoord == nil {
			writeError(w, http.StatusServiceUnavailable, "storage_disabled",
				map[string]string{"detail": "storage coordinator not wired"})
			return
		}

		var body struct {
			MaxBytes      *int64 `json:"max_bytes"`
			MaxAgeDays    *int   `json:"max_age_days"`
			WarnAtPercent *int   `json:"warn_at_percent"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_body",
				map[string]string{"detail": err.Error()})
			return
		}

		// Build the RetentionConfig with the operator-supplied values;
		// nil sub-fields mean "leave at the documented default" rather
		// than "use the previous value" — same convention as media.
		retention := storage.RetentionConfig{}
		if body.MaxBytes != nil {
			retention.MaxBytes = *body.MaxBytes
		}
		if body.MaxAgeDays != nil {
			retention.MaxAgeDays = *body.MaxAgeDays
		}
		if body.WarnAtPercent != nil {
			retention.WarnAtPercent = *body.WarnAtPercent
		}

		// Apply to coord first — UpdateConfig validates the same way
		// New() validates Config (rejects negative knobs / out-of-range
		// percentages). Validation failures surface as 400 before we
		// touch disk.
		if err := deps.StorageCoord.UpdateConfig(retention); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_retention",
				map[string]string{"detail": err.Error()})
			return
		}

		// Persist. Capture the operator-supplied pointers verbatim so a
		// later GET can tell "operator explicitly set max_bytes to 0"
		// from "operator only set max_age_days, max_bytes still default."
		err := runtime.SaveOverride(deps.DataDir, func(ov *runtime.Overrides) {
			if ov.Retention == nil {
				ov.Retention = &runtime.RetentionOverrides{}
			}
			if body.MaxBytes != nil {
				v := *body.MaxBytes
				ov.Retention.MaxBytes = &v
			}
			if body.MaxAgeDays != nil {
				v := *body.MaxAgeDays
				ov.Retention.MaxAgeDays = &v
			}
			if body.WarnAtPercent != nil {
				v := *body.WarnAtPercent
				ov.Retention.WarnAtPercent = &v
			}
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "override_write_failed",
				map[string]string{"detail": err.Error()})
			return
		}

		// Re-read the (now-up-to-date) status for the response body so
		// callers don't have to issue a separate GET to confirm.
		s := deps.StorageCoord.Status()
		writeJSON(w, http.StatusOK, retentionConfigJSON{
			MaxBytes:      s.MaxBytes,
			MaxAgeDays:    s.MaxAgeDays,
			WarnAtPercent: currentRetentionWarnAtPercent(deps),
			Source:        "override",
		})
	})
}

// currentRetentionSource mirrors currentMediaSource — checks the
// override file to decide whether to label the source "override" or
// "yaml" (== default in the absence of a yaml retention block).
func currentRetentionSource(deps Deps) string {
	ov, err := runtime.LoadOverrides(deps.DataDir)
	if err != nil {
		return "yaml"
	}
	if ov.Retention != nil {
		return "override"
	}
	return "yaml"
}

// currentRetentionWarnAtPercent re-reads the persisted override to
// surface WarnAtPercent. coord.Status() doesn't carry WarnAtPercent
// directly (it's used internally to compute State); the operator-set
// value lives in the override file when an override exists.
func currentRetentionWarnAtPercent(deps Deps) int {
	ov, err := runtime.LoadOverrides(deps.DataDir)
	if err != nil {
		return 80
	}
	if ov.Retention != nil && ov.Retention.WarnAtPercent != nil {
		return *ov.Retention.WarnAtPercent
	}
	return 80
}
