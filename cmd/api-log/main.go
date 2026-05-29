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
	"strings"
	"sync"
	"syscall"
	"time"

	"log/slog"

	"path/filepath"

	"github.com/leoyun/api-log/internal/admin"
	"github.com/leoyun/api-log/internal/api"
	"github.com/leoyun/api-log/internal/capture"
	"github.com/leoyun/api-log/internal/config"
	"github.com/leoyun/api-log/internal/counters"
	"github.com/leoyun/api-log/internal/ids"
	"github.com/leoyun/api-log/internal/logging"
	"github.com/leoyun/api-log/internal/parser"
	"github.com/leoyun/api-log/internal/proxy"
	"github.com/leoyun/api-log/internal/store/sqlite"
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

	// Open the SQLite derived cache. Per ARCHITECTURE § 4: WAL mode +
	// pragmas applied inside Open. Single write connection — the writer
	// goroutine below is the sole writer.
	sqlitePath := filepath.Join(cfg.Storage.DataDir, "index.sqlite")
	store, err := sqlite.Open(sqlitePath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	// Store closed in graceful shutdown sequence (after writer drains).
	slog.Info("sqlite open", "path", sqlitePath)

	// Admin bearer token for the read API.
	adminToken, generated, err := admin.EnsureToken(cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("admin token: %w", err)
	}
	if generated {
		fmt.Fprintf(os.Stdout, "admin_token: %s\n", adminToken)
	}

	// Shared atomic counters surfaced on /healthz.
	ctrs := counters.New()

	// Single-writer goroutine for JSONL append + SQLite upsert. Both
	// run in the same goroutine in one transaction per trace.
	wrtr := writer.New(cfg.Storage.DataDir, cfg.Storage.WriterChanSize, store, ctrs, nil)
	stopWriter := wrtr.Start()
	// NOTE: stopWriter is NOT deferred here — graceful shutdown calls it
	// in the right order (after proxy + API listeners are drained).

	// Per-trace registry: holds sinks for the CaptureTransport and the
	// captured req/resp metadata so finalize can build the JSONL line.
	reg := newTraceRegistry()

	innerTransport := http.DefaultTransport.(*http.Transport).Clone()
	innerTransport.DisableCompression = true // ARCHITECTURE § 10.3

	captureTransport := &proxy.CaptureTransport{
		Inner:       innerTransport,
		Sinks:       reg,
		Meta:        reg,
		OnDialError: ctrs.IncUpstreamDialErr,
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
		state.clientAddr = clientAddrOf(r)
		state.method = r.Method
		state.path = r.URL.RequestURI()

		// Stream-idle watchdog: cancel ctx if no resp byte arrives for
		// stream_idle_seconds. Triggers ServeHTTP unblock → finalize via
		// ctx-cancel path per ARCHITECTURE § 10.7.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		state.cancel = cancel
		state.watchdog = proxy.NewStreamWatchdog(cancel, cfg.Timeouts.StreamIdle())
		state.respSink.OnByte = state.watchdog.Pulse

		// Register sinks AFTER OnByte is set so the proxy's RoundTrip
		// (which calls SinksFor and then Write) sees the watchdog hook.
		reg.put(traceID, state)
		defer reg.remove(traceID)

		// Wrap ResponseWriter so we can record status code AS WRITTEN to
		// the client. ReverseProxy passes upstream's status through to
		// WriteHeader; if upstream errored before headers, ReverseProxy.
		// ErrorHandler writes a 502 instead — we capture either.
		crw := &capturingResponseWriter{ResponseWriter: w, status: -1}

		ctx = proxy.WithTraceID(ctx, traceID)
		rp.ServeHTTP(crw, r.WithContext(ctx))

		// FINALIZE — close capture channels, join drainers (bounded).
		state.watchdog.Stop()
		drainStart := time.Now()
		state.finalize(cfg.Timeouts.DrainerJoin())
		ctrs.DrainHist.Observe(time.Since(drainStart).Milliseconds())
		watchdogFired := state.watchdog.Fired()

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

		parseStart := time.Now()
		tr, err := buildTrace(traceID, state, cfg.Proxy.Upstream, finalStatus)
		ctrs.ParseHist.Observe(time.Since(parseStart).Milliseconds())
		if err != nil {
			slog.Error("buildTrace failed", "trace_id", traceID, "err", err)
			tmpDir.RemoveTraceFiles(traceID)
			return
		}
		// Stream-idle watchdog firing means the response stream went
		// silent past stream_idle_seconds. Mark the trace as
		// disconnected so consumers see why.
		if watchdogFired {
			tr.Disconnected = true
		}
		// Bump truncated counters per trace direction. Capture-side
		// flag set is the union of channel-overflow + body-cap drops.
		if tr.TruncatedReq {
			ctrs.IncTruncatedReq()
		}
		if tr.TruncatedResp {
			ctrs.IncTruncatedResp()
		}

		// Slow-trace surfaced as WARN so operators can grep for it.
		// Tail-of-distribution traces are usually upstream pathology
		// (long streaming completions, model thinking time, retries),
		// but having a single log line per occurrence with trace_id +
		// path + status makes them debuggable.
		if slowThreshold := cfg.Timeouts.SlowTrace(); slowThreshold > 0 {
			if dur := tr.TsEnd.Sub(tr.TsStart); dur >= slowThreshold {
				ctrs.IncSlowTrace()
				slog.Warn("slow trace",
					"trace_id", traceID,
					"path", tr.Path,
					"status", finalStatus,
					"duration_ms", dur.Milliseconds(),
					"threshold_ms", slowThreshold.Milliseconds(),
				)
			}
		}

		keyHash := ids.KeyHashFromHeaders(state.reqHeader)
		if !wrtr.TrySend(writer.Record{Trace: tr, KeyHash: keyHash}) {
			slog.Warn("writer channel full; trace metadata dropped",
				"trace_id", traceID)
		}
		tmpDir.RemoveTraceFiles(traceID)
	})

	proxySrv := &http.Server{
		Addr:              cfg.Proxy.Listen,
		Handler:           proxyHandler,
		ReadHeaderTimeout: cfg.Timeouts.ReadHeader(),
		IdleTimeout:       cfg.Timeouts.Idle(),
		// WriteTimeout left zero: SSE responses can stream tens of
		// minutes. M6 adds stream-idle watchdog.
	}

	// API server (port from cfg.API.Listen). Same process; separate
	// listener so a slow read API can't impact proxy traffic.
	apiHandler := api.NewMux(api.Deps{
		Store:      store,
		Counters:   ctrs,
		AdminToken: adminToken,
		Version:    "0.0.0-dev",
		StartedAt:  time.Now().UTC(),
	})
	apiSrv := &http.Server{
		Addr:              cfg.API.Listen,
		Handler:           apiHandler,
		ReadHeaderTimeout: cfg.Timeouts.ReadHeader(),
		IdleTimeout:       cfg.Timeouts.Idle(),
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Periodic counter snapshot — one INFO line per interval so prod
	// incidents have a per-minute history of appended / drops / writer
	// pressure to grep against, without anyone having to scrape
	// /healthz on a schedule. 0 disables.
	if interval := cfg.Diagnostics.SnapshotInterval(); interval > 0 {
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-rootCtx.Done():
					return
				case <-t.C:
					slog.Info("counter snapshot", "counters", ctrs.Snapshot())
				}
			}
		}()
	}

	proxyErr := make(chan error, 1)
	apiErr := make(chan error, 1)
	go func() {
		slog.Info("proxy listener up", "addr", cfg.Proxy.Listen)
		if err := proxySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			proxyErr <- err
		}
		close(proxyErr)
	}()
	go func() {
		slog.Info("api listener up", "addr", cfg.API.Listen)
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			apiErr <- err
		}
		close(apiErr)
	}()

	select {
	case <-rootCtx.Done():
		slog.Info("shutdown signal received, draining")
	case err := <-proxyErr:
		if err != nil {
			return fmt.Errorf("proxy server: %w", err)
		}
	case err := <-apiErr:
		if err != nil {
			return fmt.Errorf("api server: %w", err)
		}
	}

	// Graceful shutdown sequence per ARCHITECTURE § 7.5. Explicit
	// ordering (no defer LIFO games):
	//
	//   1. Stop accepting new proxy connections (Shutdown waits for
	//      in-flight forwarding handlers to return). The handlers
	//      themselves call wrtr.TrySend non-blockingly, so they don't
	//      depend on the writer goroutine still running — they finalize
	//      their tmp files, dispatch to the writer chan, and exit. If
	//      the chan is full, the metadata drops (logged + counter).
	//   2. Same for API listener.
	//   3. Close writer channel and wait for the writer goroutine to
	//      drain remaining records + complete in-flight gzip workers.
	//   4. Close the SQLite store. Doing this AFTER stopWriter
	//      guarantees the writer never sees a closed *sql.DB during
	//      its final flush.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Grace())
	defer cancel()
	if err := proxySrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("proxy graceful shutdown timed out", "err", err)
	}
	if err := apiSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("api graceful shutdown timed out", "err", err)
	}
	stopWriter() // drains writer chan + background gzip workers
	_ = store.Close()
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
	cancel     context.CancelFunc
	watchdog   *proxy.StreamWatchdog

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
// joinTimeout). Safe to call multiple times. If either drainer join
// times out, the corresponding direction is marked Truncated so the
// JSONL line records the loss per ARCHITECTURE § 7.1 step 7.
func (t *traceState) finalize(joinTimeout time.Duration) {
	t.once.Do(func() {
		close(t.reqSink.Ch)
		close(t.respSink.Ch)

		t.reqResult = drainWithTimeout(t.reqDone, joinTimeout, "req")
		t.respResult = drainWithTimeout(t.respDone, joinTimeout, "resp")

		_ = t.reqFile.Close()
		_ = t.respFile.Close()
		t.tsEnd = time.Now().UTC()
	})
}

