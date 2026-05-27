package writer

import (
	"compress/gzip"
	"io"
)

// newGzWriter is split into its own file so the gzip stdlib import lives
// behind a typed surface — makes it easy to swap to klauspost/compress
// later for speed without touching writer.go.
func newGzWriter(w io.Writer) io.WriteCloser {
	return gzip.NewWriter(w)
}
