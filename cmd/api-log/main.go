// Command api-log is the transparent recording proxy described in
// ../../README.md and ../../ARCHITECTURE.md.
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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"log/slog"

	"path/filepath"

	"github.com/2nd1st/api-log/internal/admin"
	"github.com/2nd1st/api-log/internal/api"
	"github.com/2nd1st/api-log/internal/capture"
	"github.com/2nd1st/api-log/internal/config"
	"github.com/2nd1st/api-log/internal/counters"
	"github.com/2nd1st/api-log/internal/ids"
	"github.com/2nd1st/api-log/internal/logging"
	"github.com/2nd1st/api-log/internal/media"
	"github.com/2nd1st/api-log/internal/parser"
	"github.com/2nd1st/api-log/internal/plugin"
	"github.com/2nd1st/api-log/internal/plugin/builtin/pathfilter"
	pluginv2 "github.com/2nd1st/api-log/internal/plugin/v2"
	"github.com/2nd1st/api-log/internal/proxy"
	"github.com/2nd1st/api-log/internal/runtime"
	"github.com/2nd1st/api-log/internal/store/sqlite"
	"github.com/2nd1st/api-log/internal/trace"
	"github.com/2nd1st/api-log/internal/viewerhost"
	"github.com/2nd1st/api-log/internal/writer"
)

