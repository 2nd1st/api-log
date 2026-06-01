// Package counters exposes the cumulative drop / overflow counters
// surfaced on /healthz per ARCHITECTURE § 6.5.
//
// Counters are process-wide and atomic. The writer goroutine, the
// trace finalize path, and any future drop site all bump them; the
// /healthz handler reads them.
package counters

import "sync/atomic"

// Counters is the shared atomic counter set.
type Counters struct {
	dropWriterFull      atomic.Int64
	dropJSONLFail       atomic.Int64
	dropSQLiteFail      atomic.Int64
	truncatedReqTotal   atomic.Int64
	truncatedRespTotal  atomic.Int64
	writerChanHighWater atomic.Int64
	appended            atomic.Int64
	indexed             atomic.Int64

	// Status-bucketed appends. Lets operators see "is upstream
	// returning a wall of 5xx" without grepping JSONL by hand.
	appended2xx atomic.Int64
	appended4xx atomic.Int64
	appended5xx atomic.Int64

	// Transport-layer errors (DNS / TLS / connection refused). Distinct
	// from HTTP 5xx so "can't reach upstream" and "upstream returned an
	// error response" don't get confused on /healthz.
	upstreamDialErr atomic.Int64

	// End-to-end traces that exceeded the slow-trace threshold.
	slowTraces atomic.Int64

	// Cumulative on-disk resource total. Bumped at JSONL append time
	// (writer side) so /healthz can answer "how much have we recorded so
	// far" without a fs walk. Token totals would belong here too, but the
	// writer doesn't extract usage from response bodies yet — deferred.
	totalBytes atomic.Int64

	// Cumulative count of extracted media files (Phase K). Bumped at trace
	// finalize time, after JSONL is on disk, when media extraction is on.
	// Lets /healthz answer "are we actually pulling out images" without
	// walking the media tree.
	totalMediaFiles atomic.Int64

	// Cumulative token totals. Bumped at JSONL append time from the usage block
	// extracted by parser.ExtractUsage. These are deterministic copies of named
	// protocol fields; no synthesized usage is added. They are a derived cache:
	// rebuildable from JSONL by replaying the same extractor. n == 0 calls are
	// a no-op so callers can blindly add whatever the extractor returned without
	// branching.
	totalPromptTokens        atomic.Int64
	totalCompletionTokens    atomic.Int64
	totalCachedTokens        atomic.Int64
	totalCacheCreationTokens atomic.Int64
	totalReasoningTokens     atomic.Int64

	// Per-stage timing histograms. Drain = sink-close → drainer-join.
	// Parse = JSON unmarshal + SSE event walk + decompression. SQLite =
	// AppendTrace (one tx covering JSONL append + UPSERT + session-
	// inference IN-clause + UPDATE).
	DrainHist  *Histogram
	ParseHist  *Histogram
	SQLiteHist *Histogram
}

func New() *Counters {
	return &Counters{
		DrainHist:  NewHistogram(DefaultTimingBounds),
		ParseHist:  NewHistogram(DefaultTimingBounds),
		SQLiteHist: NewHistogram(DefaultTimingBounds),
	}
}

// IncDropWriterFull is bumped when the writer channel was full and a
// trace's metadata was dropped (writer.Writer.TrySend returns false).
func (c *Counters) IncDropWriterFull() { c.dropWriterFull.Add(1) }

// IncDropJSONLFail is bumped when JSONL append fails (disk full, EIO).
func (c *Counters) IncDropJSONLFail() { c.dropJSONLFail.Add(1) }

// IncDropSQLiteFail is bumped when SQLite upsert fails after JSONL succeeds.
func (c *Counters) IncDropSQLiteFail() { c.dropSQLiteFail.Add(1) }

// IncTruncatedReq is bumped when a trace's request capture lost bytes
// (either ring overflow or max_body_bytes cap).
func (c *Counters) IncTruncatedReq() { c.truncatedReqTotal.Add(1) }

// IncTruncatedResp is bumped when a trace's response capture lost bytes.
func (c *Counters) IncTruncatedResp() { c.truncatedRespTotal.Add(1) }

// IncAppended is bumped on each successful JSONL append.
func (c *Counters) IncAppended() { c.appended.Add(1) }

// IncAppendedByStatus bumps the right status-bucketed counter. status==0
// or a sentinel (-1) lands nowhere; only 200/4xx/5xx are tallied — those
// are the buckets that map cleanly to upstream-health questions.
func (c *Counters) IncAppendedByStatus(status int) {
	switch {
	case status >= 200 && status < 300:
		c.appended2xx.Add(1)
	case status >= 400 && status < 500:
		c.appended4xx.Add(1)
	case status >= 500 && status < 600:
		c.appended5xx.Add(1)
	}
}

// IncIndexed is bumped on each successful SQLite mirror upsert.
func (c *Counters) IncIndexed() { c.indexed.Add(1) }

// IncUpstreamDialErr is bumped from the CaptureTransport when the inner
// transport returned a non-HTTP error (DNS / TLS / connect refused).
func (c *Counters) IncUpstreamDialErr() { c.upstreamDialErr.Add(1) }

// IncSlowTrace is bumped when finalize sees an end-to-end duration
// over the operator-configured slow-trace threshold.
func (c *Counters) IncSlowTrace() { c.slowTraces.Add(1) }

// AddBytes records the size of a single JSONL line append. Used to
// expose total recorded bytes on /healthz without walking the data dir.
func (c *Counters) AddBytes(n int64) { c.totalBytes.Add(n) }

