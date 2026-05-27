package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamWatchdogFiresOnIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var fired atomic.Int32
	w := NewStreamWatchdog(func() { fired.Add(1); cancel() }, 50*time.Millisecond)

	select {
	case <-ctx.Done():
		// expected within ~50ms
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog did not fire within deadline")
	}
	if fired.Load() != 1 {
		t.Errorf("fire count = %d, want 1", fired.Load())
	}
	if !w.Fired() {
		t.Error("Fired() = false after timer triggered")
	}
}

func TestStreamWatchdogPulseDelaysFire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := NewStreamWatchdog(cancel, 80*time.Millisecond)
	defer w.Stop()

	// Pulse every 20ms for 200ms. Watchdog should NOT fire.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.Pulse()
		time.Sleep(20 * time.Millisecond)
	}
	if w.Fired() {
		t.Errorf("watchdog fired despite continuous pulses")
	}
	// Now stop pulsing — it should fire within ~80ms.
	select {
	case <-ctx.Done():
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Errorf("watchdog should have fired after pulses stopped")
	}
}

func TestStreamWatchdogStopDisarms(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	w := NewStreamWatchdog(cancel, 30*time.Millisecond)
	w.Stop()

	time.Sleep(80 * time.Millisecond)
	if w.Fired() {
		t.Errorf("watchdog fired after Stop")
	}
}

func TestStreamWatchdogNilSafe(t *testing.T) {
	var w *StreamWatchdog
	w.Pulse() // must not panic
	w.Stop()  // must not panic
	if w.Fired() {
		t.Errorf("nil watchdog should report Fired() = false")
	}
}

func TestStreamWatchdogZeroTimeoutDisabled(t *testing.T) {
	w := NewStreamWatchdog(func() { t.Fatal("zero-timeout watchdog should not fire") }, 0)
	if w != nil {
		t.Errorf("zero timeout should return nil watchdog, got %p", w)
	}
}