// version is the binary version string. Overridden at build time via
// `-ldflags "-X main.version=<tag>"` (see Dockerfile + release CI).
// The default keeps `go run` / unstamped builds identifiable.
var version = "0.0.0-dev"

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
		"version", version,
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

	// Phase K — media extraction + runtime toggle.
	mediaEnabled := &atomic.Bool{}
	mediaEnabled.Store(cfg.Media.SaveAttachments)
	runtimeOverrides, err := runtime.LoadOverrides(cfg.Storage.DataDir)
	if err != nil {
		slog.Warn("runtime overrides load failed", "err", err)
	}
	if runtimeOverrides.Media.SaveAttachments != nil {
		mediaEnabled.Store(*runtimeOverrides.Media.SaveAttachments)
	}
	mediaExt := media.New(media.Config{DataDir: cfg.Storage.DataDir})

	// Phase A observer-class registry. Constructed BEFORE the writer
	// goroutine starts so it is visible to the proxyHandler closure.
	// Operators control the enabled set via cfg.Plugins; an empty block
	// (zero value) registers nothing and the proxy behaves exactly as
	// before this commit.
	//
	// PHILOSOPHY §2 — capture never interferes: OnFinalize runs AFTER
	// the response has fully reached the client. It can drop a trace
	// from recording but cannot affect forwarding.
	pluginReg := plugin.NewRegistry()
	if patterns := cfg.Plugins.PathFilter.Patterns; len(patterns) > 0 {
		pf := pathfilter.New()
		if err := pluginReg.Register(pf); err != nil {
			return fmt.Errorf("register %s: %w", pathfilter.Name, err)
		}
		// pathfilter.Init takes the raw YAML-map shape ([]any of strings),
		// so convert the typed []string from config here. Doing the
		// conversion in main keeps the plugin's Init contract honest
		// (it can be called from any source that produces a map[string]any).
		raw := make([]any, len(patterns))
		for i, p := range patterns {
			raw[i] = p
		}
		if err := pluginReg.Init(map[string]map[string]any{
			pathfilter.Name: {"patterns": raw},
		}); err != nil {
			return fmt.Errorf("plugin init: %w", err)
		}
		// Operators MUST be able to see what's NOT being recorded at
		// startup — silently dropping traces would violate PHILOSOPHY §2.
		slog.Info("plugin enabled", "name", pf.Name(), "patterns", patterns)
	}

	// v2 hook-class registry — plugin-b-c-spec §2 / §3. Hot-path BEFORE
	// + AFTER plugins that can mutate or intercept requests/responses.
	// In v1 the YAML side has no instance-list yet (cfg.Plugins is the
	// Phase A mapping shape, not the §3.3.1 sequence); everything flows
	// through runtime_overrides.json's `plugins` block, which the read
	// API (api/plugins.go) and the viewer Settings UI (W4) own. An
	// override block with non-nil but empty Instances = "all plugins
	// off"; a nil block = "no v2 plugins configured."
	//
	// PHILOSOPHY §2 (amended): explicit operator-configured plugins MAY
	// interfere through BEFORE/AFTER hooks; the capture path itself
	// still never independently rewrites or routes.
	//
	// yamlV2Plugins is the pre-override YAML default list. Today the v0
	// config carries no v2 entries, so it stays nil; the API layer's
	// Reload feeds this as the YAML base on every hot-reload so override
	// merges don't double-apply.
	var yamlV2Plugins []pluginv2.InstanceConfig
	var v2InstanceConfigs []pluginv2.InstanceConfig
	if runtimeOverrides.Plugins != nil {
		v2InstanceConfigs = pluginv2.Load(yamlV2Plugins, &pluginv2.OverrideList{
			Instances: buildEffectivePluginInstances(runtimeOverrides.Plugins),
		})
	}
	pluginV2Reg, v2Errs := pluginv2.NewRegistry(v2InstanceConfigs)
	for _, e := range v2Errs {
		slog.Warn("v2 plugin init", "err", e)
	}
	// Process-start convention (spec §4.4): Init failures are fatal.
	// Runtime config edits via the API call Registry.Reload, which
	// applies all-or-nothing semantics and keeps the old snapshot live
	// on Init error — the API handler then rolls back the persisted
	// override file (W4.2).
	if len(v2Errs) > 0 {
		return fmt.Errorf("v2 plugin init failed: %d error(s)", len(v2Errs))
	}
	defer func() { _ = pluginV2Reg.Close() }()
	for _, inst := range pluginV2Reg.Instances() {
		slog.Info("v2 plugin enabled",
			"type", inst.Type, "id", inst.ID, "enabled", inst.Enabled)
	}

	// Single-writer goroutine for JSONL append + SQLite upsert. Both
	// run in the same goroutine in one transaction per trace.
	wrtr := writer.New(cfg.Storage.DataDir, cfg.Storage.WriterChanSize, store, ctrs, mediaExt, mediaEnabled, nil)
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
	// Hook the AFTER chain onto the reverse proxy. ModifyResponse runs
	// after the upstream response is in hand but before the body is
	// streamed to the client; the plumbing in plugin_wiring.go reads
	// the per-request ParsedRequest from the outbound context and
	// either replaces the body buffer (non-streaming) or wraps it in
	// an io.Pipe driven by a StreamDispatcher (streaming).
	rp.ModifyResponse = makeModifyResponse(pluginV2Reg)

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

		// v2 BEFORE chain — runs only when at least one hook plugin
		// (BEFORE or AFTER) is registered. Empty registry means the
		// proxy behaves byte-for-byte as before this commit: no body
		// buffering, no header rewriting, no ModifyResponse overhead.
		// When only AFTER plugins are enabled we still buffer the
		// request so ModifyResponse can build its parsed view from
		// the same shape BEFORE plugins would have seen.
		slot := &interceptSlot{}
		ctx = withInterceptSlot(ctx, slot)
		var postChainReq *pluginv2.ParsedRequest
		if hasAnyV2Plugins(pluginV2Reg) {
			body, berr := readAndResetBody(r)
			if berr != nil {
				slog.Warn("buffer request body for plugins failed; skipping plugin chain",
					"trace_id", traceID, "err", berr)
			} else {
				parsedIn := pluginv2.ParsedRequestFromHTTPRequest(r, body)
				parsedIn.ClientIP = state.clientAddr
				continued, intercept, mutated := applyBeforeHooks(ctx, pluginV2Reg, r, parsedIn)
				if !continued {
					// BEFORE intercept short-circuits forwarding. Per
					// spec §2.2 / §2.4 the AFTER chain MUST still run
					// on the synthesized intercept response so
					// decorators (watermark / text-append /
					// text-replace AFTER halves) get to decorate the
					// short-circuited reply. The plugin-intercepted
					// marker tracks the ORIGINATING intercept; an
					// AFTER plugin that re-intercepts on top moves
					// the marker to itself (later intercept wins).
					recordIntercept(ctx, intercept)
					finalIntercept := runAfterChainOnIntercept(ctx, pluginV2Reg, &parsedIn, intercept)
					if finalIntercept != intercept {
						recordIntercept(ctx, finalIntercept)
					}
					serveIntercept(crw, finalIntercept)
					goto finalize
				}
				postChainReq = mutated
			}
		}
		if postChainReq != nil {
			ctx = withParsedRequest(ctx, postChainReq)
		}

		rp.ServeHTTP(crw, r.WithContext(ctx))

	finalize:

		// FINALIZE — close capture channels, join drainers (drainers own
		// their tmp files; receiving on the done channels guarantees Close
		// happens-before buildTrace's os.Open below).
		state.watchdog.Stop()
		drainStart := time.Now()
		state.finalize()
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

		// Observer-class OnFinalize hooks — runs AFTER the response
		// fully reached the client (rp.ServeHTTP returned) but BEFORE
		// the writer goroutine sees the trace. A plugin returning
		// shouldRecord=false drops the trace from JSONL + SQLite; the
		// upstream forward is untouched. Per PHILOSOPHY §3 ("fail open
		// on capture"), plugin errors are logged but do not by
		// themselves drop the trace — the plugin must explicitly say
		// "drop." We use context.Background() rather than the inbound
		// ctx: that ctx may have already been cancelled by the
		// stream-idle watchdog, and the recording decision is
		// decoupled from the forward lifecycle.
		shouldRecord, errs := pluginReg.IterateOnFinalize(context.Background(), tr)
		for _, e := range errs {
			slog.Warn("plugin OnFinalize error",
				"trace_id", traceID, "err", e)
		}
		if !shouldRecord {
			// Trace dropped from recording by an operator-configured
			// plugin. The client already has its response; we just
			// don't persist this one.
			tmpDir.RemoveTraceFiles(traceID)
			return
		}

		// Plugin-intercept marker (spec §5.2): an intercepted trace
		// gets a top-level plugin_intercepted field so operators can
		// distinguish "plugin returned 403" from "upstream returned
		// 403." The slot is written by the BEFORE/AFTER chain inside
		// applyBeforeHooks / makeModifyResponse.
		if slot != nil && slot.info != nil {
			tr.PluginIntercepted = slot.info
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

	// rootCtx is created here (rather than after NewMux) so the
	// hosted-viewer host below can take a ctx whose lifetime tracks
	// the process. viewerhost.New uses ctx for its one-shot startup
	// fetch + cache populate; cancelling it on shutdown stops any
	// in-flight HTTP GET to GitHub.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Hosted viewer (optional; default on, pinned to viewerVersion +
	// viewerSha256 in cmd/api-log/viewer_pins.go). The viewerhost
	// Host fetches `dist.zip` from the configured GitHub release at
	// startup, sha-verifies against the pinned constant, extracts to
	// the cache dir, and serves `/viewer/*`. Failures (sha mismatch,
	// fetch error, operator-override without matching sha) leave the
	// binary up; the route returns 503 and Info().Source="error" +
	// Info().Error surface the failure on /healthz.
	viewerCfg := viewerhost.Config{
		Enabled:    cfg.Viewer.Enabled,
		Repo:       coalesce(cfg.Viewer.Repo, viewerRepo),
		Version:    coalesce(cfg.Viewer.Version, viewerVersion),
		Sha256:     coalesce(cfg.Viewer.Sha256, viewerSha256),
		LocalPath:  cfg.Viewer.LocalPath,
		CacheDir:   coalesce(cfg.Viewer.CacheDir, filepath.Join(cfg.Storage.DataDir, "viewer-cache")),
		PublicPath: coalesce(cfg.Viewer.PublicPath, "/viewer"),
	}
	viewerHost := viewerhost.New(rootCtx, viewerCfg)

	// API server (port from cfg.API.Listen). Same process; separate
	// listener so a slow read API can't impact proxy traffic.
	apiHandler := api.NewMux(api.Deps{
		Store:        store,
		Counters:     ctrs,
		AdminToken:   adminToken,
		Version:      version,
		StartedAt:    time.Now().UTC(),
		DataDir:      cfg.Storage.DataDir,
		MediaEnabled: mediaEnabled,
		PluginTypes:  pluginTypeCatalogue,
		// W4.2 hot-reload: pass the SAME *Registry main captured for
		// the proxy + makeModifyResponse closures. Reload mutates the
		// snapshot pointer inside the struct, so all of those callers
		// pick up the new instance list on their next call.
		PluginV2Reg:      pluginV2Reg,
		YAMLPlugins:      yamlV2Plugins,
		ViewerHost:       viewerHost,
		ViewerPublicPath: viewerCfg.PublicPath,
	})
	apiSrv := &http.Server{
		Addr:              cfg.API.Listen,
		Handler:           apiHandler,
		ReadHeaderTimeout: cfg.Timeouts.ReadHeader(),
		IdleTimeout:       cfg.Timeouts.Idle(),
	}

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
	// Plugin Close runs AFTER the writer has drained, per the
	// plugin.Plugin.Close contract — by this point no BeforeRecord
	// invocation can still be in flight. Errors are logged but do not
	// block shutdown (best-effort across all plugins).
	if err := pluginReg.Close(); err != nil {
		slog.Warn("plugin close", "err", err)
	}
	_ = store.Close()
	slog.Info("api-log stopped")
	return nil
}

