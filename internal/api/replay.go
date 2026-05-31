package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/xiayangzhang/api-log/internal/sse"
	"github.com/xiayangzhang/api-log/internal/store/sqlite"
)

// replayHandler implements GET /api/traces/{id}/replay per ARCHITECTURE § 6.4.
//
// Behavior summary:
//   - 400 not_streaming if the trace's resp is non-streaming (has .body, not .events).
//   - Reconstructs SSE frames from the recorded {event, data} pairs.
//   - Between events: sleeps (events[i].t_delta_ms - events[i-1].t_delta_ms) / speed ms.
//   - Null t_delta_ms (sentinel for reparse-reconstructed traces, or encoded
//     responses): emit immediately for that gap.
//   - `?speed=N` (default 1.0, range (0, 100]): delay multiplier in *divisive*
//     direction. speed=2 → 2× faster (sleeps halved). speed=0.5 → half-speed.
//   - `?nodelay=1`: skip all sleeps; dump back-to-back.
//   - Cancellation: if the client disconnects, abort the loop promptly.
func replayHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "missing_id")
			return
		}

		// Parse params first; reject before we touch SQLite if invalid.
		speed, nodelay, errCode := parseReplayParams(r)
		if errCode != "" {
			writeError(w, http.StatusBadRequest, "invalid_param", map[string]string{"param": errCode})
			return
		}

		row, err := deps.Store.GetByID(id)
		if err != nil {
			if errors.Is(err, sqlite.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found")
				return
			}
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}

		line, err := readJSONLLine(row.JSONLPath, row.JSONLOffset)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "jsonl_read_failed")
			return
		}

		events, hasEvents, err := extractEventsFromLine(line)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "trace_parse_failed")
			return
		}
		if !hasEvents {
			writeError(w, http.StatusBadRequest, "not_streaming")
			return
		}

		// Stream the response.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)

		var prevDelta *int64
		ctx := r.Context()
		for _, e := range events {
			// Sleep before emitting (except first event).
			if !nodelay && prevDelta != nil && e.TDeltaMs != nil {
				gapMs := *e.TDeltaMs - *prevDelta
				if gapMs > 0 {
					sleep := time.Duration(float64(gapMs)/speed) * time.Millisecond
					select {
					case <-time.After(sleep):
					case <-ctx.Done():
						return
					}
				}
			}
			// (Otherwise: emit immediately — either first event, or a
			// null-t_delta_ms fallback for reparse-reconstructed events.)

			if err := writeEventFrame(w, e); err != nil {
				return // client disconnected or transient write error
			}
			if flusher != nil {
				flusher.Flush()
			}
			if e.TDeltaMs != nil {
				prevDelta = e.TDeltaMs
			}
		}
	})
}

func parseReplayParams(r *http.Request) (speed float64, nodelay bool, errParam string) {
	q := r.URL.Query()
	speed = 1.0
	if v := q.Get("speed"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 100 {
			return 0, false, "speed"
		}
		speed = f
	}
	if v := q.Get("nodelay"); v == "1" || v == "true" {
		nodelay = true
	}
	return speed, nodelay, ""
}

// extractEventsFromLine parses the full JSONL line and returns the
// resp.events array (if any). hasEvents=false means the response was
// non-streaming (.body instead of .events).
func extractEventsFromLine(line []byte) ([]sse.Event, bool, error) {
	var raw struct {
		Resp struct {
			Body   json.RawMessage `json:"body"`
			Events []sse.Event     `json:"events"`
		} `json:"resp"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, false, err
	}
	if len(raw.Resp.Events) == 0 {
		// .body present (non-streaming) or both absent (empty body) → not replayable.
		return nil, false, nil
	}
	return raw.Resp.Events, true, nil
}

// writeEventFrame writes one SSE frame to w. Event name omitted when empty
// (matches OpenAI Chat data-only wire format).
func writeEventFrame(w http.ResponseWriter, e sse.Event) error {
	if e.Name != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", e.Name); err != nil {
			return err
		}
	}
	if len(e.Data) == 0 {
		if _, err := fmt.Fprint(w, "data: \n\n"); err != nil {
			return err
		}
		return nil
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", string(e.Data))
	return err
}
