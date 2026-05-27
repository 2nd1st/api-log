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
	"sync"
	"time"
)

// chunkBufPool reuses backing arrays for the byte slices that travel
// the capture channel. Without pooling, each Sink.Write allocates
// `make([]byte, len(p))` — at high throughput (a 2000-user team's
// bursts run ~500-1000 traces/sec with multiple chunks per trace),
// that allocation rate creates real GC pressure.
//
// Pool stores byte slices keyed by no size class — Go's pool returns
// whatever it has; the caller resizes / discards as needed. Since SSE
// chunks are typically 4-32 KB and JSON request bodies vary widely,
// we just retain the underlying capacity per slice and let the GC
// reclaim if the pool grows huge.
var chunkBufPool = sync.Pool{
	New: func() any {
		// Return a small backing array; chunks larger than this just
		// allocate fresh (rare for SSE; common for one-shot JSON bodies).
		b := make([]byte, 0, 4096)
		return &b
	},
}

// putChunkBuf returns a chunk's buf back to the pool if its capacity
// is reasonable. Large outliers are dropped so the pool doesn't retain
// big allocations forever.
func putChunkBuf(buf []byte) {
	const maxRetainCap = 64 * 1024 // 64 KB — drop anything bigger
	if cap(buf) == 0 || cap(buf) > maxRetainCap {
		return
	}
	buf = buf[:0]
	chunkBufPool.Put(&buf)
}

// ReleaseChunk returns a Chunk's backing buffer to the pool. The
// drainer calls this once it has finished consuming the chunk.
func ReleaseChunk(c Chunk) {
	putChunkBuf(c.Data)
}

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
	// OnByte is called once per non-empty Write — *before* the channel
	// send — so a stream-idle watchdog can pulse without depending on
	// the chunk reaching the drainer. Optional; nil = no callback.
	OnByte func()
	// Now is overridable for tests; production keeps it nil → time.Now.
	Now func() time.Time
}

// Write copies p, stamps the current time, and non-blockingly sends the
// chunk on s.Ch. Always returns (len(p), nil) — Sinks cannot fail in the
// io.Writer sense; the forwarding goroutine's reads are never short-read
// or error'd because of capture behavior.
//
// The byte slice is taken from a sync.Pool; the drainer must call
// ReleaseChunk(c) once it has finished consuming the chunk so the
// backing array returns to the pool.
func (s *Sink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.OnByte != nil {
		s.OnByte()
	}
	bufPtr := chunkBufPool.Get().(*[]byte)
	buf := *bufPtr
	if cap(buf) < len(p) {
		// Pool slot wasn't large enough — allocate a fresh slice large
		// enough for this write. Future writes of similar size will
		// recycle this larger allocation through the pool.
		buf = make([]byte, len(p))
	} else {
		buf = buf[:len(p)]
	}
	copy(buf, p)

	ts := time.Now()
	if s.Now != nil {
		ts = s.Now()
	}
	c := Chunk{At: ts, Data: buf}

	select {
	case s.Ch <- c:
	default:
		// Channel full → drop. Return the buffer to the pool so we don't
		// leak it on a drop spike.
		putChunkBuf(buf)
		if s.OnDrop != nil {
			s.OnDrop()
		}
	}
	return len(p), nil
}
