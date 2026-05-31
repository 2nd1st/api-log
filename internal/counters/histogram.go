package counters

import (
	"math"
	"sync/atomic"
)

// Histogram is a fixed-bucket cumulative histogram used for diagnostic
// timings (drain / parse / sqlite per-trace ms). Buckets are upper-
// inclusive: an observation of v lands in the first bucket where
// v <= bounds[i].
//
// Picked over sketch / digest approaches because (a) we know the
// expected operating range a priori (single-digit ms to tens of seconds)
// and (b) the cost of an Observe is one atomic compare + one atomic add,
// matching ARCHITECTURE § 2 "capture must not interfere."
//
// Sum and Count are tracked separately so the mean is exact, even though
// per-bucket percentile reads are approximate to the bucket boundary.
type Histogram struct {
	bounds  []float64
	buckets []atomic.Int64
	sum     atomic.Int64
	count   atomic.Int64
}

// DefaultTimingBounds covers the operating range we expect for
// per-trace work: drain typically <100ms, parse <50ms, sqlite <100ms.
// Tail bounds (1s, 10s) catch path-blocked traces so the diag is still
// honest about p99 even when something stalls upstream.
var DefaultTimingBounds = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 10000, math.Inf(1)}

func NewHistogram(bounds []float64) *Histogram {
	return &Histogram{
		bounds:  bounds,
		buckets: make([]atomic.Int64, len(bounds)),
	}
}

// Observe records v (in the unit of the bounds — ms for the default).
// Safe for concurrent callers.
func (h *Histogram) Observe(v int64) {
	h.sum.Add(v)
	h.count.Add(1)
	for i, b := range h.bounds {
		if float64(v) <= b {
			h.buckets[i].Add(1)
			return
		}
	}
	// Should be unreachable when last bound is +Inf, but guard anyway.
	h.buckets[len(h.bounds)-1].Add(1)
}

// HistogramSnapshot is the /healthz-friendly view. p50/p95/p99 are
// upper-bucket-boundary approximations; mean is exact.
type HistogramSnapshot struct {
	Count  int64   `json:"count"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  int64   `json:"p50_ms"`
	P95Ms  int64   `json:"p95_ms"`
	P99Ms  int64   `json:"p99_ms"`
}

func (h *Histogram) Snapshot() HistogramSnapshot {
	count := h.count.Load()
	if count == 0 {
		return HistogramSnapshot{}
	}
	counts := make([]int64, len(h.bounds))
	total := int64(0)
	for i := range h.buckets {
		counts[i] = h.buckets[i].Load()
		total += counts[i]
	}
	return HistogramSnapshot{
		Count:  count,
		MeanMs: float64(h.sum.Load()) / float64(count),
		P50Ms:  bucketAt(counts, h.bounds, total, 0.50),
		P95Ms:  bucketAt(counts, h.bounds, total, 0.95),
		P99Ms:  bucketAt(counts, h.bounds, total, 0.99),
	}
}

// bucketAt walks the cumulative count and returns the upper bound of
// the bucket containing the p-th percentile observation. Last bucket
// (math.Inf) is reported as the second-to-last bound for readability.
func bucketAt(counts []int64, bounds []float64, total int64, p float64) int64 {
	target := int64(float64(total) * p)
	if target == 0 {
		target = 1
	}
	cum := int64(0)
	for i, c := range counts {
		cum += c
		if cum >= target {
			b := bounds[i]
			if math.IsInf(b, 1) && i > 0 {
				return int64(bounds[i-1])
			}
			return int64(b)
		}
	}
	if len(bounds) >= 2 {
		return int64(bounds[len(bounds)-2])
	}
	return 0
}
