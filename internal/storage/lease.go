package storage

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrFileBeingDeleted is returned by AcquireLease when the eviction
// path has begun deleting the requested FileID. Callers (writer
// during fileFor, exporter during Phase 2, gzip background worker)
// MUST treat this as "try again later" rather than as a hard error
// — it is a normal racing-with-retention outcome at scale, not a
// data corruption signal.
var ErrFileBeingDeleted = errors.New("storage: file is being deleted by retention")

// Lease is a refcounted hold on a single FileID. Multiple callers
// may hold concurrent leases on the same FileID; eviction blocks
// (returns false from markDeleting) until every lease is released.
//
// Lease values are returned by Coordinator.AcquireLease and must
// have Release called exactly once. Release is idempotent — calling
// it more than once is a no-op rather than a panic, so it's safe to
// chain `defer lease.Release()` even in code paths that might
// release explicitly first.
type Lease struct {
	coord    *Coordinator
	key      string // canonical jsonl_path
	released atomic.Bool
}

// Release releases the hold. Idempotent. Safe to call from defer
// even after an explicit Release().
func (l *Lease) Release() {
	if l.released.Swap(true) {
		return
	}
	l.coord.leaseMu.Lock()
	defer l.coord.leaseMu.Unlock()
	l.coord.refs[l.key]--
	if l.coord.refs[l.key] <= 0 {
		delete(l.coord.refs, l.key)
	}
}

// Coordinator is the storage coordination primitive: lease arbitration
// plus (added by coordinator.go in B1.7) monitor, status, inventory,
// and eviction. This file declares only the lease-state portion; the
// remaining state and the public constructor live in coordinator.go.
//
// A zero-value Coordinator is usable for lease operations — maps are
// lazily initialized under the mutex. atomic.Pointer fields are also
// nil-safe to load (returns nil pointer; consumers handle).
type Coordinator struct {
	leaseMu  sync.Mutex
	refs     map[string]int      // canonical path -> active lease count
	deleting map[string]struct{} // canonical path -> currently being deleted by eviction

	// Published status — readers (healthz handler, Settings UI) Load
	// atomically; the monitor goroutine Stores after each tick.
	// Pre-first-tick load returns nil; Status() wraps that into a
	// "pending" Status for callers.
	status atomic.Pointer[Status]

	// Dynamic retention thresholds — the subset operators mutate via
	// PUT /api/config/retention. UpdateConfig swaps the pointer
	// atomically; the monitor loads it at the start of each tick.
	// nil means "no retention" (engine still runs to maintain status).
	retention atomic.Pointer[RetentionConfig]

	// Static config — set once at construction by New() (B1.7); never
	// mutated after Start. Zero values mean "uninitialized" — tests
	// that exercise lease-only paths leave them empty; monitor paths
	// guard with default fallbacks (`if tickInterval == 0 { ... }`).
	dataDir         string        // absolute root for walkAndStat
	tickInterval    time.Duration // monitor tick cadence; B1.7 wires from Config
	evictionCap     int           // per-tick cap; default 1000
	idleEvictionLag time.Duration // grace period: files modified within this are NOT evicted; default 5m

	// Backend store — defined as a narrow Store interface in
	// monitor.go to avoid pulling internal/store/sqlite into this
	// package. The sqlite store satisfies it implicitly via duck
	// typing.
	store Store

	// Counters surface — narrow interface declared in coordinator.go.
	// Nil-safe: the Coordinator nil-checks before each call so unit
	// tests that don't care about metrics can leave it empty.
	counters Counters

	// Last inventory snapshot, refreshed by monitor.runTick. Used by
	// Inventory() (Coordinator's public accessor for ops UI / debug).
	// Held under its own RWMutex rather than swapped via
	// atomic.Pointer because []FileInfo is large and callers usually
	// want a defensive copy anyway.
	inventoryMu   sync.RWMutex
	lastInventory []FileInfo
}

// AcquireLease borrows a refcounted hold on fid. The caller must
// call Release on the returned Lease exactly once.
//
// Returns ErrFileBeingDeleted if eviction is mid-delete on this fid;
// the caller's correct response is to skip the operation that
// required the lease (writer: return the error and let the trace
// route to a new bucket on the next finalize; exporter: skip the
// group with a warn log; gzip worker: log + return).
func (c *Coordinator) AcquireLease(fid FileID) (*Lease, error) {
	key := fid.CanonicalPath()
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	if _, beingDeleted := c.deleting[key]; beingDeleted {
		return nil, ErrFileBeingDeleted
	}
	if c.refs == nil {
		c.refs = make(map[string]int)
	}
	c.refs[key]++
	return &Lease{coord: c, key: key}, nil
}

// markDeleting is the eviction-side first half of deleteIfIdle.
// Returns true if the path is unleased AND not already being deleted
// — caller proceeds with the on-disk + SQLite delete sequence and
// MUST call unmarkDeleting (deferred) when done.
//
// Returns false if the path has active leases (eviction skips this
// tick; the file gets another chance next hour) or is already being
// deleted by a concurrent tick (treat as "someone else is handling
// it"; eviction skips this candidate).
//
// Package-private — only the eviction code path (coordinator.go +
// eviction.go in B1.6) is allowed to call this. AcquireLease and
// markDeleting share the same mutex so the (read, mark) sequence is
// atomic with respect to lease acquires.
func (c *Coordinator) markDeleting(fid FileID) bool {
	key := fid.CanonicalPath()
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	if c.refs[key] > 0 {
		return false
	}
	if _, already := c.deleting[key]; already {
		return false
	}
	if c.deleting == nil {
		c.deleting = make(map[string]struct{})
	}
	c.deleting[key] = struct{}{}
	return true
}

// unmarkDeleting releases the eviction-side hold set by markDeleting.
// Always paired with a successful markDeleting via defer.
func (c *Coordinator) unmarkDeleting(fid FileID) {
	key := fid.CanonicalPath()
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	delete(c.deleting, key)
}

// isLeased reports whether a canonical path has any active leases.
// Used by reconcileOrphans (B1.5) to skip orphan-row deletion for
// paths a writer is concurrently creating but hasn't yet flushed
// to the filesystem.
//
// Takes the canonical path directly (not a FileID) because
// reconcileOrphans reads paths from SQLite where the canonical form
// is already the stored shape.
func (c *Coordinator) isLeased(canonicalPath string) bool {
	c.leaseMu.Lock()
	defer c.leaseMu.Unlock()
	return c.refs[canonicalPath] > 0
}