// --- per-trace state ---

type traceState struct {
	// Capture-side. The tmp *os.File handles are intentionally NOT held
	// here: each drainer goroutine owns its file for the file's full
	// lifetime and closes it before publishing the DrainResult. finalize
	// reads from reqDone/respDone, which sequences-after the close, so
	// buildTrace can safely os.Open(reqPath/respPath) on completion.
	reqSink  *capture.Sink
	respSink *capture.Sink
	reqDone  chan capture.DrainResult
	respDone chan capture.DrainResult
	reqPath  string
	respPath string

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
	// Drainer goroutines own their tmp file's lifetime: Close runs AFTER
	// Drain returns and BEFORE the result is sent. Receiving from reqDone /
	// respDone is therefore the synchronization point that guarantees the
	// file is closed and all writes are flushed before any reader (finalize
	// + buildTrace) touches the path. Closing in finalize while the drainer
	// was still writing would produce torn JSONL bytes; see v0.1.0 review
	// finding #4.
	go func() {
		res := capture.Drain(reqCh, reqFile, maxBodyBytes)
		_ = reqFile.Close()
		reqDone <- res
	}()
	go func() {
		res := capture.Drain(respCh, respFile, maxBodyBytes)
		_ = respFile.Close()
		respDone <- res
	}()

	return &traceState{
		reqSink:  reqSink,
		respSink: respSink,
		reqDone:  reqDone,
		respDone: respDone,
		reqPath:  reqFile.Name(),
		respPath: respFile.Name(),
	}, nil
}

