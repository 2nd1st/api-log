// Package integration_test exercises the full proxy pipeline against
// a httptest mock upstream, with v2 plugins enabled via a seeded
// runtime_overrides.json. Verifies that BEFORE mutates request bodies
// the upstream actually receives, AFTER decorates responses the client
// actually receives, and that streaming `tool_use` `input_json_delta`
// events pass through untouched per spec §10.6.
//
// The test compiles the api-log binary in a temp dir, points it at the
// httptest upstream, and drives it over the real proxy listener. This
// is slower than a unit test (one go build per run) but is the only
// honest end-to-end check for the io.Pipe + ModifyResponse plumbing
// the W6 wiring introduces.
package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPluginWiring_NonStreamingChat asserts the BEFORE chain mutates
// the request the upstream receives, and the AFTER chain decorates the
// response the client receives, for a non-streaming Chat call.
//
// Two plugins are seeded via runtime_overrides.json:
//
//   - text-replace: up "你" → "世界上最好最好的ai"
//   - text-append:  down suffix " --watermark"
//
// Request payload contains "你好世界"; we expect the upstream-recorded
// request body to carry "世界上最好最好的ai好世界" and the client to see
// the assistant reply with " --watermark" appended.
func TestPluginWiring_NonStreamingChat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped in -short mode")
	}

	var upstreamReqBody atomicBytes
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamReqBody.Store(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	defer upstream.Close()

	env := bootProxy(t, upstream.URL, runtimeOverridesNonStreaming())
	defer env.shutdown()

	body := `{"model":"mock-gpt","messages":[{"role":"user","content":"你好世界"}]}`
	resp, err := http.Post(env.proxyURL+"/v1/chat/completions",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST proxy: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// 1. Upstream saw the mutated request body.
	got := string(upstreamReqBody.Load())
	if !strings.Contains(got, "世界上最好最好的ai好世界") {
		t.Errorf("upstream did not receive replacement; body=%s", got)
	}
	if strings.Contains(got, "你好世界") {
		t.Errorf("upstream still saw original needle; body=%s", got)
	}

	// 2. Client got the response decorated with the watermark suffix.
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		t.Fatalf("client resp not JSON: %v body=%s", err, respBody)
	}
	if len(parsed.Choices) == 0 {
		t.Fatalf("no choices: %s", respBody)
	}
	content := parsed.Choices[0].Message.Content
	if !strings.HasSuffix(content, " --watermark") {
		t.Errorf("client content missing watermark suffix; content=%q", content)
	}
}

// TestPluginWiring_StreamingMessages_ToolUseUntouched asserts the
// AFTER-streaming dispatcher injects a synthesized content delta
// before message_stop AND leaves `input_json_delta` events byte-
// identical (spec §10.6).
func TestPluginWiring_StreamingMessages_ToolUseUntouched(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped in -short mode")
	}

	// Mock upstream emits an Anthropic stream containing one text
	// delta, one tool_use input_json_delta carrying the literal needle,
	// and a message_stop terminator.
	const toolJSONFragment = `{"argument":"contains needle here"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeFrame := func(name, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeFrame("message_start", `{"type":"message_start","message":{"id":"msg_1","role":"assistant"}}`)
		writeFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)
		writeFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello there"}}`)
		writeFrame("content_block_stop", `{"type":"content_block_stop","index":0}`)
		writeFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"calc"}}`)
		writeFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+jsonString(toolJSONFragment)+`}}`)
		writeFrame("content_block_stop", `{"type":"content_block_stop","index":1}`)
		writeFrame("message_stop", `{"type":"message_stop"}`)
	}))
	defer upstream.Close()

	env := bootProxy(t, upstream.URL, runtimeOverridesStreaming())
	defer env.shutdown()

	req, _ := http.NewRequest("POST", env.proxyURL+"/v1/messages",
		strings.NewReader(`{"model":"mock-claude","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST proxy stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	events := readSSEFrames(t, resp.Body)

	// Assertion 1: input_json_delta event is present and its partial_json
	// payload decodes to the literal toolJSONFragment string (carve-out
	// per spec §10.6 — tool_use deltas pass through untouched even when
	// content transforms are registered).
	found := false
	for _, ev := range events {
		if ev.name != "content_block_delta" {
			continue
		}
		var frame struct {
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(ev.data, &frame); err != nil {
			continue
		}
		if frame.Delta.Type != "input_json_delta" {
			continue
		}
		found = true
		if frame.Delta.PartialJSON != toolJSONFragment {
			t.Errorf("input_json_delta partial_json mutated; got=%q want=%q",
				frame.Delta.PartialJSON, toolJSONFragment)
		}
		break
	}
	if !found {
		t.Errorf("input_json_delta event not seen; events=%v", eventNames(events))
	}

	// Assertion 2: a synthesized content delta carrying the watermark
	// suffix lands BEFORE message_stop.
	suffix := "stream-tail"
	var sawSuffix, sawStop bool
	for _, ev := range events {
		if ev.name == "message_stop" {
			sawStop = true
		}
		if !sawStop && ev.name == "content_block_delta" && bytes.Contains(ev.data, []byte(suffix)) {
			sawSuffix = true
		}
	}
	if !sawSuffix {
		t.Errorf("synthesized suffix delta not seen before message_stop; events=%v", eventNames(events))
	}
	if !sawStop {
		t.Errorf("message_stop event not seen; events=%v", eventNames(events))
	}
}

// ---- helpers ----

// atomicBytes is a tiny goroutine-safe byte-slice cell. Used to record
// the body the upstream mock saw without dragging in sync/atomic.Pointer
// gymnastics for what is essentially a one-write/one-read assertion.
type atomicBytes struct {
	mu sync.Mutex
	b  []byte
}

func (a *atomicBytes) Store(b []byte) {
	a.mu.Lock()
	a.b = append(a.b[:0], b...)
	a.mu.Unlock()
}
func (a *atomicBytes) Load() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]byte(nil), a.b...)
}

