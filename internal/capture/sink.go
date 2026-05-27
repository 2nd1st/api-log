// Package capture implements the lossy-channel + drainer pattern from
// ARCHITECTURE § 7.2 and the per-event timing capture from § 7.1 step 2.
//
// The forwarding path teas every chunk through a [Sink]. Sinks are
// non-blocking writers: a slow disk or a saturated channel never throttles
// the forwarding read. Bytes that cannot be sent on the channel are dropped
// and the sink's onDrop callback fires (which flips truncated_req / _resp).
//
// Channel payloads are [Chunk] values that carry a wire-arrival timestamp
// taken at Write() call time. The drainer is a passive consumer: when it
// later finds an SSE frame boundary in the byte stream, the relevant
// t_delta_ms is taken from the chunk that contained the first byte past
// the boundary, so the timing reflects when bytes arrived from upstream,
// not when the drainer woke up.
package capture

import (
	"time"
)

// Chunk is one byte payload on a capture channel, tagged with the time
// captureSink.Write was called (= time the inner Transport delivered these
// bytes to the proxy = wire-arrival time, modulo Go transport buffering).
type Chunk struct {
	// At is the wall-clock time the proxy received this chunk from upstream
	// (or from client, for request capture).
	At time.Time
	// Data is a private copy of the bytes; callers may reuse their buffer
	// after Write() returns.
	Data []byte
}

// Sink is a non-blocking writer that copies each Write into a fresh slice
// and tries a non-blocking send on its channel. If the channel is full
// the bytes are dropped and OnDrop is invoked (once per dropped Write).
type Sink struct {
	// Ch is the capture channel. Closed by the owner (the forwarding
	// goroutine) at finalize; Sink.Write must not be called after Close.
	Ch chan<- Chunk
	// OnDrop is called for every Write that dropped its bytes. Typically
	// flips a per-direction truncated flag plus bumps a counter.
	OnDrop func()
	// Now is overridable for tests; production keeps it nil → time.Now.
	Now func() time.Time
}

// Write copies p, stamps the current time, and non-blockingly sends the
// chunk on s.Ch. Always returns (len(p), nil) — Sinks cannot fail in the
// io.Writer sense; the forwarding goroutine's reads are never short-read
// or error'd because of capture behavior.
func (s *Sink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := make([]byte, len(p))
	copy(buf, p)

	ts := time.Now()
	if s.Now != nil {
		ts = s.Now()
	}
	c := Chunk{At: ts, Data: buf}

	select {
	case s.Ch <- c:
	default:
		if s.OnDrop != nil {
			s.OnDrop()
		}
	}
	return len(p), nil
}
