#!/usr/bin/env python3
"""
Real-API session inference test.

Calls sub2api endpoints (OpenAI Chat Completions + Anthropic Messages) with
realistic conversation patterns, captures the request/response wire data
exactly as api-log would, then runs the prefix-matching session algorithm
on the captured traces and verifies session grouping.

Usage: python3 real_api_test.py
"""

import json
import hashlib
import sqlite3
import time
import urllib.request
import urllib.error
from dataclasses import dataclass, field
from typing import Optional

SUB2API = "http://sub2api.homelab.lan"
OPENAI_KEY = "sk-REDACTED"
ANTHROPIC_KEY = "sk-REDACTED"

OAI_MODEL = "gpt-5.4-mini"
ANT_MODEL = "claude-haiku-4-5-20251001"


# ─── HTTP helpers ────────────────────────────────────────────────────────────


def post_json(url, headers, body, timeout=60):
    data = json.dumps(body).encode()
    full_headers = {"Content-Type": "application/json", **headers}
    req = urllib.request.Request(url, data=data, headers=full_headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"HTTP {e.code} from {url}: {body}")


def call_openai(messages):
    return post_json(
        f"{SUB2API}/v1/chat/completions",
        {"Authorization": f"Bearer {OPENAI_KEY}"},
        {
            "model": OAI_MODEL,
            "messages": messages,
            "stream": False,
        },
    )


def call_anthropic(messages, system=None):
    body = {
        "model": ANT_MODEL,
        "max_tokens": 80,
        "messages": messages,
    }
    if system:
        body["system"] = system
    return post_json(
        f"{SUB2API}/v1/messages",
        {"x-api-key": ANTHROPIC_KEY, "anthropic-version": "2023-06-01"},
        body,
    )


def extract_openai_assistant(resp):
    return resp["choices"][0]["message"]["content"]


def extract_anthropic_assistant(resp):
    return resp["content"][0]["text"]


# ─── Trace model + algorithm (mirrors algo_test.py) ──────────────────────────


@dataclass
class Trace:
    id: str
    key_hash: str
    ts_start: int
    messages: list  # ALWAYS the request messages (input to algorithm)
    protocol: str
    request_body: dict
    response_body: dict


def setup_db():
    conn = sqlite3.connect(":memory:")
    conn.execute(
        """
        CREATE TABLE traces (
            id              TEXT PRIMARY KEY,
            key_hash        TEXT,
            ts_start        INTEGER,
            messages_json   TEXT,
            messages_len    INTEGER,
            protocol        TEXT,
            parent_id       TEXT,
            session_root_id TEXT NOT NULL
        )
        """
    )
    conn.execute("CREATE INDEX idx_kh ON traces(key_hash, ts_start)")
    return conn


def find_parent(T: Trace, conn) -> Optional[tuple]:
    cursor = conn.execute(
        """
        SELECT id, messages_json, messages_len, session_root_id
        FROM traces
        WHERE key_hash = ?
          AND ts_start < ?
          AND messages_len < ?
        ORDER BY messages_len DESC, ts_start DESC
        """,
        (T.key_hash, T.ts_start, len(T.messages)),
    )
    for row in cursor:
        C_id, C_msgs_json, C_len, C_root = row
        C_messages = json.loads(C_msgs_json)
        if T.messages[:C_len] == C_messages:
            return (C_id, C_root)
    return None


