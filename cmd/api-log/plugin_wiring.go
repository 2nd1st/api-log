package main

// V2 hook plugin wiring (plugin-b-c-spec §2 / §3).
//
// This file holds the request/response plumbing for the BEFORE / AFTER
// hooks. main.go is the call-site; the helpers here keep the per-side
// machinery isolated:
//
//   - bufferRequestBody / applyBeforeHooks: BEFORE chain.
//   - modifyResponseFor:                      AFTER chain (non-streaming
//                                             + streaming branches).
//   - serveIntercept:                         common writer for BEFORE +
//                                             AFTER intercept responses.
//
// The marker the trace's plugin_intercepted field needs (spec §5.2) is
// threaded through the inbound request's context via an interceptSlot.
// buildTrace reads the slot after rp.ServeHTTP returns.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/leoyun/api-log/internal/api"
	pluginv2 "github.com/leoyun/api-log/internal/plugin/v2"
	"github.com/leoyun/api-log/internal/runtime"
	"github.com/leoyun/api-log/internal/sse"
	"github.com/leoyun/api-log/internal/trace"

	// Side-effect import: each builtin registers its Ctor at init() via
	// v2.RegisterBuiltin. Importing here is how main pulls them into the
	// process-wide builtin map without coupling main to each package's
	// internals.
	_ "github.com/leoyun/api-log/internal/plugin/v2/builtin/textappend"
	_ "github.com/leoyun/api-log/internal/plugin/v2/builtin/textreplace"
)

// interceptSlotKey carries an InterceptInfo from the BEFORE / AFTER
// hook into buildTrace's finalize block. Stored as a *pointer-to-*
// pointer so the closure can WRITE the slot post-hook and the read
// path sees the update.
type interceptSlotKey struct{}

type interceptSlot struct {
	info *trace.PluginInterceptMarker
}

func withInterceptSlot(ctx context.Context, slot *interceptSlot) context.Context {
	return context.WithValue(ctx, interceptSlotKey{}, slot)
}

func interceptSlotFromContext(ctx context.Context) *interceptSlot {
	v, _ := ctx.Value(interceptSlotKey{}).(*interceptSlot)
	return v
}

// parsedReqSlotKey carries the post-BEFORE-chain ParsedRequest from the
// inbound handler into ModifyResponse so the AFTER chain sees the same
// request the upstream actually got. Stored on the outbound request's
// context (which httputil.ReverseProxy clones onto resp.Request).
type parsedReqSlotKey struct{}

func withParsedRequest(ctx context.Context, req *pluginv2.ParsedRequest) context.Context {
	return context.WithValue(ctx, parsedReqSlotKey{}, req)
}

func parsedRequestFromContext(ctx context.Context) *pluginv2.ParsedRequest {
	v, _ := ctx.Value(parsedReqSlotKey{}).(*pluginv2.ParsedRequest)
	return v
}

// buildEffectivePluginInstances merges the YAML default list (currently
// empty — v0 config.go does not expose v2 instances under `plugins:`)
// with runtime_overrides.json's plugins block, per spec §3.3.2.
//
// The merge rules are the same atomic.Pointer semantics v2.Load already
// implements; we adapt the runtime layer's *bool Enabled and *map
// Config into v2.InstanceConfig's value types.
func buildEffectivePluginInstances(ov *runtime.PluginsOverride) []pluginv2.InstanceConfig {
	if ov == nil {
		return nil
	}
	// nil instances == "no override"; the merge function later treats
	// an empty list as "all plugins off." Mirror that here.
	out := make([]pluginv2.InstanceConfig, 0, len(ov.Instances))
	for _, inst := range ov.Instances {
		ic := pluginv2.InstanceConfig{
			Type:    inst.Type,
			ID:      inst.ID,
			Enabled: false,
			Config:  inst.Config,
		}
		if inst.Enabled != nil {
			ic.Enabled = *inst.Enabled
		}
		out = append(out, ic)
	}
	return out
}

