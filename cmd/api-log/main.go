// Command api-log is the transparent recording proxy described in
// ../../README.md, ../../PHILOSOPHY.md, and ../../ARCHITECTURE.md.
//
// v0 milestone scope:
//   M1 (done): forwarding + body capture to tmp files.
//   M2 (this commit): finalize parse + JSONL writer → traces land on disk.
//   M3+:           SQLite mirror + session inference; read API; replay.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"log/slog"

	"github.com/leoyun/api-log/internal/capture"
	"github.com/leoyun/api-log/internal/config"
	"github.com/leoyun/api-log/internal/ids"
	"github.com/leoyun/api-log/internal/logging"
	"github.com/leoyun/api-log/internal/parser"
	"github.com/leoyun/api-log/internal/proxy"
	"github.com/leoyun/api-log/internal/trace"
	"github.com/leoyun/api-log/internal/writer"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "api-log: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to api-log.yaml (empty = defaults + env only)")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config validate: %w", err)
	}

	logging.SetupDefault(cfg.Logging.Level)
	slog.Info("api-log starting",
		"version", "0.0.0-dev",
		"proxy.listen", cfg.Proxy.Listen,
		"proxy.upstream", cfg.Proxy.Upstream,
		"data_dir", cfg.Storage.DataDir,
	)

	// Ensure data/ + data/tmp/ exist (tmp/ is wiped clean).
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	tmpDir, err := capture.NewTmpDir(cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("init tmp dir: %w", err)
	}

	upstreamURL, err := url.Parse(cfg.Proxy.Upstream)
	if err != nil {
		return fmt.Errorf("parse upstream URL: %w", err)
	}

	// Single-writer goroutine for JSONL append. M3 will extend with
	// SQLite upsert inside the same goroutine.
	wrtr := writer.New(cfg.Storage.DataDir, cfg.Storage.WriterChanSize, nil)
	stopWriter := wrtr.Start()
	defer stopWriter()

	// Per-trace registry: holds sinks for the CaptureTransport and the
	// captured req/resp metadata so finalize can build the JSONL line.
	reg := newTraceRegistry()

	innerTransport := http.DefaultTransport.(*http.Transport).Clone()
	innerTransport.DisableCompression = true // ARCHITECTURE § 10.3

	captureTransport := &proxy.CaptureTransport{
		Inner: innerTransport,
		Sinks: reg,
		Meta:  reg,
	}
	rp := proxy.NewReverseProxy(upstreamURL, captureTransport)

	proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := ids.NewTraceID()
		state, err := startTrace(traceID, tmpDir, cfg.Storage.CaptureChanSize, cfg.Storage.MaxBodyBytes)
		if err != nil {
			slog.Error("startTrace failed", "trace_id", traceID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		state.tsStart = time.Now().UTC()
		state.clientAddr = r.RemoteAddr
		state.method = r.Method
		state.path = r.URL.RequestURI()
		reg.put(traceID, state)
		defer reg.remove(traceID)

		// Wrap ResponseWriter so we can record status code AS WRITTEN to
		// the client. ReverseProxy passes upstream's status through to
		// WriteHeader; if upstream errored before headers, ReverseProxy.
		// ErrorHandler writes a 502 instead — we capture either.
		crw := &capturingResponseWriter{ResponseWriter: w, status: -1}

		ctx := proxy.WithTraceID(r.Context(), traceID)
		rp.ServeHTTP(crw, r.WithContext(ctx))

		// FINALIZE — M2 real flow.
		state.finalize(cfg.Timeouts.DrainerJoin())

		// Determine final status code. If the wrapped writer never saw
		// WriteHeader (extreme: handler panicked), use the sentinel.
		finalStatus := crw.status
		if finalStatus == -1 {
			// fall back: try the metadata we captured from RoundTrip.
			if state.respStatus != 0 {
				finalStatus = state.respStatus
			} else {
				finalStatus = -1 // ARCHITECTURE § 3 sentinel
			}
		}

		tr, err := buildTrace(traceID, state, cfg.Proxy.Upstream, finalStatus)
		if err != nil {
			slog.Error("buildTrace failed", "trace_id", traceID, "err", err)
			tmpDir.RemoveTraceFiles(traceID)
			return
		}
		keyHash := ids.KeyHashFromHeaders(state.reqHeader)
		if !wrtr.TrySend(writer.Record{Trace: tr, KeyHash: keyHash}) {
			slog.Warn("writer channel full; trace metadata dropped",
				"trace_id", traceID)
		}
		tmpDir.RemoveTraceFiles(traceID)
	})

	srv := &http.Server{
		Addr:              cfg.Proxy.Listen,
		Handler:           proxyHandler,
		ReadHeaderTimeout: cfg.Timeouts.ReadHeader(),
		IdleTimeout:       cfg.Timeouts.Idle(),
		// WriteTimeout left zero: SSE responses can stream tens of
		// minutes. M6 adds stream-idle watchdog.
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("proxy listener up", "addr", cfg.Proxy.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-rootCtx.Done():
		slog.Info("shutdown signal received, draining")
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("proxy server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Grace())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown timed out", "err", err)
	}
	slog.Info("api-log stopped")
	return nil
}