// AddMediaFiles records the number of media files extracted from a
// single trace. Called once per finalize when media extraction is on,
// with the count of files written for that trace (zero is a valid value
// but is skipped by callers to avoid a no-op atomic add).
func (c *Counters) AddMediaFiles(n int64) { c.totalMediaFiles.Add(n) }

// AddPromptTokens records the prompt-token count for a single trace
// (T3). n may be 0, which is a no-op.
func (c *Counters) AddPromptTokens(n int64) { c.totalPromptTokens.Add(n) }

// AddCompletionTokens records the completion-token count for a single
// trace (T3). n may be 0, which is a no-op.
func (c *Counters) AddCompletionTokens(n int64) { c.totalCompletionTokens.Add(n) }

// AddCachedTokens records the cached-prompt-token count for a single
// trace (T3). n may be 0, which is a no-op.
func (c *Counters) AddCachedTokens(n int64) { c.totalCachedTokens.Add(n) }

// AddCacheCreationTokens records the cache-creation-token count for a
// single trace (T3). n may be 0, which is a no-op.
func (c *Counters) AddCacheCreationTokens(n int64) { c.totalCacheCreationTokens.Add(n) }

// AddReasoningTokens records the reasoning-token count for a single
// trace (T3). n may be 0, which is a no-op.
func (c *Counters) AddReasoningTokens(n int64) { c.totalReasoningTokens.Add(n) }

// ObserveWriterChanLen records the current writer channel length;
// keeps the running max in writerChanHighWater.
func (c *Counters) ObserveWriterChanLen(n int) {
	current := int64(n)
	for {
		hw := c.writerChanHighWater.Load()
		if current <= hw {
			return
		}
		if c.writerChanHighWater.CompareAndSwap(hw, current) {
			return
		}
	}
}

// Snapshot returns a point-in-time view of all counters. Marshaled
// directly into the /healthz response body.
type Snapshot struct {
	DropWriterFull      int64 `json:"drop_writer_full"`
	DropJSONLFail       int64 `json:"drop_jsonl_fail"`
	DropSQLiteFail      int64 `json:"drop_sqlite_fail"`
	TruncatedReqTotal   int64 `json:"truncated_req_total"`
	TruncatedRespTotal  int64 `json:"truncated_resp_total"`
	WriterChanHighWater int64 `json:"writer_chan_high_water"`
	Appended            int64 `json:"appended"`
	Indexed             int64 `json:"indexed"`
	Appended2xx         int64 `json:"appended_2xx"`
	Appended4xx         int64 `json:"appended_4xx"`
	Appended5xx         int64 `json:"appended_5xx"`
	UpstreamDialErr     int64 `json:"upstream_dial_err"`
	SlowTraces          int64 `json:"slow_traces"`

	// Cumulative on-disk resource total (since process start).
	TotalBytes int64 `json:"total_bytes"`

	// Cumulative count of extracted media files (since process start).
	// Phase K. Zero when media extraction is disabled or has never run.
	TotalMediaFiles int64 `json:"total_media_files"`

	// Cumulative token totals. Sum across all appended traces of the usage
	// fields extracted by parser.ExtractUsage. Zero for protocols without usage
	// or traces where extraction failed.
	TotalPromptTokens        int64 `json:"total_prompt_tokens"`
	TotalCompletionTokens    int64 `json:"total_completion_tokens"`
	TotalCachedTokens        int64 `json:"total_cached_tokens"`
	TotalCacheCreationTokens int64 `json:"total_cache_creation_tokens"`
	TotalReasoningTokens     int64 `json:"total_reasoning_tokens"`

	Timings struct {
		DrainMs  HistogramSnapshot `json:"drain_ms"`
		ParseMs  HistogramSnapshot `json:"parse_ms"`
		SqliteMs HistogramSnapshot `json:"sqlite_ms"`
	} `json:"timings"`
}

func (c *Counters) Snapshot() Snapshot {
	s := Snapshot{
		DropWriterFull:      c.dropWriterFull.Load(),
		DropJSONLFail:       c.dropJSONLFail.Load(),
		DropSQLiteFail:      c.dropSQLiteFail.Load(),
		TruncatedReqTotal:   c.truncatedReqTotal.Load(),
		TruncatedRespTotal:  c.truncatedRespTotal.Load(),
		WriterChanHighWater: c.writerChanHighWater.Load(),
		Appended:            c.appended.Load(),
		Indexed:             c.indexed.Load(),
		Appended2xx:         c.appended2xx.Load(),
		Appended4xx:         c.appended4xx.Load(),
		Appended5xx:         c.appended5xx.Load(),
		UpstreamDialErr:     c.upstreamDialErr.Load(),
		SlowTraces:          c.slowTraces.Load(),

		TotalBytes:      c.totalBytes.Load(),
		TotalMediaFiles: c.totalMediaFiles.Load(),

		TotalPromptTokens:        c.totalPromptTokens.Load(),
		TotalCompletionTokens:    c.totalCompletionTokens.Load(),
		TotalCachedTokens:        c.totalCachedTokens.Load(),
		TotalCacheCreationTokens: c.totalCacheCreationTokens.Load(),
		TotalReasoningTokens:     c.totalReasoningTokens.Load(),
	}
	s.Timings.DrainMs = c.DrainHist.Snapshot()
	s.Timings.ParseMs = c.ParseHist.Snapshot()
	s.Timings.SqliteMs = c.SQLiteHist.Snapshot()
	return s
}