// pluginTypeCatalogue exposes the in-process builtin registry to the
// read-API's GET /api/plugins/types. The list is computed once and
// snapshotted: builtin types are immutable after init().
func pluginTypeCatalogue() []api.PluginTypeDescriptor {
	// In v1 we do not expose ConfigSchema through the v2 catalogue
	// (each builtin owns its own schema type; W4 wires per-type schemas
	// when the viewer Settings UI lands). The descriptor here is a
	// minimum surface that lets the catalogue endpoint return non-empty
	// without growing the builtin contract.
	types := []api.PluginTypeDescriptor{
		{
			Type:         "text-replace",
			Description:  "Literal-substring replacement on request/response content.",
			ConfigSchema: api.PluginConfigSchema{},
		},
		{
			Type:         "text-append",
			Description:  "Append fixed text to request or response content.",
			ConfigSchema: api.PluginConfigSchema{},
		},
	}
	return types
}

// hasAnyV2Plugins reports whether at least one enabled instance is
// registered, on either hook side. The handler uses this to decide
// whether to buffer the request body + build a ParsedRequest — without
// either an enabled BEFORE plugin OR an enabled AFTER plugin, no hook
// has anything to observe and the proxy forwards r.Body as a streaming
// Reader exactly as before the plugin layer landed.
//
// (We pay the buffering cost when only AFTER plugins are enabled because
// AFTER needs the post-BEFORE-chain ParsedRequest to dispatch — and
// "post-BEFORE-chain" with zero BEFORE plugins is just the original
// parsed request. ModifyResponse looks the parsed request up via the
// outbound context; missing context means "no plugins ran", so the
// AFTER chain short-circuits.)
func hasAnyV2Plugins(reg *pluginv2.Registry) bool {
	if reg == nil {
		return false
	}
	for _, inst := range reg.Instances() {
		if !inst.Enabled {
			continue
		}
		if pluginv2.InstanceHasBefore(inst) || pluginv2.InstanceHasAfter(inst) {
			return true
		}
	}
	return false
}

// readAndResetBody fully reads r.Body and replaces it with an
// io.NopCloser around the buffered bytes so the downstream forwarder
// gets the same payload. Returns the buffered bytes for the BEFORE-
// chain parser.
//
// On read error returns nil; the caller falls back to "no plugins ran"
// and forwards the original (now possibly-EOF) body — capture still
// records what flowed.
func readAndResetBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		_ = r.Body.Close()
		return nil, fmt.Errorf("buffer request body: %w", err)
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.ContentLength = int64(len(buf))
	r.Header.Set("Content-Length", strconv.Itoa(len(buf)))
	return buf, nil
}

// applyBeforeHooks runs the BEFORE chain on parsed, then — if any
// plugin mutated the request — re-serializes the mutated body, swaps it
// back into r, and updates Content-Length.
//
// On ActionIntercept, returns (false, *InterceptInfo, nil). Caller
// serves the intercept response and skips upstream forwarding.
//
// On ActionMutate / ActionContinue, returns (true, nil, currentParsed).
// currentParsed is the post-chain ParsedRequest the AFTER hook will
// later see; the caller stashes it on the outbound context.
func applyBeforeHooks(
	ctx context.Context,
	reg *pluginv2.Registry,
	r *http.Request,
	parsed pluginv2.ParsedRequest,
) (continued bool, intercept *pluginv2.InterceptInfo, mutated *pluginv2.ParsedRequest) {
	finalReq, info := reg.IterateBefore(ctx, &parsed)
	if info != nil {
		return false, info, nil
	}
	// If the chain mutated the parsed view, rebuild the wire body so the
	// forwarder sends the new bytes. v2.BuildRequestBody overlays the
	// typed fields onto RawBody, so unknown root fields (temperature,
	// response_format, custom keys) survive.
	if finalReq != &parsed {
		newBody, err := pluginv2.BuildRequestBody(finalReq)
		if err != nil {
			slog.Warn("plugin-mutate: build request body failed; forwarding original",
				"err", err, "protocol", finalReq.Protocol)
		} else {
			r.Body = io.NopCloser(bytes.NewReader(newBody))
			r.ContentLength = int64(len(newBody))
			r.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
		}
	}
	return true, nil, finalReq
}