// finalize closes the capture channels and waits unconditionally for both
// drainers to finish. Safe to call multiple times.
//
// Receiving from reqDone / respDone is the synchronization point that
// guarantees each drainer has returned from its loop AND closed its tmp
// file (the close runs inside the goroutine before the result is sent).
// buildTrace's subsequent os.Open therefore sees a complete, no-longer-
// being-written file.
//
// The previous implementation bounded the wait with a join timeout and
// abandoned the file on timeout, but that raced the drainer's still-live
// writes against finalize's Close and buildTrace's open — see v0.1.0
// review finding #4. Drainers cannot block indefinitely: producers send
// non-blockingly via Sink.Write, and once the channel is closed the
// `for range` loop terminates after consuming pending chunks.
func (t *traceState) finalize() {
	t.once.Do(func() {
		close(t.reqSink.Ch)
		close(t.respSink.Ch)

		t.reqResult = <-t.reqDone
		t.respResult = <-t.respDone

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
	defer func() { _ = reqF.Close() }()
	reqBody, err := parser.ParseRequest(reqF, st.reqHeader)
	if err != nil {
		return trace.Trace{}, fmt.Errorf("parse req: %w", err)
	}

	respF, err := os.Open(st.respPath)
	if err != nil {
		return trace.Trace{}, fmt.Errorf("open resp tmp: %w", err)
	}
	defer func() { _ = respF.Close() }()
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

// clientAddrOf resolves the real-client IP from inbound HTTP, walking
// common reverse-proxy header chains and skipping private / loopback
// hops so that a Caddy / nginx / docker-proxy-on-loopback topology
// doesn't yield a useless intermediate-hop address.
//
// Priority order (each header's value is filtered through
// isPrivateOrLoopback):
//  1. X-Forwarded-For (RFC 7239) — left-to-right; first non-private wins
//  2. X-Real-IP (nginx, Caddy single-hop convention)
//  3. Cf-Connecting-Ip (Cloudflare — IPv4 OR IPv6)
//  4. True-Client-Ip (Akamai / Cloudflare Enterprise)
//  5. r.RemoteAddr (the socket peer, verbatim including port)
//
// IPs are validated via net.ParseIP. Unparseable hops are treated as
// "skip and continue" — never returned. The final RemoteAddr fallback
// is returned verbatim because it's the only place we couldn't ground-
// truth a real IP and want operators to see "the socket said X" even
// when X is loopback.
//
// Zero-config bar (open-source-first): this must produce a useful
// client IP under direct / Caddy / nginx / Cloudflare / docker /
// incus-loopback topologies WITHOUT operator-specific config knobs.
// The header is still a named field on the wire — we read it as-is,
// no synthesis (PHILOSOPHY §1).
func clientAddrOf(r *http.Request) string {
	// 1. XFF chain: leftmost first; skip private/loopback hops.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, hop := range strings.Split(xff, ",") {
			ip := strings.TrimSpace(hop)
			if ip == "" {
				continue
			}
			if !isPrivateOrLoopback(ip) {
				return ip
			}
		}
	}
	// 2. Cf-Connecting-Ip — Cloudflare always sets this to the real
	//    client IP at the edge and rejects client-side spoofing. Higher
	//    priority than X-Real-IP because middle proxies behind the CDN
	//    (Caddy / nginx) overwrite X-Real-IP with their own source view,
	//    which is the CDN POP edge IP — public but NOT the real client.
	//    Empirically observed 2026-05-30: external probe through Caddy
	//    surfaced X-Real-IP=172.70.47.73 (Cloudflare AMS edge) while
	//    Cf-Connecting-Ip carried the real user IPv6.
	if cf := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); cf != "" {
		return cf
	}
	// 3. True-Client-Ip — Akamai + Cloudflare Enterprise convention.
	//    Same edge-set semantics; trust verbatim.
	if tc := strings.TrimSpace(r.Header.Get("True-Client-Ip")); tc != "" {
		return tc
	}
	// 4. X-Real-IP — single-value header set by single-hop reverse
	//    proxies (nginx, Caddy). Lowest among the edge-headers because
	//    middle proxies may rewrite it; skip if loopback / RFC1918
	//    (homelab loopback / docker-proxy topologies poison it).
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" && !isPrivateOrLoopback(xr) {
		return xr
	}
	return r.RemoteAddr
}

