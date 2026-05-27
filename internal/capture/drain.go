package capture

import (
	"errors"
	"io"
	"os"
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
		written     int64
		truncated   bool
		firstByteAt int64
		writeErr    error
		stopped     bool
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
	}
}
