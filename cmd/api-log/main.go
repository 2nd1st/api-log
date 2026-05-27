// Command api-log is the transparent recording proxy described in
// ../../README.md, ../../PHILOSOPHY.md, and ../../ARCHITECTURE.md.
//
// v0 M1 scope (this milestone): forwarding + body capture. No JSONL writer,
// no SQLite, no read API yet. M2+ adds those.
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
	"github.com/leoyun/api-log/internal/proxy"
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

	// Reg is the per-trace sink registry that the CaptureTransport will
	// consult on RoundTrip. Forwarding handler populates / removes entries.
	reg := newSinkRegistry()

	innerTransport := http.DefaultTransport.(*http.Transport).Clone()
	innerTransport.DisableCompression = true // ARCHITECTURE § 10.3

	captureTransport := &proxy.CaptureTransport{
		Inner: innerTransport,
		Sinks: reg,
	}
	rp := proxy.NewReverseProxy(upstreamURL, captureTransport)

	// Forwarding handler. M1: spin up sinks + drainers, forward, then
	// (placeholder) cleanup. M2 will add finalize parse + writer-channel send.
	proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := ids.NewTraceID()
		trace, err := startTrace(traceID, tmpDir, cfg.Storage.CaptureChanSize, cfg.Storage.MaxBodyBytes)
		if err != nil {
			slog.Error("startTrace failed", "trace_id", traceID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		reg.put(traceID, trace.reqSink, trace.respSink)
		defer reg.remove(traceID)

		ctx := proxy.WithTraceID(r.Context(), traceID)
		rp.ServeHTTP(w, r.WithContext(ctx))

		// M1 placeholder finalize: stop drainers, log results, delete
		// tmp files. M2 will route through writer goroutine instead.
		trace.finalize(cfg.Timeouts.DrainerJoin())
		tmpDir.RemoveTraceFiles(traceID)

		slog.Debug("trace finalized",
			"trace_id", traceID,
			"req_bytes", trace.reqResult.BytesWritten,
			"resp_bytes", trace.respResult.BytesWritten,
			"req_truncated", trace.reqResult.Truncated,
			"resp_truncated", trace.respResult.Truncated,
		)
	})

	srv := &http.Server{
		Addr:              cfg.Proxy.Listen,
		Handler:           proxyHandler,
		ReadHeaderTimeout: cfg.Timeouts.ReadHeader(),
		IdleTimeout:       cfg.Timeouts.Idle(),
		// WriteTimeout left zero: SSE responses can legitimately stream
		// for tens of minutes. See ARCHITECTURE § 10.7.
	}

	// Run + graceful shutdown.
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
	reqSink   *capture.Sink
	respSink  *capture.Sink
	reqDone   chan capture.DrainResult
	respDone  chan capture.DrainResult
	reqFile   *os.File
	respFile  *os.File

	once        sync.Once
	reqResult   capture.DrainResult
	respResult  capture.DrainResult
}

func startTrace(traceID string, tmpDir *capture.TmpDir, chanSize int, maxBodyBytes int64) (*traceState, error) {
	reqFile, respFile, err := tmpDir.CreateTraceFiles(traceID)
	if err != nil {
		return nil, err
	}

	reqCh := make(chan capture.Chunk, chanSize)
	respCh := make(chan capture.Chunk, chanSize)

	// onDrop callbacks: M1 just logs. M2 will flip per-trace
	// truncated_req / truncated_resp flags and bump counters.
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
	}, nil
}

// finalize closes the capture channels and waits for drainers, with an
// upper-bound wait of joinTimeout. Safe to call multiple times; only the
// first call does work.
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
		// Re-arm timer (Go pattern: avoid relying on the same C).
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
	})
}

// --- sinkRegistry implements proxy.SinkLookup ---

type sinkRegistry struct {
	mu sync.RWMutex
	m  map[string]registryEntry
}

type registryEntry struct {
	req, resp *capture.Sink
}

func newSinkRegistry() *sinkRegistry {
	return &sinkRegistry{m: make(map[string]registryEntry)}
}

func (r *sinkRegistry) put(traceID string, req, resp *capture.Sink) {
	r.mu.Lock()
	r.m[traceID] = registryEntry{req: req, resp: resp}
	r.mu.Unlock()
}

func (r *sinkRegistry) remove(traceID string) {
	r.mu.Lock()
	delete(r.m, traceID)
	r.mu.Unlock()
}

func (r *sinkRegistry) SinksFor(traceID string) (*capture.Sink, *capture.Sink) {
	r.mu.RLock()
	e := r.m[traceID]
	r.mu.RUnlock()
	return e.req, e.resp
}
