package writer

import (
	"compress/gzip"
	"io"
)

// rotateCompressionLevel is the gzip level the day-cross rotation
// runs at. v0.1.0–v0.1.2 used the zero default (gzip.DefaultCompression
// via gzip.NewWriter; in practice this is level 6) for rotation, which
// was fine. Level 6 is the right balance — rotation runs in a
// background goroutine off the writer hot path, so the modest extra
// CPU buys 20–30% smaller files on JSONL (which is highly redundant
// text). v0.1.3 makes the choice explicit so future tuning has a
// single named knob.
const rotateCompressionLevel = gzip.BestCompression - 3 // = 6

// newGzWriter is split into its own file so the gzip stdlib import lives
// behind a typed surface — makes it easy to swap to klauspost/compress
// later for speed without touching writer.go.
//
// Compression level: rotateCompressionLevel (6). The writer's hot path
// never gzips — rotation runs in a separate background goroutine
// (writer.go compressInPlace), so the CPU cost is bounded and not on
// the request critical path.
func newGzWriter(w io.Writer) io.WriteCloser {
	// NewWriterLevel only errors on out-of-range level; the named
	// constant above is in-range so the error path is unreachable.
	// Fall back to NewWriter if a future refactor passes an invalid
	// level — better a working binary than a panic.
	gz, err := gzip.NewWriterLevel(w, rotateCompressionLevel)
	if err != nil {
		return gzip.NewWriter(w)
	}
	return gz
}
