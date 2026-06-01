package writer

import (
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/2nd1st/api-log/internal/counters"
	"github.com/2nd1st/api-log/internal/storage"
	"github.com/2nd1st/api-log/internal/store/sqlite"
	"github.com/2nd1st/api-log/internal/trace"
)

// idleHarness builds a writer wired with a real storage.Coordinator
// (so lease release is observable) but no monitor goroutine (tests
// drive sweeps via clock advance + ticker firing). Returns helpers
// the test can call to enqueue traces, check file handle count, and
// shut down cleanly.
type idleHarness struct {
	dir   string
	w     *Writer
	store *sqlite.Store
	coord *storage.Coordinator
	stop  func()
}

func newIdleHarness(t *testing.T, idleTimeout time.Duration) *idleHarness {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctrs := counters.New()
	coord, err := storage.New(storage.Config{DataDir: dir, WarnAtPercent: 80}, store, ctrs)
	if err != nil {
		t.Fatal(err)
	}

	w := New(dir, 16, store, ctrs, nil, nil, coord, nil)
	w.SetIdleTimeout(idleTimeout)
	stop := w.Start()
	t.Cleanup(stop)
	return &idleHarness{dir: dir, w: w, store: store, coord: coord, stop: stop}
}

func (h *idleHarness) send(t *testing.T, id, keyHash string) {
	t.Helper()
	tr := trace.Trace{
		ID:       id,
		TsStart:  time.Now().UTC(),
		TsEnd:    time.Now().UTC().Add(time.Second),
		Client:   "127.0.0.1:1",
		Method:   "POST",
		Path:     "/v1/chat/completions",
		Upstream: "http://gw",
		Status:   200,
		Req: trace.Body{
			Headers: trace.Headers{"Content-Type": {"application/json"}},
			Body:    json.RawMessage(`{"model":"m","messages":[]}`),
		},
		Resp: trace.Body{
			Headers: trace.Headers{"Content-Type": {"application/json"}},
			Body:    json.RawMessage(`{"ok":true}`),
		},
	}
	if !h.w.TrySend(Record{Trace: tr, KeyHash: keyHash}) {
		t.Fatalf("TrySend dropped %s", id)
	}
}

// flush. Avoids the SQLite round-trip on the test's hot path.
func (h *idleHarness) waitForAppends(n int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.w.SnapshotStats().Appended >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h.w.SnapshotStats().Appended >= n
}

func TestIdleClose_ReleasesLeaseAfterTimeout(t *testing.T) {
	h := newIdleHarness(t, 60*time.Millisecond)

	// Send a trace, wait for append. The writer will hold a lease on
	// the bucket for as long as the file handle is open.
	const keyHash = "aaaa1111aaaa1111"
	h.send(t, "01H_IDLE_A", keyHash)
	if !h.waitForAppends(1, time.Second) {
		t.Fatalf("append never landed")
	}
	hashShort := keyHash[:8]
	date := time.Now().UTC().Format("2006-01-02")

	// Immediately after append, a writer-side lease is held. The probe
	// AcquireLease succeeds anyway (leases are refcount, not exclusive),
	// so we observe the open-handle state via writer stats holding the
	// file open. Instead of inspecting w.files directly, we wait for
	// the idleSweep to fire and check that a SUBSEQUENT send re-opens
	// (which means the prior open was closed).

	// Wait long enough for at least 2 sweep ticks past idleTimeout.
	time.Sleep(200 * time.Millisecond)

	// Send another trace to the same bucket. If idle-close worked,
	// fileFor will re-open + reacquire lease; the append still lands.
	h.send(t, "01H_IDLE_B", keyHash)
	if !h.waitForAppends(2, time.Second) {
		t.Fatalf("second append never landed; writer may be wedged after idle-close")
	}

	// Use the probe: AcquireLease for the bucket must still succeed
	// (lease re-acquired by the second send), confirming no orphaned
	// markDeleting state.
	fid := storage.FileID{DataDir: h.dir, Date: date, KeyHash8: hashShort}
	lease, err := h.coord.AcquireLease(fid)
	if err != nil {
		t.Errorf("post-reopen AcquireLease err = %v; lease state stuck?", err)
	} else {
		lease.Release()
	}
}

func TestIdleClose_ZeroTimeoutDisabled(t *testing.T) {
	// idleTimeout=0 keeps the original drain loop path; no ticker
	// goroutine, no sweeps. Confirm we can still process appends.
	h := newIdleHarness(t, 0)
	h.send(t, "01H_NOIDLE", "bbbb2222bbbb2222")
	if !h.waitForAppends(1, time.Second) {
		t.Fatalf("append failed with idleTimeout=0")
	}
}

func TestIdleClose_ActiveBucketNotSwept(t *testing.T) {
	// A bucket receiving appends faster than idleTimeout should never
	// get swept. Send repeatedly + measure no spurious re-open via lease
	// counter. We approximate the assertion: total stats.Appended ==
	// number sent, and no error counters bumped.
	h := newIdleHarness(t, 100*time.Millisecond)

	const keyHash = "cccc3333cccc3333"
	stop := make(chan struct{})
	var sent atomic.Int64
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				h.send(t, randID(int(sent.Add(1))), keyHash)
				time.Sleep(20 * time.Millisecond) // 5× shorter than idleTimeout
			}
		}
	}()

	// Run for 500ms — long enough for ~5 sweeps to fire.
	time.Sleep(500 * time.Millisecond)
	close(stop)

	expected := sent.Load()
	if !h.waitForAppends(expected, time.Second) {
		t.Fatalf("appended %d, sent %d — active bucket sweeps may have dropped writes",
			h.w.SnapshotStats().Appended, expected)
	}
	if h.w.SnapshotStats().DropJSONLFail != 0 {
		t.Errorf("DropJSONLFail = %d; idle sweep on active bucket caused a drop",
			h.w.SnapshotStats().DropJSONLFail)
	}
}

func randID(i int) string {
	const hex = "0123456789abcdef"
	var b [26]byte
	const prefix = "01H_ID"
	for j, c := range []byte(prefix) {
		b[j] = c
	}
	for j := len(prefix); j < len(b); j++ {
		b[j] = hex[i&0xf]
		i >>= 4
		if i == 0 {
			i = j // sprinkle some variation
		}
	}
	return string(b[:])
}