// serveIntercept writes an InterceptResponse to crw. Used by both the
// BEFORE-intercept path (called directly from the handler) and the
// non-streaming AFTER-intercept path (called from ModifyResponse).
func serveIntercept(crw http.ResponseWriter, info *pluginv2.InterceptInfo) {
	if info == nil || info.Response == nil {
		return
	}
	for k, vs := range info.Response.Headers {
		for _, v := range vs {
			crw.Header().Add(k, v)
		}
	}
	status := info.Response.Status
	if status == 0 {
		status = http.StatusOK
	}
	crw.WriteHeader(status)
	if len(info.Response.Body) > 0 {
		_, _ = crw.Write(info.Response.Body)
	}
}

// runAfterChainOnIntercept synthesizes a ParsedResponse from a
// BEFORE-intercept's payload and runs the AFTER chain on it (spec
// §2.2 / §2.4: "the FULL AFTER chain still runs on the synthesized
// response so watermark etc. still decorates it").
//
// The returned InterceptInfo is what should be written to the client.
// Semantics:
//
//   - AFTER plugin returns Continue or Mutate (text-append etc.): the
//     plugin's mutated ParsedResponse is re-serialized into the
//     original intercept's protocol shape and the intercept body is
//     replaced. Status / headers preserved from the source intercept
//     (the BEFORE plugin "owns" the wire envelope).
//
//   - AFTER plugin returns Intercept: the AFTER intercept fully
//     replaces the BEFORE intercept; the trace marker also updates so
//     buildTrace records the later (more specific) source.
//
// If the synthesized ParsedResponse cannot be built (no upstream
// protocol identified, body not JSON-shaped), the AFTER chain still
// runs but plugins see an empty Content / Reasoning view. Mutations
// to those fields silently no-op when BuildResponseBody can't
// re-serialize; the original intercept body is returned.
func runAfterChainOnIntercept(
	ctx context.Context,
	reg *pluginv2.Registry,
	req *pluginv2.ParsedRequest,
	src *pluginv2.InterceptInfo,
) *pluginv2.InterceptInfo {
	if src == nil || src.Response == nil {
		return src
	}
	if reg == nil || req == nil {
		return src
	}
	// Build the synthesized ParsedResponse. Use the request's protocol
	// (the intercept body usually mirrors what the upstream would have
	// returned) and the intercept's status / headers / body.
	status := src.Response.Status
	if status == 0 {
		status = http.StatusOK
	}
	parsedResp := pluginv2.ParsedResponseFromBody(req.Protocol, status, src.Response.Headers, src.Response.Body)
	ac := &pluginv2.AfterContext{Response: &parsedResp}
	afterInfo := reg.IterateAfter(ctx, req, ac)
	if afterInfo != nil {
		// AFTER plugin intercepted on top of the BEFORE intercept.
		// The AFTER intercept wins; later trace marker reflects it.
		return afterInfo
	}
	// No further intercept. If a plugin mutated the response, try to
	// re-serialize back into the protocol wire shape and replace the
	// body. Failure keeps the original intercept body — operators see
	// the source intercept verbatim, which is safer than serving a
	// broken body.
	if ac.Response != &parsedResp {
		out, err := pluginv2.BuildResponseBody(ac.Response)
		if err == nil && len(out) > 0 {
			hdrs := src.Response.Headers
			if hdrs == nil {
				hdrs = http.Header{}
			}
			return &pluginv2.InterceptInfo{
				Type: src.Type,
				ID:   src.ID,
				Hook: src.Hook,
				Response: &pluginv2.InterceptResponse{
					Status:  status,
					Headers: hdrs,
					Body:    out,
				},
			}
		}
		slog.Warn("after-on-intercept: build response body failed; serving original intercept",
			"err", err, "protocol", req.Protocol)
	}
	return src
}

