package writer

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/2nd1st/api-log/internal/counters"
)

// TestWriterChannelSaturationDoesNotBlockProducers verifies the
// failure-mode behavior from ARCHITECTURE § 7.3 row "Writer channel
// full": producers (forwarding goroutines, in production) NEVER block;
// they get an immediate "dropped" signal and continue. The drop is
// logged via the DropWriterFull counter.
func TestWriterChannelSaturationDoesNotBlockProducers(t *testing.T) {
	dir := t.TempDir()
	ctrs := counters.New()

	// Tiny channel + NO writer goroutine consuming → every TrySend after
	// the first one drops immediately.
	w := New(dir, 1, nil, ctrs, nil, nil, nil)
	// Intentionally NOT calling Start — simulates a wedged writer.

	const concurrent = 50
	const perGoroutine = 10
	const expectedTotal = concurrent * perGoroutine

	var accepted atomic.Int64
	var wg sync.WaitGroup

	deadline := time.Now().Add(2 * time.Second)

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				// In production this is a forwarding goroutine sending
				// finalized trace metadata. Each TrySend must return
				// within microseconds, never blocking on the writer.
				start := time.Now()
				if w.TrySend(Record{Trace: chatTrace("c", []map[string]any{{"role": "user", "content": "hi"}}), KeyHash: "k"}) {
					accepted.Add(1)
				}
				// Any individual TrySend that takes >10ms here is a
				// regression on principle 2 ("capture, never interfere").
				// In practice it's a few microseconds.
				if time.Since(start) > 100*time.Millisecond {
					t.Errorf("TrySend blocked for %v — must be non-blocking", time.Since(start))
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		t.Fatal("producers did not finish within deadline — Some TrySend blocked")
	}

	snap := ctrs.Snapshot()
	dropped := snap.DropWriterFull

	// All sends accounted for: accepted + dropped == expectedTotal.
	if accepted.Load()+dropped != int64(expectedTotal) {
		t.Errorf("accepted (%d) + dropped (%d) = %d, want %d",
			accepted.Load(), dropped, accepted.Load()+dropped, expectedTotal)
	}
	// Channel cap 1, no consumer → at most 1 accepted, rest dropped.
	if accepted.Load() > 1 {
		t.Errorf("accepted = %d, expected ≤1 with wedged writer", accepted.Load())
	}
	if dropped < int64(expectedTotal-1) {
		t.Errorf("dropped = %d, want ≥ %d (everything but the first)", dropped, expectedTotal-1)
	}
}