def insert_trace(T: Trace, conn):
    parent = find_parent(T, conn)
    if parent:
        parent_id, session_root_id = parent
    else:
        parent_id = None
        session_root_id = T.id
    conn.execute(
        """
        INSERT INTO traces
        (id, key_hash, ts_start, messages_json, messages_len, protocol,
         parent_id, session_root_id)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            T.id,
            T.key_hash,
            T.ts_start,
            json.dumps(T.messages),
            len(T.messages),
            T.protocol,
            parent_id,
            session_root_id,
        ),
    )
    return parent_id, session_root_id


def render_tree(conn):
    rows = list(
        conn.execute(
            "SELECT id, parent_id, session_root_id, messages_len, ts_start, protocol "
            "FROM traces ORDER BY ts_start"
        )
    )
    by_id = {r[0]: r for r in rows}
    children = {}
    roots = []
    for r in rows:
        if r[1] is None:
            roots.append(r[0])
        else:
            children.setdefault(r[1], []).append(r[0])

    def walk(node_id, depth):
        r = by_id[node_id]
        prefix = "  " * depth + ("└─ " if depth > 0 else "")
        print(f"  {prefix}{r[0]:<12} [{r[5]:<10}] (root={r[2]}, len={r[3]})")
        for c in children.get(node_id, []):
            walk(c, depth + 1)

    print(f"\nSessions ({len(roots)} roots):")
    for root in roots:
        walk(root, 0)


# ─── Scenarios that actually hit the API ─────────────────────────────────────


def key_hash_for(key: str) -> str:
    return hashlib.sha256(("Bearer " + key).encode()).hexdigest()[:8]


KH_OAI = key_hash_for(OPENAI_KEY)
KH_ANT = key_hash_for(ANTHROPIC_KEY)


def trace_id(scenario, idx):
    return f"{scenario}-{idx:02d}"


def scenario_openai_multi_turn(conn, results):
    """1 conversation, 3 turns via OpenAI Chat Completions."""
    name = "openai_multi_turn"
    print(f"\n{'=' * 70}\nREAL API: {name}\n{'=' * 70}")

    sys_msg = {"role": "system", "content": "You answer briefly."}

    # Turn 1
    msgs1 = [sys_msg, {"role": "user", "content": "what is rust"}]
    r1 = call_openai(msgs1)
    a1 = extract_openai_assistant(r1)
    print(f"  T1 sent {len(msgs1)} msgs, got: {a1[:50]!r}")
    t1 = Trace(trace_id(name, 1), KH_OAI, int(time.time() * 1000),
               msgs1, "openai_chat", {"messages": msgs1}, r1)
    insert_trace(t1, conn)

    # Turn 2
    msgs2 = msgs1 + [
        {"role": "assistant", "content": a1},
        {"role": "user", "content": "show a 2-line example"},
    ]
    r2 = call_openai(msgs2)
    a2 = extract_openai_assistant(r2)
    print(f"  T2 sent {len(msgs2)} msgs, got: {a2[:50]!r}")
    t2 = Trace(trace_id(name, 2), KH_OAI, int(time.time() * 1000),
               msgs2, "openai_chat", {"messages": msgs2}, r2)
    parent, root = insert_trace(t2, conn)
    print(f"  T2 attached: parent={parent}, root={root}")

    # Turn 3
    msgs3 = msgs2 + [
        {"role": "assistant", "content": a2},
        {"role": "user", "content": "is it memory safe?"},
    ]
    r3 = call_openai(msgs3)
    a3 = extract_openai_assistant(r3)
    print(f"  T3 sent {len(msgs3)} msgs, got: {a3[:50]!r}")
    t3 = Trace(trace_id(name, 3), KH_OAI, int(time.time() * 1000),
               msgs3, "openai_chat", {"messages": msgs3}, r3)
    parent, root = insert_trace(t3, conn)
    print(f"  T3 attached: parent={parent}, root={root}")

    results.append((name, "expect 1 root", {
        "expected_session_count_delta": 1,
        "ids": [t1.id, t2.id, t3.id],
    }))


def scenario_openai_batch(conn, results):
    """5 batch single-shot via OpenAI."""
    name = "openai_batch"
    print(f"\n{'=' * 70}\nREAL API: {name}\n{'=' * 70}")

    sys_msg = {"role": "system", "content": "Answer in one sentence."}
    queries = [
        "what is python",
        "what is go",
        "what is rust",
        "what is haskell",
        "what is zig",
    ]
    trace_ids = []
    for i, q in enumerate(queries, 1):
        msgs = [sys_msg, {"role": "user", "content": q}]
        r = call_openai(msgs)
        a = extract_openai_assistant(r)
        print(f"  B{i} q={q!r}, got: {a[:40]!r}")
        t = Trace(trace_id(name, i), KH_OAI, int(time.time() * 1000),
                  msgs, "openai_chat", {"messages": msgs}, r)
        parent, root = insert_trace(t, conn)
        if parent:
            print(f"  ⚠️  B{i} unexpectedly attached to parent={parent}")
        trace_ids.append(t.id)

    results.append((name, "expect 5 independent roots", {
        "expected_session_count_delta": 5,
        "ids": trace_ids,
    }))


def scenario_openai_batch_with_followups(conn, results):
    """3 batch + 2 followups attached to specific items."""
    name = "openai_batch_fu"
    print(f"\n{'=' * 70}\nREAL API: {name}\n{'=' * 70}")

    sys_msg = {"role": "system", "content": "Answer in one sentence."}
    # 3 batch
    batch_queries = [
        ("what is recursion in 1 sentence", 1),
        ("what is currying in 1 sentence", 2),
        ("what is monad in 1 sentence", 3),
    ]
    batch_traces = []
    for q, i in batch_queries:
        msgs = [sys_msg, {"role": "user", "content": q}]
        r = call_openai(msgs)
        a = extract_openai_assistant(r)
        t = Trace(trace_id(name, i), KH_OAI, int(time.time() * 1000),
                  msgs, "openai_chat", {"messages": msgs}, r)
        parent, root = insert_trace(t, conn)
        batch_traces.append((t, a))
        print(f"  B{i} done (root={root})")

    # Follow-up to B1
    t_b1, a_b1 = batch_traces[0]
    msgs_fu1 = list(t_b1.messages) + [
        {"role": "assistant", "content": a_b1},
        {"role": "user", "content": "give a code example"},
    ]
    r_fu1 = call_openai(msgs_fu1)
    t_fu1 = Trace(trace_id(name, 11), KH_OAI, int(time.time() * 1000),
                  msgs_fu1, "openai_chat", {"messages": msgs_fu1}, r_fu1)
    parent, root = insert_trace(t_fu1, conn)
    print(f"  FU1 attached: parent={parent} (expected={t_b1.id}), root={root}")
    fu1_ok = parent == t_b1.id

    # Follow-up to B3
    t_b3, a_b3 = batch_traces[2]
    msgs_fu3 = list(t_b3.messages) + [
        {"role": "assistant", "content": a_b3},
        {"role": "user", "content": "give a code example"},
    ]
    r_fu3 = call_openai(msgs_fu3)
    t_fu3 = Trace(trace_id(name, 13), KH_OAI, int(time.time() * 1000),
                  msgs_fu3, "openai_chat", {"messages": msgs_fu3}, r_fu3)
    parent, root = insert_trace(t_fu3, conn)
    print(f"  FU3 attached: parent={parent} (expected={t_b3.id}), root={root}")
    fu3_ok = parent == t_b3.id

    results.append((name, "expect 3 roots, FU1→B1, FU3→B3", {
        "expected_session_count_delta": 3,
        "fu1_attached_correctly": fu1_ok,
        "fu3_attached_correctly": fu3_ok,
    }))


def scenario_openai_fork(conn, results):
    """Fork: same first turn, two different continuations (regenerate)."""
    name = "openai_fork"
    print(f"\n{'=' * 70}\nREAL API: {name}\n{'=' * 70}")

    msgs1 = [{"role": "user", "content": "name a fruit"}]
    r1 = call_openai(msgs1)
    a1 = extract_openai_assistant(r1)
    t1 = Trace(trace_id(name, 1), KH_OAI, int(time.time() * 1000),
               msgs1, "openai_chat", {"messages": msgs1}, r1)
    insert_trace(t1, conn)
    print(f"  T1 ({a1!r}) is root")

    # Branch A
    msgs2a = msgs1 + [
        {"role": "assistant", "content": a1},
        {"role": "user", "content": "describe its color"},
    ]
    r2a = call_openai(msgs2a)
    t2a = Trace(trace_id(name, 2), KH_OAI, int(time.time() * 1000),
                msgs2a, "openai_chat", {"messages": msgs2a}, r2a)
    parent, root = insert_trace(t2a, conn)
    print(f"  T2a (branch describe-color) parent={parent}")

    # Branch B (different continuation from the same T1)
    msgs2b = msgs1 + [
        {"role": "assistant", "content": a1},
        {"role": "user", "content": "describe its taste"},
    ]
    r2b = call_openai(msgs2b)
    t2b = Trace(trace_id(name, 3), KH_OAI, int(time.time() * 1000),
                msgs2b, "openai_chat", {"messages": msgs2b}, r2b)
    parent, root = insert_trace(t2b, conn)
    print(f"  T2b (branch describe-taste) parent={parent}")

    fork_ok = t2a.id in [r[1] for r in conn.execute(
        "SELECT id, parent_id FROM traces WHERE id IN (?, ?)", (t2a.id, t2b.id)
    )] is False and parent == t1.id

    results.append((name, "expect 1 root, t2a/t2b both children of t1", {
        "expected_session_count_delta": 1,
        "t1_id": t1.id,
    }))


def scenario_anthropic_multi_turn(conn, results):
    """3 turns via Anthropic Messages."""
    name = "anth_multi_turn"
    print(f"\n{'=' * 70}\nREAL API: {name}\n{'=' * 70}")

    system = "You answer briefly."

    msgs1 = [{"role": "user", "content": "what is golang"}]
    r1 = call_anthropic(msgs1, system=system)
    a1 = extract_anthropic_assistant(r1)
    print(f"  T1 sent {len(msgs1)} msgs, got: {a1[:50]!r}")
    t1 = Trace(trace_id(name, 1), KH_ANT, int(time.time() * 1000),
               msgs1, "anthropic_messages",
               {"system": system, "messages": msgs1}, r1)
    insert_trace(t1, conn)

    msgs2 = msgs1 + [
        {"role": "assistant", "content": a1},
        {"role": "user", "content": "show a 2-line example"},
    ]
    r2 = call_anthropic(msgs2, system=system)
    a2 = extract_anthropic_assistant(r2)
    print(f"  T2 sent {len(msgs2)} msgs, got: {a2[:50]!r}")
    t2 = Trace(trace_id(name, 2), KH_ANT, int(time.time() * 1000),
               msgs2, "anthropic_messages",
               {"system": system, "messages": msgs2}, r2)
    parent, root = insert_trace(t2, conn)
    print(f"  T2 attached: parent={parent}, root={root}")

    msgs3 = msgs2 + [
        {"role": "assistant", "content": a2},
        {"role": "user", "content": "is it good for concurrency?"},
    ]
    r3 = call_anthropic(msgs3, system=system)
    a3 = extract_anthropic_assistant(r3)
    print(f"  T3 sent {len(msgs3)} msgs, got: {a3[:50]!r}")
    t3 = Trace(trace_id(name, 3), KH_ANT, int(time.time() * 1000),
               msgs3, "anthropic_messages",
               {"system": system, "messages": msgs3}, r3)
    parent, root = insert_trace(t3, conn)
    print(f"  T3 attached: parent={parent}, root={root}")

    results.append((name, "expect 1 root", {
        "expected_session_count_delta": 1,
        "ids": [t1.id, t2.id, t3.id],
    }))


def scenario_cross_protocol(conn, results):
    """Mix one OpenAI and one Anthropic single-shot. Different key_hash, so
    they MUST end up in separate sessions."""
    name = "cross_protocol"
    print(f"\n{'=' * 70}\nREAL API: {name}\n{'=' * 70}")

    msgs_oai = [{"role": "user", "content": "what is sqlite"}]
    r_oai = call_openai(msgs_oai)
    t_oai = Trace(trace_id(name, 1), KH_OAI, int(time.time() * 1000),
                  msgs_oai, "openai_chat", {"messages": msgs_oai}, r_oai)
    insert_trace(t_oai, conn)

    msgs_ant = [{"role": "user", "content": "what is sqlite"}]
    r_ant = call_anthropic(msgs_ant)
    t_ant = Trace(trace_id(name, 2), KH_ANT, int(time.time() * 1000),
                  msgs_ant, "anthropic_messages", {"messages": msgs_ant}, r_ant)
    insert_trace(t_ant, conn)

    print(f"  OpenAI trace key_hash={KH_OAI}, Anthropic trace key_hash={KH_ANT}")
    print(f"  Cross-key isolation: same messages, different key_hash → must be 2 roots")

    results.append((name, "expect 2 roots (different key_hash)", {
        "expected_session_count_delta": 2,
    }))


# ─── Main orchestration ──────────────────────────────────────────────────────


def main():
    conn = setup_db()
    results = []

    # Run each scenario, sharing the same SQLite so prior traces can match
    # follow-ups across scenario boundaries (more realistic).
    scenarios = [
        scenario_openai_multi_turn,
        scenario_openai_batch,
        scenario_openai_batch_with_followups,
        scenario_openai_fork,
        scenario_anthropic_multi_turn,
        scenario_cross_protocol,
    ]

    for fn in scenarios:
        try:
            fn(conn, results)
        except Exception as e:
            print(f"\n  ✗ ERROR in {fn.__name__}: {e}")
            import traceback
            traceback.print_exc()

    # Final tree
    print(f"\n{'=' * 70}\nFINAL SESSION TREE\n{'=' * 70}")
    render_tree(conn)

    # Summary
    print(f"\n{'=' * 70}\nSCENARIO RESULTS\n{'=' * 70}")
    for name, desc, data in results:
        print(f"  {name}: {desc}")
        for k, v in data.items():
            print(f"    {k}: {v}")

    # Aggregate counts
    n_traces = conn.execute("SELECT COUNT(*) FROM traces").fetchone()[0]
    n_roots = conn.execute(
        "SELECT COUNT(*) FROM traces WHERE parent_id IS NULL"
    ).fetchone()[0]
    n_sessions = conn.execute(
        "SELECT COUNT(DISTINCT session_root_id) FROM traces"
    ).fetchone()[0]
    print(f"\n  Total traces: {n_traces}")
    print(f"  Total roots:  {n_roots}")
    print(f"  Total distinct sessions: {n_sessions}")


if __name__ == "__main__":
    main()