// isPrivateOrLoopback returns true when s parses as an IP that lives
// in a non-publicly-routable range. Unparseable inputs return true so
// the caller skips them (XFF chains can carry garbage from misbehaving
// upstream proxies; we don't want garbage as the recorded client).
//
// Skipped ranges:
//   - 127.0.0.0/8 + ::1/128             (loopback)
//   - 10.0.0.0/8, 172.16.0.0/12,
//     192.168.0.0/16                    (RFC 1918)
//   - fc00::/7                          (RFC 4193 unique local)
//   - 169.254.0.0/16 + fe80::/10        (link-local)
//   - 100.64.0.0/10                     (RFC 6598 carrier-grade NAT —
//     also treated as private because it's never a real client IP at
//     the edge of a CDN)
func isPrivateOrLoopback(s string) bool {
	// Strip an optional :port suffix on IPv4-with-port; IPv6 needs
	// bracket-stripping. Best-effort; net.ParseIP rejects port-suffixed
	// strings.
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s, "::") {
		// IPv4 with port "a.b.c.d:port" — but only when there is exactly
		// one ":" (i.e. not a bare IPv6 with multiple colons). Bracketed
		// IPv6 forms are handled below.
		if strings.Count(s, ":") == 1 {
			s = s[:i]
		}
	}
	s = strings.TrimPrefix(s, "[")
	if i := strings.LastIndex(s, "]"); i > 0 {
		s = s[:i]
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		isCgNat(ip)
}

// isCgNat reports whether the IPv4 ip lies in 100.64.0.0/10 (RFC 6598
// carrier-grade NAT). net.IP.IsPrivate does not cover this; we treat
// it as "skip" because seeing it from XFF means the chain is still
// inside a carrier's NAT, not at the original client.
func isCgNat(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
}

// coalesce returns s if non-empty, else fallback. Used to layer the
// backend-source-pinned viewer constants (viewerRepo / viewerVersion
// / viewerSha256 in viewer_pins.go) underneath the operator's
// config-file + env overrides: empty config field = use the pin,
// non-empty = operator wins.
func coalesce(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