type sseFrame struct {
	name string
	data []byte
}

// readSSEFrames consumes an SSE stream into a slice of frames. Used by
// both streaming assertions.
func readSSEFrames(t *testing.T, body io.ReadCloser) []sseFrame {
	t.Helper()
	br := bufio.NewReader(body)
	var (
		out  []sseFrame
		name string
		data bytes.Buffer
		has  bool
	)
	flush := func() {
		if has || name != "" {
			out = append(out, sseFrame{name: name, data: append([]byte(nil), data.Bytes()...)})
		}
		name = ""
		data.Reset()
		has = false
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trim := line
			if n := len(trim); n > 0 && trim[n-1] == '\n' {
				trim = trim[:n-1]
			}
			if n := len(trim); n > 0 && trim[n-1] == '\r' {
				trim = trim[:n-1]
			}
			if len(trim) == 0 {
				flush()
			} else if bytes.HasPrefix(trim, []byte("event: ")) {
				name = string(trim[len("event: "):])
			} else if bytes.HasPrefix(trim, []byte("data: ")) {
				if has {
					data.WriteByte('\n')
				}
				data.Write(trim[len("data: "):])
				has = true
			}
		}
		if err != nil {
			break
		}
	}
	flush()
	return out
}

func eventNames(evs []sseFrame) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.name)
	}
	return out
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// proxyEnv holds the running api-log subprocess + URLs.
type proxyEnv struct {
	t        *testing.T
	cmd      *exec.Cmd
	proxyURL string
	apiURL   string
	dataDir  string
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
}

func (e *proxyEnv) shutdown() {
	if e.cmd != nil && e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
		_, _ = e.cmd.Process.Wait()
	}
	if e.dataDir != "" {
		_ = os.RemoveAll(e.dataDir)
	}
}

// bootProxy builds the api-log binary, seeds runtime_overrides.json
// with the given JSON, starts the process pointed at upstreamURL, and
// blocks until the proxy listener accepts a TCP connection.
func bootProxy(t *testing.T, upstreamURL, runtimeOverrides string) *proxyEnv {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "runtime_overrides.json"),
		[]byte(runtimeOverrides), 0o644); err != nil {
		t.Fatalf("seed runtime_overrides: %v", err)
	}

	binPath := filepath.Join(dataDir, "api-log-test")
	repoRoot := repoRootFromTest(t)
	build := exec.Command("go", "build", "-o", binPath, "./cmd/api-log")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("go build api-log: %v\n%s", err, out)
	}

	proxyPort := freePort(t)
	apiPort := freePort(t)
	env := &proxyEnv{
		t:        t,
		dataDir:  dataDir,
		proxyURL: fmt.Sprintf("http://127.0.0.1:%d", proxyPort),
		apiURL:   fmt.Sprintf("http://127.0.0.1:%d", apiPort),
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
	}
	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(),
		"APILOG_PROXY_LISTEN=127.0.0.1:"+itoa(proxyPort),
		"APILOG_API_LISTEN=127.0.0.1:"+itoa(apiPort),
		"APILOG_PROXY_UPSTREAM="+upstreamURL,
		"APILOG_STORAGE_DATA_DIR="+dataDir,
		"APILOG_LOGGING_LEVEL=warn",
		"APILOG_DIAGNOSTICS_SNAPSHOT_INTERVAL_SECONDS=0",
	)
	cmd.Stdout = env.stdout
	cmd.Stderr = env.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start api-log: %v", err)
	}
	env.cmd = cmd

	if !waitForListener(env.proxyURL, 10*time.Second) {
		env.shutdown()
		t.Fatalf("proxy did not start on %s; stderr=%s", env.proxyURL, env.stderr.String())
	}
	return env
}

func waitForListener(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, "GET", url+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// repoRootFromTest walks up from the test file's directory until it
// finds go.mod; that's the api-log repo root.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find go.mod from %s", wd)
	return ""
}

// runtimeOverridesNonStreaming seeds text-replace (up rule) +
// text-append (down rule) for the non-streaming Chat test.
func runtimeOverridesNonStreaming() string {
	return `{
	  "plugins": {
	    "instances": [
	      {
	        "type": "text-replace",
	        "id": "tr-1",
	        "enabled": true,
	        "config": {
	          "up": [{"match": "你", "replace": "世界上最好最好的ai"}]
	        }
	      },
	      {
	        "type": "text-append",
	        "id": "ta-1",
	        "enabled": true,
	        "config": {
	          "down": {"suffix": " --watermark", "target": "content"}
	        }
	      }
	    ]
	  }
	}`
}

// runtimeOverridesStreaming seeds text-append's AFTER half for the
// streaming-tail injection test.
func runtimeOverridesStreaming() string {
	return `{
	  "plugins": {
	    "instances": [
	      {
	        "type": "text-append",
	        "id": "ta-stream",
	        "enabled": true,
	        "config": {
	          "down": {"suffix": "stream-tail", "target": "content"}
	        }
	      }
	    ]
	  }
	}`
}
