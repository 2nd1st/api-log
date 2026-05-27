package proxy

import (
	"context"
	"sync/atomic"
	"time"
)

// StreamWatchdog implements the per-stream idle timeout described in
// ARCHITECTURE § 10.7. A timer is reset on each forwarded chunk via
// Pulse(); if no chunk arrives for `timeout`, the watchdog cancels
// the request context. That unblocks the ReverseProxy body copy
// (which sees ctx cancellation), bubbles up an error to ServeHTTP,
// and lets finalize set disconnected: true.
//
// The watchdog is "one shot": once it fires, further Pulse calls
// are no-ops. After finalize, callers should Stop() to release the
// underlying timer.
type StreamWatchdog struct {
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	fired   atomic.Bool
	stopped atomic.Bool
}

// NewStreamWatchdog starts the timer immediately. If `timeout` is
// zero or negative, returns nil — caller treats nil as "disabled".
func NewStreamWatchdog(cancel context.CancelFunc, timeout time.Duration) *StreamWatchdog {
	if timeout <= 0 {
		return nil
	}
	w := &StreamWatchdog{cancel: cancel, timeout: timeout}
	w.timer = time.AfterFunc(timeout, w.fire)
	return w
}

// Pulse resets the timer. Safe for concurrent calls. No-op after Stop
// or fire. Designed to be cheap enough to call on every chunk Write.
func (w *StreamWatchdog) Pulse() {
	if w == nil || w.stopped.Load() || w.fired.Load() {
		return
	}
	w.timer.Reset(w.timeout)
}

// Stop disarms the watchdog. Idempotent.
func (w *StreamWatchdog) Stop() {
	if w == nil {
		return
	}
	w.stopped.Store(true)
	w.timer.Stop()
}

// Fired reports whether the watchdog triggered (timeout expired and
// the context cancellation has been invoked).
func (w *StreamWatchdog) Fired() bool {
	if w == nil {
		return false
	}
	return w.fired.Load()
}

func (w *StreamWatchdog) fire() {
	if w.stopped.Load() {
		return
	}
	if w.fired.CompareAndSwap(false, true) {
		w.cancel()
	}
}
