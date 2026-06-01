package storage

import (
	"errors"
	"sync"
	"testing"
)

// Tests live next to the implementation so they can poke at the
// private state. The zero-value Coordinator is the documented
// contract for lease-only use; tests respect that contract.

func newTestFileID(date, keyHash string) FileID {
	return FileID{DataDir: "/data", Date: date, KeyHash8: keyHash}
}

func TestAcquireLease_BasicLifecycle(t *testing.T) {
	c := &Coordinator{}
	fid := newTestFileID("2026-06-01", "a1b2c3d4")

	lease, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if lease == nil {
		t.Fatal("AcquireLease returned nil Lease without error")
	}
	if !c.isLeased(fid.CanonicalPath()) {
		t.Error("isLeased should be true after AcquireLease")
	}

	lease.Release()

	if c.isLeased(fid.CanonicalPath()) {
		t.Error("isLeased should be false after Release")
	}
	// Map entry should be cleaned up to keep memory bounded.
	if _, present := c.refs[fid.CanonicalPath()]; present {
		t.Error("refs map should not retain zero-count entries")
	}
}

func TestAcquireLease_RefcountSemantics(t *testing.T) {
	c := &Coordinator{}
	fid := newTestFileID("2026-06-01", "a1b2c3d4")

	l1, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatal(err)
	}

	// Both leases live; markDeleting should fail.
	if c.markDeleting(fid) {
		t.Error("markDeleting should fail while leases are held")
	}

	l1.Release()

	// Still one lease; markDeleting should still fail.
	if c.markDeleting(fid) {
		t.Error("markDeleting should fail while at least one lease is held")
	}

	l2.Release()

	// Now unleased; markDeleting should succeed.
	if !c.markDeleting(fid) {
		t.Error("markDeleting should succeed after all leases released")
	}
	c.unmarkDeleting(fid)
}

func TestRelease_Idempotent(t *testing.T) {
	c := &Coordinator{}
	fid := newTestFileID("2026-06-01", "a1b2c3d4")

	lease, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatal(err)
	}

	lease.Release()
	lease.Release() // second Release must be no-op, not panic, not decrement again

	// Acquire again to verify the refcount didn't go negative
	lease2, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatal(err)
	}
	if c.refs[fid.CanonicalPath()] != 1 {
		t.Errorf("refcount = %d after acquire post double-release; want 1", c.refs[fid.CanonicalPath()])
	}
	lease2.Release()
}

func TestAcquireLease_FailsDuringEviction(t *testing.T) {
	c := &Coordinator{}
	fid := newTestFileID("2026-06-01", "a1b2c3d4")

	// Simulate eviction starting
	if !c.markDeleting(fid) {
		t.Fatal("markDeleting should succeed on unleased path")
	}

	_, err := c.AcquireLease(fid)
	if !errors.Is(err, ErrFileBeingDeleted) {
		t.Errorf("AcquireLease during eviction: got %v, want ErrFileBeingDeleted", err)
	}

	c.unmarkDeleting(fid)

	// After unmarkDeleting, acquisitions succeed again
	lease, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatalf("AcquireLease after unmark: %v", err)
	}
	lease.Release()
}

func TestMarkDeleting_RefusesDoubleMark(t *testing.T) {
	c := &Coordinator{}
	fid := newTestFileID("2026-06-01", "a1b2c3d4")

	if !c.markDeleting(fid) {
		t.Fatal("first markDeleting should succeed")
	}
	defer c.unmarkDeleting(fid)

	if c.markDeleting(fid) {
		t.Error("second concurrent markDeleting should fail; only one eviction tick may own a delete at a time")
	}
}

func TestMultipleFileIDs_AreIndependent(t *testing.T) {
	c := &Coordinator{}
	a := newTestFileID("2026-06-01", "a1b2c3d4")
	b := newTestFileID("2026-06-01", "e5f6a7b8")

	leaseA, err := c.AcquireLease(a)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseA.Release()

	// Marking B deleting must not be blocked by A's lease
	if !c.markDeleting(b) {
		t.Error("markDeleting on different FileID should succeed")
	}
	c.unmarkDeleting(b)

	// Acquiring B's lease again must not be blocked by A's lease
	leaseB, err := c.AcquireLease(b)
	if err != nil {
		t.Fatalf("AcquireLease on independent FileID: %v", err)
	}
	leaseB.Release()
}

func TestConcurrent_AcquireRelease_StressAndConsistency(t *testing.T) {
	// Refcount must remain consistent under -race; we verify by
	// running N goroutines doing repeated acquire/release on the
	// same FileID and asserting the final refcount is zero.
	const goroutines = 64
	const opsPerGoroutine = 1000

	c := &Coordinator{}
	fid := newTestFileID("2026-06-01", "a1b2c3d4")

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				lease, err := c.AcquireLease(fid)
				if err != nil {
					t.Errorf("AcquireLease stress: %v", err)
					return
				}
				lease.Release()
			}
		}()
	}
	wg.Wait()

	if c.isLeased(fid.CanonicalPath()) {
		t.Errorf("after %d acquire/release pairs, expected fid unleased; got refs[%q] = %d",
			goroutines*opsPerGoroutine, fid.CanonicalPath(), c.refs[fid.CanonicalPath()])
	}
}

func TestConcurrent_MarkDeleting_BlocksConcurrentAcquire(t *testing.T) {
	// Once markDeleting succeeds, NO subsequent AcquireLease for the
	// same path may succeed until unmarkDeleting. We verify with N
	// goroutines hammering AcquireLease while the test holds the
	// deleting mark; every acquisition must return ErrFileBeingDeleted.
	const goroutines = 32
	const opsPerGoroutine = 100

	c := &Coordinator{}
	fid := newTestFileID("2026-06-01", "a1b2c3d4")

	if !c.markDeleting(fid) {
		t.Fatal("markDeleting setup failed")
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				lease, err := c.AcquireLease(fid)
				if !errors.Is(err, ErrFileBeingDeleted) {
					t.Errorf("expected ErrFileBeingDeleted under markDeleting hold; got err=%v lease=%v", err, lease)
					if lease != nil {
						lease.Release()
					}
					return
				}
			}
		}()
	}
	wg.Wait()

	c.unmarkDeleting(fid)

	// Sanity: after unmark, acquisitions work again
	lease, err := c.AcquireLease(fid)
	if err != nil {
		t.Fatalf("post-unmark AcquireLease: %v", err)
	}
	lease.Release()
}

func TestIsLeased_OnUnknownPath(t *testing.T) {
	c := &Coordinator{}
	if c.isLeased("/never/seen/before.jsonl") {
		t.Error("isLeased on unknown path should be false")
	}
}
