// Command mockup is a minimal LLM-gateway-shaped HTTP server used in
// the api-log development stack and integration tests.
//
// It speaks three protocol shapes recognized by api-log:
//
//	POST /v1/chat/completions   (OpenAI Chat Completions)
//	POST /v1/responses          (OpenAI Responses)
//	POST /v1/messages           (Anthropic Messages)
//
// For each, when the request body has "stream": true it emits an SSE
// response shaped like the real provider's wire format; otherwise it
// emits a single JSON response with a `usage` block. The body content
// is deterministic and small — these are fixtures for capturing,
// session-inferring, and replaying, not for benchmarking LLM quality.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	addr := flag.String("listen", ":8888", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handleChat)
	mux.HandleFunc("POST /v1/responses", handleResponses)
	mux.HandleFunc("POST /v1/messages", handleMessages)
	mux.HandleFunc("GET /v1/models", handleModels)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})

	log.Printf("mockup listening on %s", *addr)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ----- shared body parsing -----

type genericReq struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Input    json.RawMessage   `json:"input"`
	Stream   bool              `json:"stream"`
}

func readReq(r *http.Request) (genericReq, error) {
	var req genericReq
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return req, err
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return req, err
		}
	}
	return req, nil
}

// chunkDelay sleeps the stream-pacing delay between SSE events.
// Configurable via the X-Mockup-Chunk-Delay-Ms request header so tests
// can dial it up or down.
func chunkDelay(r *http.Request) time.Duration {
	if v := r.Header.Get("X-Mockup-Chunk-Delay-Ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 && ms <= 10000 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 100 * time.Millisecond
}

// ----- OpenAI Chat Completions -----

func handleChat(w http.ResponseWriter, r *http.Request) {
	req, err := readReq(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-mock-1",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11,
			},
		})
		return
	}

	// Streaming: data-only SSE terminated by [DONE].
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)
	delay := chunkDelay(r)
	emit := func(payload any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flush != nil {
			flush.Flush()
		}
	}
	for i, tok := range []string{"H", "i"} {
		emit(map[string]any{
			"id":      "chatcmpl-mock-1",
			"object":  "chat.completion.chunk",
			"model":   req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": tok}}},
		})
		if i == 0 {
			time.Sleep(delay)
		}
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flush != nil {
		flush.Flush()
	}
}

// ----- OpenAI Responses -----

func handleResponses(w http.ResponseWriter, r *http.Request) {
	req, err := readReq(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_mock_1",
			"object": "response",
			"model":  req.Model,
			"status": "completed",
			"output": []any{map[string]any{
				"id":      "msg_mock_1",
				"type":    "message",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "ok"}},
			}},
			"usage": map[string]any{"input_tokens": 8, "output_tokens": 1, "total_tokens": 9},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)
	delay := chunkDelay(r)
	emit := func(name string, payload any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if flush != nil {
			flush.Flush()
		}
	}
	emit("response.created", map[string]any{
		"type":            "response.created",
		"response":        map[string]any{"id": "resp_mock_1", "model": req.Model},
		"sequence_number": 0,
	})
	time.Sleep(delay)
	emit("response.output_text.delta", map[string]any{
		"type":            "response.output_text.delta",
		"delta":           "ok",
		"sequence_number": 1,
	})
	time.Sleep(delay)
	emit("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":    "resp_mock_1",
			"model": req.Model,
			"usage": map[string]any{"input_tokens": 8, "output_tokens": 1, "total_tokens": 9},
		},
	})
}

// ----- Anthropic Messages -----

func handleMessages(w http.ResponseWriter, r *http.Request) {
	req, err := readReq(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_mock_1",
			"type":        "message",
			"role":        "assistant",
			"model":       req.Model,
			"content":     []any{map[string]any{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 1},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)
	delay := chunkDelay(r)
	emit := func(name string, payload any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if flush != nil {
			flush.Flush()
		}
	}
	emit("message_start", map[string]any{
		"type":    "message_start",
		"message": map[string]any{"id": "msg_mock_1", "model": req.Model, "usage": map[string]any{"input_tokens": 10, "output_tokens": 0}},
	})
	time.Sleep(delay)
	emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "ok"},
	})
	time.Sleep(delay)
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 1},
	})
	time.Sleep(delay)
	emit("message_stop", map[string]any{"type": "message_stop"})
}

// ----- /v1/models -----

func handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": []any{
			map[string]any{"id": "mock-gpt", "object": "model", "owned_by": "mockup"},
			map[string]any{"id": "mock-claude", "object": "model", "owned_by": "mockup"},
		},
		"object": "list",
	})
}