// recordIntercept marks the inbound context's intercept slot so
// buildTrace can populate trace.PluginIntercepted. Safe to call with a
// nil slot (the request flowed without an intercept-slot ctx, e.g. in
// tests that bypass the handler).
func recordIntercept(ctx context.Context, info *pluginv2.InterceptInfo) {
	if info == nil {
		return
	}
	slot := interceptSlotFromContext(ctx)
	if slot == nil {
		return
	}
	slot.info = &trace.PluginInterceptMarker{
		Type: info.Type,
		ID:   info.ID,
		Hook: info.Hook,
	}
}

// makeModifyResponse builds the rp.ModifyResponse closure that runs the
// AFTER chain. Closure captures the registry + a reference to the per-
// process context (we read the inbound trace ctx via resp.Request).
//
// Two branches:
//
//   - Non-streaming: read resp.Body into memory, build a ParsedResponse,
//     run the AFTER chain, optionally serialize the mutated response
//     back into resp.Body. Intercept replaces resp entirely.
//
//   - Streaming (Content-Type text/event-stream): set up an io.Pipe.
//     The reader becomes the new resp.Body. A goroutine reads the
//     upstream body event-by-event via sse.Scanner, runs each event
//     through StreamDispatcher.Process, writes out the transformed
//     events via sse.WriteEvent. On EOF the goroutine runs
//     FlushBeforeFinish and writes the synthesized deltas, then closes
//     the pipe. The terminal event (message_stop / response.completed)
//     is emitted BEFORE the flush so spec §7.2's "land suffix before
//     message_stop" semantics… actually the inverse: we hold the
//     terminal event until after the flush so the suffix arrives first,
//     then the terminator. See the goroutine for details.
func makeModifyResponse(reg *pluginv2.Registry) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp == nil || reg == nil {
			return nil
		}
		ctx := resp.Request.Context()
		parsed := parsedRequestFromContext(ctx)
		if parsed == nil {
			// Inbound handler did not stash a parsed request — means the
			// BEFORE chain did not run (no plugins, or empty registry).
			// AFTER chain has nothing to do; pass through.
			return nil
		}

		if pluginv2.IsSSEContentType(resp.Header.Get("Content-Type")) {
			return modifyStreamingResponse(ctx, reg, parsed, resp)
		}
		return modifyBufferedResponse(ctx, reg, parsed, resp)
	}
}

// modifyBufferedResponse handles the non-streaming AFTER branch. Reads
// the upstream body into memory, builds ParsedResponse, runs the chain.
// On Mutate: re-marshal and replace resp.Body. On Intercept: replace
// resp.Body + status + headers with the intercept payload.
func modifyBufferedResponse(
	ctx context.Context,
	reg *pluginv2.Registry,
	req *pluginv2.ParsedRequest,
	resp *http.Response,
) error {
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		// Read failed; replay empty body downstream (capture already saw
		// what arrived). Caller's WriteHeader path is unaffected.
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		resp.ContentLength = 0
		return nil
	}
	parsedResp := pluginv2.ParsedResponseFromBody(req.Protocol, resp.StatusCode, resp.Header, body)
	ac := &pluginv2.AfterContext{Response: &parsedResp}
	info := reg.IterateAfter(ctx, req, ac)
	if info != nil {
		recordIntercept(ctx, info)
		newBody := info.Response.Body
		for k, vs := range info.Response.Headers {
			resp.Header[k] = append(resp.Header[k][:0], vs...)
		}
		if info.Response.Status != 0 {
			resp.StatusCode = info.Response.Status
		}
		resp.Body = io.NopCloser(bytes.NewReader(newBody))
		resp.ContentLength = int64(len(newBody))
		resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
		return nil
	}
	// Plugin chain may have mutated ac.Response. Build the wire body
	// only when it actually changed something — comparing pointers is
	// good enough; IterateAfter only re-assigns ac.Response on Mutate.
	if ac.Response != &parsedResp {
		out, berr := pluginv2.BuildResponseBody(ac.Response)
		if berr != nil {
			slog.Warn("plugin-mutate: build response body failed; serving original",
				"err", berr, "protocol", req.Protocol)
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			return nil
		}
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
		return nil
	}
	// No mutation, no intercept: hand the original bytes back.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

