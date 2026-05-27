package capture

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"
)

func TestSinkWriteCopiesBuffer(t *testing.T) {
	ch := make(chan Chunk, 1)
	s := &Sink{Ch: ch, OnDrop: func() { t.Fatal("unexpected drop") }}

	buf := []byte("hello")
	n, err := s.Write(buf)
	if err != nil || n != 5 {
		t.Fatalf("Write returned (%d, %v)", n, err)
	}
	// Caller-reuses-buffer test: mutate buf after Write returns; the
	// chunk on the channel must NOT change.
	buf[0] = 'X'

	c := <-ch
	if string(c.Data) != "hello" {
		t.Errorf("Chunk.Data = %q, want %q (caller's buffer leaked into chunk)", c.Data, "hello")
	}
}

func TestSinkWriteNonBlockingDropOnFullChannel(t *testing.T) {
	ch := make(chan Chunk, 1)
	var drops int32
	s := &Sink{Ch: ch, OnDrop: func() { atomic.AddInt32(&drops, 1) }}

	// Fill channel.
	if _, err := s.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	// Second write must drop (channel cap = 1, no consumer yet).
	done := make(chan struct{})
	go func() {
		_, _ = s.Write([]byte("b"))
		close(done)
	}()
	select {
	case <-done:
		// expected: Write returned without blocking
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Write should not block on full channel")
	}
	if atomic.LoadInt32(&drops) != 1 {
		t.Errorf("drops = %d, want 1", atomic.LoadInt32(&drops))
	}
}

func TestSinkWriteEmptyNoOp(t *testing.T) {
	ch := make(chan Chunk, 1)
	s := &Sink{Ch: ch, OnDrop: func() { t.Fatal("empty write should not drop") }}
	n, err := s.Write(nil)
	if err != nil || n != 0 {
		t.Fatalf("Write(nil) = (%d, %v)", n, err)
	}
	select {
	case c := <-ch:
		t.Errorf("empty write enqueued: %#v", c)
	default:
		// OK
	}
}

func TestSinkTimestamp(t *testing.T) {
	ch := make(chan Chunk, 1)
	fixed := time.Date(2026, 5, 27, 10, 23, 45, 0, time.UTC)
	s := &Sink{Ch: ch, Now: func() time.Time { return fixed }}
	if _, err := s.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	c := <-ch
	if !c.At.Equal(fixed) {
		t.Errorf("Chunk.At = %v, want %v", c.At, fixed)
	}
}

func TestDrainHonorsMaxBodyBytes(t *testing.T) {
	ch := make(chan Chunk, 4)
	var buf bytes.Buffer

	go func() {
		ch <- Chunk{At: time.Now(), Data: []byte("aaaa")}      // 4 bytes
		ch <- Chunk{At: time.Now(), Data: []byte("bbbbbbbb")}  // 8 bytes — will exceed cap
		ch <- Chunk{At: time.Now(), Data: []byte("c")}         // dropped, post-truncate
		close(ch)
	}()

	res := Drain(ch, &buf, 10)
	if res.BytesWritten != 10 {
		t.Errorf("BytesWritten = %d, want 10", res.BytesWritten)
	}
	if !res.Truncated {
		t.Errorf("Truncated = false, want true")
	}
	if buf.String() != "aaaabbbbbb" {
		t.Errorf("buf = %q, want %q", buf.String(), "aaaabbbbbb")
	}
}

func TestDrainContinuesAfterWriteFailure(t *testing.T) {
	ch := make(chan Chunk, 4)
	go func() {
		ch <- Chunk{At: time.Now(), Data: []byte("ok")}
		ch <- Chunk{At: time.Now(), Data: []byte("fail")}
		ch <- Chunk{At: time.Now(), Data: []byte("more")}
		close(ch)
	}()

	w := &failingAfterNWriter{n: 2} // succeeds on first write, then errors
	res := Drain(ch, w, 1<<20)
	if res.Err == nil {
		t.Errorf("expected write error in Err, got nil")
	}
	// Drain must have consumed all chunks despite the failure.
	if w.calls < 2 {
		t.Errorf("drain didn't keep reading channel after error: calls=%d", w.calls)
	}
}

type failingAfterNWriter struct {
	n     int // total bytes the writer accepts before erroring
	calls int
}

func (f *failingAfterNWriter) Write(p []byte) (int, error) {
	f.calls++
	if f.n <= 0 {
		return 0, errFakeIO
	}
	if len(p) > f.n {
		n := f.n
		f.n = 0
		return n, errFakeIO
	}
	f.n -= len(p)
	return len(p), nil
}

var errFakeIO = stringErr("fake io error")

type stringErr string

func (e stringErr) Error() string { return string(e) }
