// Concurrency / latency / write benchmark for api-log.
//
// Spawns -conc workers; each fires -count requests cycling through the
// configured protocols. Records per-request latency and prints
// percentile and throughput stats; emits a JSON summary on stdout for
// the orchestrator to ingest.
//
// Typical use:
//   bench -upstream http://localhost:7861 \
//         -key sk-... -conc 50 -count 20 \
//         -chat-model gpt-4o-mini \
//         -anthropic-model claude-haiku-4-5 \
//         -responses-model gpt-4o-mini
//
// The bench is upstream-agnostic: point -upstream at the api-log proxy
// (default) to measure end-to-end including the proxy, or at the
// upstream directly to measure the bare baseline. Two runs with the
// same -seed give the same request body sequence, so before/after
// comparisons are apples-to-apples.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type sample struct {
	Proto    string
	Stream   bool
	Status   int
	Bytes    int64
	Latency  time.Duration
	FirstTok time.Duration // for streaming: time-to-first-byte
	Err      string
}

type opts struct {
	Upstream        string
	Keys            []string
	AuthHeader      string
	Conc            int
	Count           int
	Protocols       []string
	ChatModel       string
	AnthropicModel  string
	ResponsesModel  string
	MaxTokens       int
	PerReqTimeout   time.Duration
	Seed            int64
	JSONOut         string
}

func main() {
	var (
		upstream       = flag.String("upstream", "http://localhost:7861", "api-log proxy URL (or upstream URL for baseline)")
		key            = flag.String("key", "", "Bearer token(s) to send. Comma-separated to round-robin across goroutines (each worker pins to one key for the run — gives realistic distinct key_hashes downstream).")
		authHdr        = flag.String("auth-header", "Authorization", "header name for the key (Authorization | x-api-key)")
		conc           = flag.Int("conc", 50, "concurrent clients")
		count          = flag.Int("count", 20, "requests per client")
		protocols      = flag.String("protocols", "chat-nostream,chat-stream,anthropic-stream,responses-stream", "comma-separated protocols")
		chatModel      = flag.String("chat-model", "gpt-4o-mini", "model for /v1/chat/completions")
		anthropicModel = flag.String("anthropic-model", "claude-haiku-4-5", "model for /v1/messages")
		responsesModel = flag.String("responses-model", "gpt-4o-mini", "model for /v1/responses")
		maxTokens      = flag.Int("max-tokens", 64, "max_tokens / cap on response length where applicable")
		perReqTimeout  = flag.Duration("timeout", 120*time.Second, "per-request timeout")
		seed           = flag.Int64("seed", 1, "PRNG seed for request body content (deterministic across runs)")
		jsonOut        = flag.String("json-out", "", "if set, write JSON summary to this file in addition to stdout")
	)
	flag.Parse()

	if *key == "" {
		fmt.Fprintln(os.Stderr, "bench: -key is required")
		os.Exit(2)
	}
	keys := strings.Split(*key, ",")
	for i := range keys {
		keys[i] = strings.TrimSpace(keys[i])
	}
	o := opts{
		Upstream:       strings.TrimRight(*upstream, "/"),
		Keys:           keys,
		AuthHeader:     *authHdr,
		Conc:           *conc,
		Count:          *count,
		Protocols:      strings.Split(*protocols, ","),
		ChatModel:      *chatModel,
		AnthropicModel: *anthropicModel,
		ResponsesModel: *responsesModel,
		MaxTokens:      *maxTokens,
		PerReqTimeout:  *perReqTimeout,
		Seed:           *seed,
		JSONOut:        *jsonOut,
	}

	total := o.Conc * o.Count
	samples := make([]sample, 0, total)
	mu := sync.Mutex{}
	var wg sync.WaitGroup
	var sent int64

	// One HTTP client per worker — keep-alive per goroutine, isolates
	// connection pool contention from latency measurement.
	makeClient := func() *http.Client {
		return &http.Client{
			Timeout: o.PerReqTimeout,
			Transport: &http.Transport{
				MaxIdleConns:          o.Conc,
				MaxIdleConnsPerHost:   o.Conc,
				IdleConnTimeout:       90 * time.Second,
				DisableCompression:    false,
				ResponseHeaderTimeout: o.PerReqTimeout,
			},
		}
	}

	start := time.Now()
	for w := 0; w < o.Conc; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			client := makeClient()
			rng := rand.New(rand.NewSource(o.Seed + int64(workerID)*1009))
			// Pin one key per worker for the full run — that way each
			// key produces its own distinct key_hash batch on the
			// recorder side, and downstream rate limiters see realistic
			// per-user distribution rather than a shuffled stream.
			key := o.Keys[workerID%len(o.Keys)]
			for i := 0; i < o.Count; i++ {
				proto := o.Protocols[(workerID+i)%len(o.Protocols)]
				s := runOne(client, &o, proto, workerID, i, rng, key)
				atomic.AddInt64(&sent, 1)
				mu.Lock()
				samples = append(samples, s)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)

	summary := summarize(samples, elapsed, total)
	printHuman(summary)
	out, _ := json.MarshalIndent(summary, "", "  ")
	if o.JSONOut != "" {
		_ = os.WriteFile(o.JSONOut, out, 0o644)
	}
}