// --- per-trace state ---

type traceState struct {
	// Capture-side
	reqSink   *capture.Sink
	respSink  *capture.Sink
	reqDone   chan capture.DrainResult
	respDone  chan capture.DrainResult
	reqFile   *os.File
	respFile  *os.File
	reqPath   string
	respPath  string

	// Set by handler when accepting the request.
	tsStart    time.Time
	clientAddr string
	method     string
	path       string

	// Set by CaptureTransport via MetaCapture callbacks.
	reqHeaderMu sync.Mutex
	reqHeader   http.Header
	respHeader  http.Header
	respStatus  int

	// Finalize results.
	once       sync.Once
	tsEnd      time.Time
	reqResult  capture.DrainResult
	respResult capture.DrainResult
}

func startTrace(traceID string, tmpDir *capture.TmpDir, chanSize int, maxBodyBytes int64) (*traceState, error) {
	reqFile, respFile, err := tmpDir.CreateTraceFiles(traceID)
	if err != nil {
		return nil, err
	}

	reqCh := make(chan capture.Chunk, chanSize)
	respCh := make(chan capture.Chunk, chanSize)
	reqSink := &capture.Sink{Ch: reqCh, OnDrop: func() {
		slog.Debug("req capture chan full, dropping", "trace_id", traceID)
	}}
	respSink := &capture.Sink{Ch: respCh, OnDrop: func() {
		slog.Debug("resp capture chan full, dropping", "trace_id", traceID)
	}}

	reqDone := make(chan capture.DrainResult, 1)
	respDone := make(chan capture.DrainResult, 1)
	go func() {
		reqDone <- capture.Drain(reqCh, reqFile, maxBodyBytes)
	}()
	go func() {
		respDone <- capture.Drain(respCh, respFile, maxBodyBytes)
	}()

	return &traceState{
		reqSink:  reqSink,
		respSink: respSink,
		reqDone:  reqDone,
		respDone: respDone,
		reqFile:  reqFile,
		respFile: respFile,
		reqPath:  reqFile.Name(),
		respPath: respFile.Name(),
	}, nil
}

