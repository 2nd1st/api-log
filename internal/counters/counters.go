// Package counters exposes the cumulative drop / overflow counters
// surfaced on /healthz per ARCHITECTURE § 6.5.
//
// Counters are process-wide and atomic. The writer goroutine, the
// trace finalize path, and any future drop site all bump them; the
// /healthz handler reads them.
package counters

import "sync/atomic"

// Counters is the shared atomic counter set.
type Counters struct {
	dropWriterFull      atomic.Int64
	dropJSONLFail       atomic.Int64
	dropSQLiteFail      atomic.Int64
	truncatedReqTotal   atomic.Int64
	truncatedRespTotal  atomic.Int64
	writerChanHighWater atomic.Int64
	appended            atomic.Int64
	indexed             atomic.Int64
}

func New() *Counters { return &Counters{} }

// IncDropWriterFull is bumped when the writer channel was full and a
// trace's metadata was dropped (writer.Writer.TrySend returns false).
func (c *Counters) IncDropWriterFull() { c.dropWriterFull.Add(1) }

// IncDropJSONLFail is bumped when JSONL append fails (disk full, EIO).
func (c *Counters) IncDropJSONLFail() { c.dropJSONLFail.Add(1) }

// IncDropSQLiteFail is bumped when SQLite upsert fails after JSONL succeeds.
func (c *Counters) IncDropSQLiteFail() { c.dropSQLiteFail.Add(1) }

// IncTruncatedReq is bumped when a trace's request capture lost bytes
// (either ring overflow or max_body_bytes cap).
func (c *Counters) IncTruncatedReq() { c.truncatedReqTotal.Add(1) }

// IncTruncatedResp is bumped when a trace's response capture lost bytes.
func (c *Counters) IncTruncatedResp() { c.truncatedRespTotal.Add(1) }

// IncAppended is bumped on each successful JSONL append.
func (c *Counters) IncAppended() { c.appended.Add(1) }

// IncIndexed is bumped on each successful SQLite mirror upsert.
func (c *Counters) IncIndexed() { c.indexed.Add(1) }

// ObserveWriterChanLen records the current writer channel length;
// keeps the running max in writerChanHighWater.
func (c *Counters) ObserveWriterChanLen(n int) {
	current := int64(n)
	for {
		hw := c.writerChanHighWater.Load()
		if current <= hw {
			return
		}
		if c.writerChanHighWater.CompareAndSwap(hw, current) {
			return
		}
	}
}

// Snapshot returns a point-in-time view of all counters. Marshaled
// directly into the /healthz response body.
type Snapshot struct {
	DropWriterFull      int64 `json:"drop_writer_full"`
	DropJSONLFail       int64 `json:"drop_jsonl_fail"`
	DropSQLiteFail      int64 `json:"drop_sqlite_fail"`
	TruncatedReqTotal   int64 `json:"truncated_req_total"`
	TruncatedRespTotal  int64 `json:"truncated_resp_total"`
	WriterChanHighWater int64 `json:"writer_chan_high_water"`
	Appended            int64 `json:"appended"`
	Indexed             int64 `json:"indexed"`
}

func (c *Counters) Snapshot() Snapshot {
	return Snapshot{
		DropWriterFull:      c.dropWriterFull.Load(),
		DropJSONLFail:       c.dropJSONLFail.Load(),
		DropSQLiteFail:      c.dropSQLiteFail.Load(),
		TruncatedReqTotal:   c.truncatedReqTotal.Load(),
		TruncatedRespTotal:  c.truncatedRespTotal.Load(),
		WriterChanHighWater: c.writerChanHighWater.Load(),
		Appended:            c.appended.Load(),
		Indexed:             c.indexed.Load(),
	}
}