func runOne(client *http.Client, o *opts, proto string, worker, iter int, rng *rand.Rand, key string) sample {
	s := sample{Proto: proto}
	method, path, body, stream := buildRequest(o, proto, worker, iter, rng)
	s.Stream = stream
	url := o.Upstream + path

	ctx, cancel := context.WithTimeout(context.Background(), o.PerReqTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		s.Err = "build: " + err.Error()
		return s
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(o.AuthHeader, "Bearer "+key)
	if proto == "anthropic-stream" {
		// Anthropic protocol expects bare key in x-api-key.
		req.Header.Del(o.AuthHeader)
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		s.Err = "do: " + err.Error()
		s.Latency = time.Since(t0)
		return s
	}
	defer resp.Body.Close()
	s.Status = resp.StatusCode

	// For streaming, time-to-first-byte: read one byte, record, then
	// drain the rest. For non-streaming we just read all.
	if stream {
		buf := make([]byte, 1)
		n, _ := io.ReadFull(resp.Body, buf)
		s.FirstTok = time.Since(t0)
		if n > 0 {
			s.Bytes++
		}
		copied, _ := io.Copy(io.Discard, resp.Body)
		s.Bytes += copied
	} else {
		copied, _ := io.Copy(io.Discard, resp.Body)
		s.Bytes = copied
	}
	s.Latency = time.Since(t0)
	return s
}

// buildRequest returns (method, path, body, isStream).
func buildRequest(o *opts, proto string, worker, iter int, rng *rand.Rand) (string, string, []byte, bool) {
	prompt := fmt.Sprintf("bench worker=%d iter=%d seed=%d say hi", worker, iter, rng.Intn(1_000_000))
	switch proto {
	case "chat-nostream":
		body, _ := json.Marshal(map[string]any{
			"model":    o.ChatModel,
			"messages": []any{map[string]string{"role": "user", "content": prompt}},
			"max_tokens": o.MaxTokens,
		})
		return "POST", "/v1/chat/completions", body, false
	case "chat-stream":
		body, _ := json.Marshal(map[string]any{
			"model":    o.ChatModel,
			"messages": []any{map[string]string{"role": "user", "content": prompt}},
			"max_tokens": o.MaxTokens,
			"stream":   true,
		})
		return "POST", "/v1/chat/completions", body, true
	case "anthropic-stream":
		body, _ := json.Marshal(map[string]any{
			"model":      o.AnthropicModel,
			"max_tokens": o.MaxTokens,
			"messages":   []any{map[string]string{"role": "user", "content": prompt}},
			"stream":     true,
		})
		return "POST", "/v1/messages", body, true
	case "responses-stream":
		body, _ := json.Marshal(map[string]any{
			"model":             o.ResponsesModel,
			"input":             prompt,
			"max_output_tokens": o.MaxTokens,
			"stream":            true,
		})
		return "POST", "/v1/responses", body, true
	default:
		// Unknown protocol → smallest valid Chat request, treated as a
		// hard error in the report so it surfaces.
		body, _ := json.Marshal(map[string]any{"model": o.ChatModel, "messages": []any{}})
		return "POST", "/v1/chat/completions", body, false
	}
}

type protoStat struct {
	Proto       string  `json:"proto"`
	Count       int     `json:"count"`
	Ok          int     `json:"ok"`
	HTTP4xx     int     `json:"http_4xx"`
	HTTP5xx     int     `json:"http_5xx"`
	Errors      int     `json:"errors"`
	P50Ms       float64 `json:"p50_ms"`
	P90Ms       float64 `json:"p90_ms"`
	P95Ms       float64 `json:"p95_ms"`
	P99Ms       float64 `json:"p99_ms"`
	MaxMs       float64 `json:"max_ms"`
	MeanMs      float64 `json:"mean_ms"`
	FirstTokP50 float64 `json:"first_tok_p50_ms,omitempty"`
	FirstTokP95 float64 `json:"first_tok_p95_ms,omitempty"`
	BytesTotal  int64   `json:"bytes_total"`
}

type summary struct {
	Total            int          `json:"total"`
	ElapsedSec       float64      `json:"elapsed_sec"`
	Throughput       float64      `json:"req_per_sec"`
	OverallP50Ms     float64      `json:"overall_p50_ms"`
	OverallP95Ms     float64      `json:"overall_p95_ms"`
	OverallP99Ms     float64      `json:"overall_p99_ms"`
	OverallMeanMs    float64      `json:"overall_mean_ms"`
	OkCount          int          `json:"ok_count"`
	HTTP4xxCount     int          `json:"http_4xx_count"`
	HTTP5xxCount     int          `json:"http_5xx_count"`
	ErrorCount       int          `json:"error_count"`
	BytesTotal       int64        `json:"bytes_total"`
	PerProtocol      []protoStat  `json:"per_protocol"`
	FirstFewErrors   []string     `json:"first_few_errors,omitempty"`
}

func summarize(samples []sample, elapsed time.Duration, total int) summary {
	s := summary{
		Total:      len(samples),
		ElapsedSec: elapsed.Seconds(),
		Throughput: float64(len(samples)) / elapsed.Seconds(),
	}

	byProto := map[string][]sample{}
	for _, x := range samples {
		byProto[x.Proto] = append(byProto[x.Proto], x)
	}

	all := make([]float64, 0, len(samples))
	var meanSum float64
	for _, x := range samples {
		ms := float64(x.Latency.Milliseconds())
		all = append(all, ms)
		meanSum += ms
		if x.Err != "" {
			s.ErrorCount++
			if len(s.FirstFewErrors) < 10 {
				s.FirstFewErrors = append(s.FirstFewErrors, fmt.Sprintf("[%s] %s", x.Proto, x.Err))
			}
		} else if x.Status >= 200 && x.Status < 300 {
			s.OkCount++
		} else if x.Status >= 400 && x.Status < 500 {
			s.HTTP4xxCount++
		} else if x.Status >= 500 {
			s.HTTP5xxCount++
		}
		s.BytesTotal += x.Bytes
	}
	if len(all) > 0 {
		sort.Float64s(all)
		s.OverallP50Ms = pct(all, 0.50)
		s.OverallP95Ms = pct(all, 0.95)
		s.OverallP99Ms = pct(all, 0.99)
		s.OverallMeanMs = meanSum / float64(len(all))
	}

	for proto, xs := range byProto {
		ps := protoStat{Proto: proto, Count: len(xs)}
		lats := make([]float64, 0, len(xs))
		fts := make([]float64, 0, len(xs))
		var sum float64
		for _, x := range xs {
			ms := float64(x.Latency.Milliseconds())
			lats = append(lats, ms)
			sum += ms
			if x.Err != "" {
				ps.Errors++
			} else if x.Status >= 200 && x.Status < 300 {
				ps.Ok++
			} else if x.Status >= 400 && x.Status < 500 {
				ps.HTTP4xx++
			} else if x.Status >= 500 {
				ps.HTTP5xx++
			}
			ps.BytesTotal += x.Bytes
			if x.FirstTok > 0 {
				fts = append(fts, float64(x.FirstTok.Milliseconds()))
			}
		}
		sort.Float64s(lats)
		if len(lats) > 0 {
			ps.P50Ms = pct(lats, 0.50)
			ps.P90Ms = pct(lats, 0.90)
			ps.P95Ms = pct(lats, 0.95)
			ps.P99Ms = pct(lats, 0.99)
			ps.MaxMs = lats[len(lats)-1]
			ps.MeanMs = sum / float64(len(lats))
		}
		if len(fts) > 0 {
			sort.Float64s(fts)
			ps.FirstTokP50 = pct(fts, 0.50)
			ps.FirstTokP95 = pct(fts, 0.95)
		}
		s.PerProtocol = append(s.PerProtocol, ps)
	}
	sort.Slice(s.PerProtocol, func(i, j int) bool {
		return s.PerProtocol[i].Proto < s.PerProtocol[j].Proto
	})
	return s
}

// pct returns the linearly-interpolated percentile (p in [0,1]) of a
// sorted slice. Returns 0 for empty.
func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := p * float64(len(sorted)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func printHuman(s summary) {
	fmt.Printf("\n=== bench summary ===\n")
	fmt.Printf("total: %d requests in %.2fs  (%.1f req/s)\n", s.Total, s.ElapsedSec, s.Throughput)
	fmt.Printf("ok:    %d  | 4xx: %d  | 5xx: %d  | err: %d\n",
		s.OkCount, s.HTTP4xxCount, s.HTTP5xxCount, s.ErrorCount)
	fmt.Printf("bytes: %d (%.2f MB)\n", s.BytesTotal, float64(s.BytesTotal)/1024/1024)
	fmt.Printf("overall latency (ms):  p50=%.0f  p95=%.0f  p99=%.0f  mean=%.0f\n",
		s.OverallP50Ms, s.OverallP95Ms, s.OverallP99Ms, s.OverallMeanMs)

	fmt.Printf("\nper-protocol:\n")
	fmt.Printf("  %-20s  %5s  %4s  %4s  %4s  %4s  %8s  %8s  %8s  %8s\n",
		"proto", "count", "ok", "4xx", "5xx", "err", "p50", "p95", "p99", "max")
	for _, p := range s.PerProtocol {
		fmt.Printf("  %-20s  %5d  %4d  %4d  %4d  %4d  %8.0f  %8.0f  %8.0f  %8.0f\n",
			p.Proto, p.Count, p.Ok, p.HTTP4xx, p.HTTP5xx, p.Errors,
			p.P50Ms, p.P95Ms, p.P99Ms, p.MaxMs)
		if p.FirstTokP50 > 0 {
			fmt.Printf("  %-20s    first-byte p50=%.0fms p95=%.0fms\n", "", p.FirstTokP50, p.FirstTokP95)
		}
	}
	if len(s.FirstFewErrors) > 0 {
		fmt.Printf("\nfirst errors:\n")
		for _, e := range s.FirstFewErrors {
			fmt.Printf("  - %s\n", e)
		}
	}
	fmt.Println()
}
