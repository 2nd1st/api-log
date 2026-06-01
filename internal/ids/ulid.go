// Package ids provides trace IDs and deterministic hashes derived from named
// request values.
package ids

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// monotonic is a thread-safe ULID entropy source. ULIDs are lexically
// time-sortable per ARCHITECTURE § 10.5, which lets `ls data/<date>/`
// be naturally chronological.
var monotonic = struct {
	mu sync.Mutex
	r  *ulid.MonotonicEntropy
}{
	r: ulid.Monotonic(rand.Reader, 0),
}

// NewTraceID returns a new ULID for the current time using the package
// entropy source. Safe for concurrent use.
func NewTraceID() string {
	monotonic.mu.Lock()
	defer monotonic.mu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), monotonic.r).String()
}