// modifyStreamingResponse handles the SSE AFTER branch. Wraps the
// upstream body in a goroutine that drives a StreamDispatcher; the
// returned resp.Body is an io.PipeReader that feeds the
// reverseproxy → client copy.
//
// The R1 amendment removed the "hold-the-terminal-event" choreography
// the R0 design used to land a synthesized footer before message_stop.
// With OnLastTextDelta the mutation happens on the buffered last
// content delta BEFORE the protocol's terminator flows through the
// dispatcher, so the terminator can pass through Process unmolested.
// The final EOF flush is still required to cover Chat (no terminator
// at all) and Gemini.
func modifyStreamingResponse(
	ctx context.Context,
	reg *pluginv2.Registry,
	req *pluginv2.ParsedRequest,
	resp *http.Response,
) error {
	dispatcher, info := reg.WrapStreamDispatcher(ctx, req)
	if info != nil {
		// Intercept fired during AFTER-plugin registration: drop the
		// upstream stream entirely, replace with the intercept payload.
		recordIntercept(ctx, info)
		_ = resp.Body.Close()
		for k, vs := range info.Response.Headers {
			resp.Header[k] = append(resp.Header[k][:0], vs...)
		}
		if info.Response.Status != 0 {
			resp.StatusCode = info.Response.Status
		}
		resp.Body = io.NopCloser(bytes.NewReader(info.Response.Body))
		resp.ContentLength = int64(len(info.Response.Body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(info.Response.Body)))
		// Drop streaming hints since the body is now a fixed-size byte
		// blob; keeping text/event-stream is fine but clients will
		// notice the abrupt close.
		resp.Header.Del("Transfer-Encoding")
		return nil
	}
	// Streaming Content-Length is unknown until close; remove it so
	// ReverseProxy's body-copy doesn't choke on a mismatch when we end
	// up writing more bytes than upstream advertised.
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1

	upstream := resp.Body
	pr, pw := io.Pipe()
	go func() {
		// On any panic in the transform goroutine, close the pipe with
		// an error so the downstream client write loop unblocks. The
		// capture tee on resp.Body already saw the upstream bytes; this
		// goroutine only affects what flows to the CLIENT.
		defer func() {
			if rec := recover(); rec != nil {
				slog.Warn("stream dispatcher panic", "panic", rec)
				_ = pw.CloseWithError(fmt.Errorf("stream dispatcher panic: %v", rec))
			}
		}()
		defer func() { _ = upstream.Close() }()

		scanner := sse.NewScanner(upstream)
		for {
			ev, err := scanner.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				slog.Warn("sse scanner error", "err", err)
				break
			}
			for _, out := range dispatcher.Process(ev) {
				if werr := sse.WriteEvent(pw, out); werr != nil {
					_ = pw.CloseWithError(werr)
					return
				}
			}
		}
		// EOF flush: covers Chat (no terminator), Gemini (no
		// terminator), and is defensive for Messages / Responses
		// streams that closed without their protocol terminator.
		for _, out := range dispatcher.FlushBeforeFinish() {
			if werr := sse.WriteEvent(pw, out); werr != nil {
				_ = pw.CloseWithError(werr)
				return
			}
		}
		_ = pw.Close()
	}()
	resp.Body = pr
	return nil
}
