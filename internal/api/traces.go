package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/2nd1st/api-log/internal/store/sqlite"
)

// listTraces handles GET /api/traces. Response shape per ARCHITECTURE § 6.2.
//
// The Row is serialized to a frontend-friendly JSON shape — flat keys,
// timestamps as RFC3339, nullable Go *T values become JSON null/value.
func listTraces(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filters, errCode, errDetail := parseListFilters(r)
		if errCode != "" {
			writeError(w, http.StatusBadRequest, errCode, map[string]string{"param": errDetail})
			return
		}

		page, err := deps.Store.List(filters)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}

		out := make([]rowJSON, 0, len(page.Rows))
		for _, r := range page.Rows {
			out = append(out, rowToJSON(r))
		}

		resp := map[string]any{
			"traces": out,
		}
		if page.NextCursorID != "" {
			resp["next_cursor"] = encodeCursor(page.NextCursorMs, page.NextCursorID)
		} else {
			resp["next_cursor"] = nil
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// getTrace handles GET /api/traces/{id}. Returns the SQLite row + the
// full parsed JSONL line (req + resp bodies).
func getTrace(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "missing_id")
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

		// Seek into the JSONL file and read the line at jsonl_offset.
		line, err := readJSONLLine(row.JSONLPath, row.JSONLOffset)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "jsonl_read_failed",
				map[string]string{"detail": err.Error()})
			return
		}

		// Compose the response: row metadata + the full trace JSON line.
		// We embed the parsed line as `trace` so the caller sees both
		// SQLite columns and the original JSONL payload in one shot.
		body := map[string]any{
			"row":   rowToJSON(row),
			"trace": json.RawMessage(line), // pre-parsed for the consumer
		}
		writeJSON(w, http.StatusOK, body)
	})
}

// ---- query-param parsing ----

func parseListFilters(r *http.Request) (sqlite.ListFilters, string, string) {
	q := r.URL.Query()
	f := sqlite.ListFilters{}

	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, "bad_param", "since"
		}
		f.Since = t
	}
	if v := q.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, "bad_param", "until"
		}
		f.Until = t
	}
	if v := q.Get("status"); v != "" {
		// Accept "2xx" / "4xx" / "5xx" as a range filter; anything else
		// must parse as an exact status code.
		if len(v) == 3 && (v[1] == 'x' || v[1] == 'X') && (v[2] == 'x' || v[2] == 'X') {
			switch v[0] {
			case '2', '4', '5':
				f.StatusBucket = int(v[0] - '0')
			default:
				return f, "bad_param", "status"
			}
		} else {
			n, err := strconv.Atoi(v)
			if err != nil {
				return f, "bad_param", "status"
			}
			f.Status = &n
		}
	}
	if v := q.Get("model"); v != "" {
		f.Model = v
	}
	if v := q.Get("path"); v != "" {
		// Trailing "*" makes it a prefix match; otherwise exact.
		// Useful default: /v1/* to hide /api/v1/* admin UI noise.
		if strings.HasSuffix(v, "*") {
			f.PathPrefix = strings.TrimSuffix(v, "*")
		} else {
			f.Path = v
		}
	}
	if v := q.Get("key_hash"); v != "" {
		// Accept 8- or 16-char prefix.
		if len(v) != 8 && len(v) != 16 {
			return f, "bad_param", "key_hash"
		}
		f.KeyHashPrefix = v
	}
	if v := q.Get("session_root_id"); v != "" {
		f.SessionRootID = v
	}
	if v := q.Get("project"); v != "" {
		// Exact match on client_project so callers can scope a list to
		// one project once the finalize-time extractor has populated the
		// column.
		f.Project = v
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return f, "bad_param", "limit"
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		ms, id, err := decodeCursor(v)
		if err != nil {
			return f, "bad_param", "cursor"
		}
		f.CursorTsStart = ms
		f.CursorID = id
	}
	return f, "", ""
}

// Cursors are opaque base64-encoded "<ts_ms>:<id>".
func encodeCursor(tsMs int64, id string) string {
	s := strconv.FormatInt(tsMs, 10) + ":" + id
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func decodeCursor(s string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, "", err
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return 0, "", errors.New("malformed cursor")
	}
	ms, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", err
	}
	return ms, parts[1], nil
}

// ---- row JSON shape (matches ARCHITECTURE § 6.2 example) ----

type rowJSON struct {
	ID                  string  `json:"id"`
	TsStart             string  `json:"ts_start"`
	TsEnd               string  `json:"ts_end"`
	Client              string  `json:"client"`
	Method              string  `json:"method"`
	Path                string  `json:"path"`
	Upstream            string  `json:"upstream"`
	Status              int     `json:"status"`
	Model               *string `json:"model"`
	Stream              *bool   `json:"stream"`
	PromptTokens        *int64  `json:"prompt_tokens"`
	CompletionTokens    *int64  `json:"completion_tokens"`
	TotalTokens         *int64  `json:"total_tokens"`
	CachedTokens        *int64  `json:"cached_tokens"`
	CacheCreationTokens *int64  `json:"cache_creation_tokens"`
	ReasoningTokens     *int64  `json:"reasoning_tokens"`
	FinishReason        *string `json:"finish_reason"`
	ClientKind          *string `json:"client_kind"`
	ClientVersion       *string `json:"client_version"`
	ClientProject       *string `json:"client_project"`
	KeyHash             string  `json:"key_hash"`
	ParentID            *string `json:"parent_id"`
	SessionRootID       string  `json:"session_root_id"`
	Disconnected        bool    `json:"disconnected"`
	TruncatedReq        bool    `json:"truncated_req"`
	TruncatedResp       bool    `json:"truncated_resp"`
	MediaCount          int     `json:"media_count"`
	JSONLPath           string  `json:"jsonl_path"`
	JSONLOffset         int64   `json:"jsonl_offset"`
}

func rowToJSON(r sqlite.Row) rowJSON {
	return rowJSON{
		ID:                  r.ID,
		TsStart:             r.TsStart.UTC().Format(time.RFC3339Nano),
		TsEnd:               r.TsEnd.UTC().Format(time.RFC3339Nano),
		Client:              r.Client,
		Method:              r.Method,
		Path:                r.Path,
		Upstream:            r.Upstream,
		Status:              r.Status,
		Model:               r.Model,
		Stream:              r.Stream,
		PromptTokens:        r.PromptTokens,
		CompletionTokens:    r.CompletionTokens,
		TotalTokens:         r.TotalTokens,
		CachedTokens:        r.CachedTokens,
		CacheCreationTokens: r.CacheCreationTokens,
		ReasoningTokens:     r.ReasoningTokens,
		FinishReason:        r.FinishReason,
		ClientKind:          r.ClientKind,
		ClientVersion:       r.ClientVersion,
		ClientProject:       r.ClientProject,
		KeyHash:             r.KeyHash,
		ParentID:            r.ParentID,
		SessionRootID:       r.SessionRootID,
		Disconnected:        r.Disconnected,
		TruncatedReq:        r.TruncatedReq,
		TruncatedResp:       r.TruncatedResp,
		MediaCount:          r.MediaCount,
		JSONLPath:           r.JSONLPath,
		JSONLOffset:         r.JSONLOffset,
	}
}