// finalize closes the capture channels and waits for drainers (bounded by
// joinTimeout). Safe to call multiple times.
func (t *traceState) finalize(joinTimeout time.Duration) {
	t.once.Do(func() {
		close(t.reqSink.Ch)
		close(t.respSink.Ch)

		timer := time.NewTimer(joinTimeout)
		defer timer.Stop()

		select {
		case t.reqResult = <-t.reqDone:
		case <-timer.C:
			slog.Warn("req drainer join timeout")
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(joinTimeout)
		select {
		case t.respResult = <-t.respDone:
		case <-timer.C:
			slog.Warn("resp drainer join timeout")
		}

		_ = t.reqFile.Close()
		_ = t.respFile.Close()
		t.tsEnd = time.Now().UTC()
	})
}

// buildTrace assembles the JSONL line from per-trace state after finalize.
func buildTrace(traceID string, st *traceState, upstreamURL string, finalStatus int) (trace.Trace, error) {
	// Read + parse req body from tmp file.
	reqF, err := os.Open(st.reqPath)
	if err != nil {
		return trace.Trace{}, fmt.Errorf("open req tmp: %w", err)
	}
	defer reqF.Close()
	reqBody, err := parser.ParseRequest(reqF, st.reqHeader)
	if err != nil {
		return trace.Trace{}, fmt.Errorf("parse req: %w", err)
	}

	respF, err := os.Open(st.respPath)
	if err != nil {
		return trace.Trace{}, fmt.Errorf("open resp tmp: %w", err)
	}
	defer respF.Close()
	respBody, err := parser.ParseResponse(respF, st.respHeader)
	if err != nil {
		return trace.Trace{}, fmt.Errorf("parse resp: %w", err)
	}

	// disconnected: true if either drainer saw fewer bytes than expected
	// for a clean stream (i.e. truncation cap), OR if the response was
	// streaming and we never saw a clean terminator. For M2 we approximate:
	// disconnected ⇔ resp had Content-Type SSE and StreamDone == false.
	disconnected := false
	if respBody.StreamDone != nil && !*respBody.StreamDone {
		disconnected = true
	}

	return trace.Trace{
		ID:            traceID,
		TsStart:       st.tsStart,
		TsEnd:         st.tsEnd,
		Client:        st.clientAddr,
		Method:        st.method,
		Path:          st.path,
		Upstream:      upstreamURL,
		Status:        finalStatus,
		Req:           reqBody,
		Resp:          respBody,
		Disconnected:  disconnected,
		TruncatedReq:  st.reqResult.Truncated,
		TruncatedResp: st.respResult.Truncated,
	}, nil
}

// --- traceRegistry implements proxy.SinkLookup + proxy.MetaCapture ---

type traceRegistry struct {
	mu sync.RWMutex
	m  map[string]*traceState
}

func newTraceRegistry() *traceRegistry {
	return &traceRegistry{m: make(map[string]*traceState)}
}

func (r *traceRegistry) put(traceID string, st *traceState) {
	r.mu.Lock()
	r.m[traceID] = st
	r.mu.Unlock()
}

func (r *traceRegistry) remove(traceID string) {
	r.mu.Lock()
	delete(r.m, traceID)
	r.mu.Unlock()
}

// SinksFor implements proxy.SinkLookup.
func (r *traceRegistry) SinksFor(traceID string) (*capture.Sink, *capture.Sink) {
	r.mu.RLock()
	st := r.m[traceID]
	r.mu.RUnlock()
	if st == nil {
		return nil, nil
	}
	return st.reqSink, st.respSink
}

// OnReqHeaders implements proxy.MetaCapture.
func (r *traceRegistry) OnReqHeaders(traceID string, h http.Header) {
	r.mu.RLock()
	st := r.m[traceID]
	r.mu.RUnlock()
	if st == nil {
		return
	}
	st.reqHeaderMu.Lock()
	st.reqHeader = h
	st.reqHeaderMu.Unlock()
}

// OnRespMeta implements proxy.MetaCapture.
func (r *traceRegistry) OnRespMeta(traceID string, statusCode int, h http.Header) {
	r.mu.RLock()
	st := r.m[traceID]
	r.mu.RUnlock()
	if st == nil {
		return
	}
	st.reqHeaderMu.Lock()
	st.respHeader = h
	st.respStatus = statusCode
	st.reqHeaderMu.Unlock()
}

// --- capturingResponseWriter ---

type capturingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (c *capturingResponseWriter) WriteHeader(code int) {
	if c.status == -1 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *capturingResponseWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack is needed for any future WebSocket / Connection-Upgrade work.
// For v0 (no WebSocket support) we never expect this, but exposing it
// keeps capturingResponseWriter from blocking such handlers if they ever
// land on the proxy listener by mistake.
//
// Implementing it requires the underlying ResponseWriter to be a Hijacker.
// If not, the cast fails and the caller gets a clear error.
