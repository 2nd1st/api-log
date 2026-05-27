#!/usr/bin/env python3
"""
Session inference benchmark.

Generates N synthetic sessions × M turns, runs the same algorithm as
algo_test.py on a SQLite database, measures:
  - total wall-clock time
  - per-trace mean / p50 / p99 latency
  - throughput (traces / sec)
  - effect of candidate window size

The current algorithm loads candidate messages from JSONL on every probe.
This benchmark approximates that cost by keeping candidate messages in
the SQLite messages_json column (representing a worst-case unindexed scan).

Run: python3 benchmark.py
"""

import sqlite3
import json
import time
import statistics
from typing import Optional


def setup_db():
    conn = sqlite3.connect(":memory:")
    conn.execute("""
        CREATE TABLE traces (
            id              TEXT PRIMARY KEY,
            key_hash        TEXT,
            ts_start        INTEGER,
            messages_json   TEXT,
            messages_len    INTEGER,
            parent_id       TEXT,
            session_root_id TEXT NOT NULL
        )
    """)
    conn.execute("CREATE INDEX idx_kh ON traces(key_hash, ts_start)")
    return conn


def find_parent_v1(T_messages, T_key_hash, T_ts, conn) -> Optional[tuple]:
    """Algorithm v1: as documented in ARCHITECTURE.md §5.1."""
    cursor = conn.execute(
        """
        SELECT id, messages_json, messages_len, session_root_id
        FROM traces
        WHERE key_hash = ?
          AND ts_start < ?
          AND messages_len < ?
        ORDER BY messages_len DESC, ts_start DESC
        LIMIT 100
        """,
        (T_key_hash, T_ts, len(T_messages)),
    )
    for row in cursor:
        C_id, C_msgs_json, C_len, C_root = row
        C_messages = json.loads(C_msgs_json)
        if T_messages[:C_len] == C_messages:
            return (C_id, C_root)
    return None


def insert_trace(tid, key_hash, ts, messages, conn):
    parent = find_parent_v1(messages, key_hash, ts, conn)
    if parent:
        parent_id, root = parent
    else:
        parent_id = None
        root = tid
    conn.execute(
        "INSERT INTO traces VALUES (?, ?, ?, ?, ?, ?, ?)",
        (tid, key_hash, ts, json.dumps(messages), len(messages), parent_id, root),
    )
    return parent_id, root


def generate_workload(num_sessions: int, turns_per_session: int, num_keys: int):
    """Generate a realistic workload: many sessions, each with multiple turns,
    interleaved by timestamp across multiple keys."""
    traces = []
    ts = 0
    for s in range(num_sessions):
        key = f"key{s % num_keys:04d}"
        for turn in range(turns_per_session):
            # Build messages: system + (user, assistant) * turn + user
            msgs = [{"role": "system", "content": f"sess{s}"}]
            for t in range(turn):
                msgs.append({"role": "user", "content": f"user-s{s}-t{t}"})
                msgs.append({"role": "assistant", "content": f"asst-s{s}-t{t}"})
            msgs.append({"role": "user", "content": f"user-s{s}-t{turn}"})
            tid = f"s{s:05d}-t{turn:02d}"
            traces.append((tid, key, ts, msgs))
            ts += 1
    # Shuffle by interleaving sessions (more realistic than session-by-session)
    traces.sort(key=lambda x: (x[2],))  # ordered by ts (already is)
    return traces


def run_benchmark(num_sessions, turns_per_session, num_keys, label):
    print(f"\n{'=' * 70}")
    print(f"BENCHMARK: {label}")
    print(
        f"  {num_sessions} sessions × {turns_per_session} turns/session "
        f"× {num_keys} keys = {num_sessions * turns_per_session} traces"
    )
    print("=" * 70)

    traces = generate_workload(num_sessions, turns_per_session, num_keys)
    total = len(traces)

    conn = setup_db()
    latencies = []
    start = time.perf_counter()

    for tid, key, ts, msgs in traces:
        t0 = time.perf_counter()
        insert_trace(tid, key, ts, msgs, conn)
        t1 = time.perf_counter()
        latencies.append((t1 - t0) * 1000)  # ms

    elapsed = time.perf_counter() - start

    # Verify session count
    n_roots = conn.execute(
        "SELECT COUNT(*) FROM traces WHERE parent_id IS NULL"
    ).fetchone()[0]

    print(f"  Wall clock:      {elapsed * 1000:8.1f} ms")
    print(f"  Throughput:      {total / elapsed:8.0f} traces / sec")
    print(f"  Mean latency:    {statistics.mean(latencies):8.3f} ms")
    print(f"  Median:          {statistics.median(latencies):8.3f} ms")
    print(f"  p95:             {sorted(latencies)[int(0.95 * len(latencies))]:8.3f} ms")
    print(f"  p99:             {sorted(latencies)[int(0.99 * len(latencies))]:8.3f} ms")
    print(f"  Max:             {max(latencies):8.3f} ms")
    print(f"  Distinct roots:  {n_roots} (expected {num_sessions})")

    if n_roots != num_sessions:
        print(f"  ⚠️  Session count mismatch!")

    return {
        "label": label,
        "total": total,
        "elapsed_ms": elapsed * 1000,
        "throughput": total / elapsed,
        "mean_ms": statistics.mean(latencies),
        "p99_ms": sorted(latencies)[int(0.99 * len(latencies))],
        "roots": n_roots,
        "expected_roots": num_sessions,
    }


def main():
    results = []

    # Small: warmup + sanity
    results.append(run_benchmark(100, 5, 10, "100 sessions × 5 turns × 10 keys (500 traces)"))

    # Medium: realistic single-user-day
    results.append(run_benchmark(1000, 5, 10, "1000 sessions × 5 turns × 10 keys (5000 traces)"))

    # Large: heavy multi-user day
    results.append(run_benchmark(2000, 10, 50, "2000 sessions × 10 turns × 50 keys (20000 traces)"))

    # Long sessions: stress the candidate scan
    results.append(run_benchmark(500, 30, 10, "500 sessions × 30 turns × 10 keys (15000 traces)"))

    print(f"\n{'=' * 70}")
    print("SUMMARY")
    print("=" * 70)
    print(f"  {'label':<60} {'thr/sec':>10} {'p99 ms':>10}")
    for r in results:
        print(f"  {r['label']:<60} {r['throughput']:>10.0f} {r['p99_ms']:>10.3f}")

    print()
    print("  Interpretation:")
    print("  - If throughput drops sharply as turns/session grows, the candidate")
    print("    scan is the bottleneck (each probe loads more messages_json blobs).")
    print("  - If p99 stays under ~10ms, the algorithm is fine for v0.")
    print("  - If p99 exceeds ~50ms, a canonical_hash-based shortcut is needed.")


if __name__ == "__main__":
    main()
