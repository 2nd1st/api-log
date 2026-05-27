package counters

import "testing"

func TestHistogram_BasicPercentiles(t *testing.T) {
	h := NewHistogram(DefaultTimingBounds)
	// 100 observations clustered around 10ms, with a long tail.
	for i := 0; i < 90; i++ {
		h.Observe(10)
	}
	for i := 0; i < 5; i++ {
		h.Observe(100)
	}
	for i := 0; i < 4; i++ {
		h.Observe(1000)
	}
	h.Observe(10000)

	snap := h.Snapshot()
	if snap.Count != 100 {
		t.Fatalf("count: want 100, got %d", snap.Count)
	}
	// p50 falls inside the 10ms cluster; bucket boundary 10.
	if snap.P50Ms != 10 {
		t.Errorf("p50: want 10ms, got %d", snap.P50Ms)
	}
	// p95 lands in the 100ms tail (90+5 = 95 observations <= 100ms).
	if snap.P95Ms != 100 {
		t.Errorf("p95: want 100ms, got %d", snap.P95Ms)
	}
	// p99 lands in the 1000ms tail.
	if snap.P99Ms != 1000 {
		t.Errorf("p99: want 1000ms, got %d", snap.P99Ms)
	}
	// Mean: (90*10 + 5*100 + 4*1000 + 10000) / 100 = (900+500+4000+10000)/100 = 154
	if got := snap.MeanMs; got < 153 || got > 155 {
		t.Errorf("mean: want ~154, got %v", got)
	}
}

func TestHistogram_Empty(t *testing.T) {
	h := NewHistogram(DefaultTimingBounds)
	snap := h.Snapshot()
	if snap.Count != 0 || snap.P50Ms != 0 || snap.MeanMs != 0 {
		t.Errorf("empty snapshot non-zero: %+v", snap)
	}
}

func TestAppendedByStatus(t *testing.T) {
	c := New()
	c.IncAppendedByStatus(200)
	c.IncAppendedByStatus(201)
	c.IncAppendedByStatus(404)
	c.IncAppendedByStatus(429)
	c.IncAppendedByStatus(503)
	c.IncAppendedByStatus(-1) // sentinel: not counted

	s := c.Snapshot()
	if s.Appended2xx != 2 || s.Appended4xx != 2 || s.Appended5xx != 1 {
		t.Errorf("status buckets: 2xx=%d 4xx=%d 5xx=%d (want 2,2,1)",
			s.Appended2xx, s.Appended4xx, s.Appended5xx)
	}
}
