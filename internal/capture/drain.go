package capture

import (
	"errors"
	"io"
	"os"
	"time"
)

// DrainResult is the outcome of a single capture direction's drain loop.
// Populated by Drain() and consumed by the trace's finalize step.
type DrainResult struct {
	// BytesWritten is the total bytes written to the tmp file. Equals the
	// uncompressed Content-Length for clean captures; less if Truncated.
	BytesWritten int64
	// Truncated is true if max_body_bytes was exceeded; further bytes were
	// silently dropped after the cap. Note: this is distinct from the
	// channel-level drop tracked by Sink.OnDrop — both flip truncated_*
	// on the trace, but for different reasons.
	Truncated bool
	// FirstByteAt is the timestamp from the first non-empty chunk seen, or
	// zero if no chunks arrived. Used for sanity / debugging; not in JSONL.
	FirstByteAt int64 // unix ns
	// Err is the first non-EOF write error, if any. The drain itself does
	// not abort on disk errors — it stops writing but keeps draining the
	// channel so the producer can complete.
	Err error
	// ChunkTimings records the (file offset, arrival time) of each
	// non-empty chunk that was written. Used by the finalize phase to
	// attach per-event t_delta_ms to SSE events per ARCHITECTURE § 3.3.
	//
	// Chronologically ordered (offsets monotonically increase). Empty
	// for traces that had no bytes (or only dropped bytes).
	ChunkTimings []ChunkTiming
}

// ChunkTiming pairs a tmp-file byte offset with the wire-arrival time
// of the chunk that produced those bytes. The drainer records one per
// successfully written chunk so finalize can look up: "which chunk
// contained the first byte of this SSE event?" → its timestamp.
type ChunkTiming struct {
	Offset int64     // file offset where this chunk's bytes start
	At     time.Time // wire-arrival time recorded by Sink.Write
}

// Drain reads chunks from ch and appends their bytes to tmpFile, enforcing
// the maxBodyBytes cap. Returns when ch is closed.
//
// The drain never blocks the producer (ch is bounded; producers send
// non-blockingly via Sink). If disk writes fail mid-stream, drain stops
// writing but keeps reading the channel to EOF — this guarantees the
// producer's non-blocking sends keep finding the channel "open enough"
// to drop or send.
func Drain(ch <-chan Chunk, tmpFile io.Writer, maxBodyBytes int64) DrainResult {
	var (
		written      int64
		truncated    bool
		firstByteAt  int64
		writeErr     error
		stopped      bool
		chunkTimings []ChunkTiming
	)

	for c := range ch {
		if len(c.Data) == 0 {
			ReleaseChunk(c)
			continue
		}
		if firstByteAt == 0 {
			firstByteAt = c.At.UnixNano()
		}
		if stopped {
			// Continue draining the channel but discard.
			ReleaseChunk(c)
			continue
		}
		// Enforce max_body_bytes: write only up to the remaining quota,
		// then mark truncated and stop further writes.
		remaining := maxBodyBytes - written
		if remaining <= 0 {
			truncated = true
			stopped = true
			ReleaseChunk(c)
			continue
		}
		toWrite := c.Data
		if int64(len(toWrite)) > remaining {
			toWrite = toWrite[:remaining]
			truncated = true
			stopped = true
		}
		// Record this chunk's start offset and arrival time BEFORE writing
		// so finalize can later map a byte offset to a wire-arrival time
		// (§ 7.1 step 2 / § 3.3 t_delta_ms).
		chunkTimings = append(chunkTimings, ChunkTiming{Offset: written, At: c.At})
		n, err := tmpFile.Write(toWrite)
		written += int64(n)
		if err != nil && !errors.Is(err, os.ErrClosed) {
			writeErr = err
			stopped = true
		}
		ReleaseChunk(c)
	}

	return DrainResult{
		BytesWritten: written,
		Truncated:    truncated,
		FirstByteAt:  firstByteAt,
		Err:          writeErr,
		ChunkTimings: chunkTimings,
	}
}

// LookupChunkTime returns the wall-clock arrival time of the chunk that
// contained the byte at the given file offset, or the zero time if no
// such chunk exists.
//
// Used at finalize to attach t_delta_ms to each SSE event. Callers
// should pre-sort timings by offset; the drainer already does this
// because chunks are processed in order. Binary search; O(log N).
func LookupChunkTime(timings []ChunkTiming, offset int64) (time.Time, bool) {
	if len(timings) == 0 || offset < timings[0].Offset {
		return time.Time{}, false
	}
	// Largest i s.t. timings[i].Offset <= offset.
	lo, hi := 0, len(timings)
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if timings[mid].Offset <= offset {
			lo = mid
		} else {
			hi = mid
		}
	}
	return timings[lo].At, true
}