// drainWithTimeout waits for one drainer's result, bounded. On timeout
// returns a DrainResult with Truncated=true so the JSONL line records
// the loss (and the truncated_*_total counter increments).
func drainWithTimeout(ch chan capture.DrainResult, timeout time.Duration, label string) capture.DrainResult {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r
	case <-timer.C:
		slog.Warn("drainer join timeout", "side", label, "timeout", timeout)
		return capture.DrainResult{Truncated: true}
	}
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
	respBody, err := parser.ParseResponse(respF, st.respHeader, parser.ParseOpts{
		ChunkTimings: st.respResult.ChunkTimings,
		TsStart:      st.tsStart,
	})
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

// clientAddrOf returns the recorded "client" string for an inbound
// request. When the request arrives via a trusted reverse proxy
// (Caddy in the homelab deployment), r.RemoteAddr is the proxy's own
// loopback address; the real client lives in X-Forwarded-For or
// X-Real-IP. We prefer those when present so the captured trace
// reflects the actual originator. The header is a named field on the
// wire — we extract its leftmost value, no synthesis (PHILOSOPHY §1).
//
// Format: "<ip>" when a forwarded header is present (port is unknown
// past the proxy), else r.RemoteAddr unchanged ("<ip>:<port>" form).
// XFF values can be comma-separated proxy chains; we take the leftmost
// hop, which is the original client by RFC 7239 convention.
func clientAddrOf(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Leftmost = original client. Strip whitespace.
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if v := strings.TrimSpace(xff); v != "" {
			return v
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	return r.RemoteAddr
}
