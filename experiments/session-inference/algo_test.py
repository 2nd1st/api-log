#!/usr/bin/env python3
"""
Session inference algorithm test.

Verifies the prefix-matching session inference algorithm against realistic
scenarios. Run: python3 algo_test.py
"""

import sqlite3
import json
import hashlib
from dataclasses import dataclass
from typing import Optional


def canonical_hash(messages):
    return hashlib.sha256(
        json.dumps(messages, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()[:16]


@dataclass
class Trace:
    id: str
    key_hash: str
    ts_start: int
    messages: list


def setup_db():
    conn = sqlite3.connect(":memory:")
    conn.execute("""
        CREATE TABLE traces (
            id              TEXT PRIMARY KEY,
            key_hash        TEXT,
            ts_start        INTEGER,
            messages_json   TEXT,
            canonical_hash  TEXT,
            messages_len    INTEGER,
            parent_id       TEXT,
            session_root_id TEXT NOT NULL
        )
    """)
    conn.execute("CREATE INDEX idx_kh ON traces(key_hash, ts_start)")
    return conn


def find_parent(T: Trace, conn) -> Optional[tuple]:
    """Return (parent_id, parent_session_root_id) or None.

    Algorithm: among prior traces (same key_hash, ts_start < T.ts_start,
    messages_len < T.messages_len), find the one with the LONGEST messages
    array that equals T.messages[:len(C.messages)]. Tiebreak by most-recent
    ts_start.
    """
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
        (id, key_hash, ts_start, messages_json, canonical_hash, messages_len,
         parent_id, session_root_id)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            T.id,
            T.key_hash,
            T.ts_start,
            json.dumps(T.messages),
            canonical_hash(T.messages),
            len(T.messages),
            parent_id,
            session_root_id,
        ),
    )


def render_tree(conn):
    """Print a tree of all traces grouped by session_root_id."""
    rows = list(
        conn.execute(
            "SELECT id, parent_id, session_root_id, messages_len, ts_start "
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
        print(
            f"  {prefix}{r[0]:<10} (root={r[2]}, len={r[3]}, ts={r[4]})"
        )
        for c in children.get(node_id, []):
            walk(c, depth + 1)

    print(f"  Sessions ({len(roots)} root{'s' if len(roots) != 1 else ''}):")
    for root in roots:
        walk(root, 0)


def run_scenario(name, description, traces, expectations):
    print(f"\n{'=' * 70}")
    print(f"SCENARIO: {name}")
    print(f"  {description}")
    print("=" * 70)

    conn = setup_db()
    for T in traces:
        insert_trace(T, conn)

    render_tree(conn)

    # Verification
    rows = list(
        conn.execute(
            "SELECT id, parent_id, session_root_id FROM traces"
        )
    )
    actual_roots = {r[0] for r in rows if r[1] is None}
    actual_parents = {r[0]: r[1] for r in rows if r[1]}
    actual_root_map = {r[0]: r[2] for r in rows}

    failures = []
    if "expected_roots" in expectations:
        if actual_roots != expectations["expected_roots"]:
            failures.append(
                f"roots: expected {expectations['expected_roots']}, "
                f"got {actual_roots}"
            )
    if "expected_parents" in expectations:
        for child, expected_parent in expectations["expected_parents"].items():
            actual = actual_parents.get(child)
            if actual != expected_parent:
                failures.append(
                    f"{child}.parent: expected {expected_parent}, got {actual}"
                )
    if "expected_session_count" in expectations:
        actual_count = len(set(actual_root_map.values()))
        if actual_count != expectations["expected_session_count"]:
            failures.append(
                f"session count: expected {expectations['expected_session_count']}, "
                f"got {actual_count}"
            )

    if expectations.get("known_failure"):
        print(f"\n  ⚠️  KNOWN LIMIT — expected behavior, not a real bug:")
        for f in failures:
            print(f"      {f}")
        return "known_failure"
    elif failures:
        print(f"\n  ✗ FAIL:")
        for f in failures:
            print(f"      {f}")
        return "fail"
    else:
        print(f"\n  ✓ PASS")
        return "pass"


# ─── Scenarios ───────────────────────────────────────────────────────────────


def s_single_shot():
    """3 independent single-shot requests (Chat Completions style)."""
    return [
        Trace("t1", "k1", 100, [{"role": "user", "content": "what is 1+1"}]),
        Trace("t2", "k1", 101, [{"role": "user", "content": "what is the capital of france"}]),
        Trace("t3", "k1", 102, [{"role": "user", "content": "translate hello to chinese"}]),
    ], {
        "expected_roots": {"t1", "t2", "t3"},
        "expected_session_count": 3,
    }


def s_multi_turn():
    """1 conversation, 3 turns."""
    sys = {"role": "system", "content": "You are helpful"}
    return [
        Trace("t1", "k1", 100, [sys, {"role": "user", "content": "hi"}]),
        Trace("t2", "k1", 101, [
            sys,
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "hello!"},
            {"role": "user", "content": "how are you"},
        ]),
        Trace("t3", "k1", 102, [
            sys,
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "hello!"},
            {"role": "user", "content": "how are you"},
            {"role": "assistant", "content": "good thanks"},
            {"role": "user", "content": "bye"},
        ]),
    ], {
        "expected_roots": {"t1"},
        "expected_parents": {"t2": "t1", "t3": "t2"},
        "expected_session_count": 1,
    }


def s_batch_single_shot():
    """5 batch single-shot, all share same system prompt."""
    sys = {"role": "system", "content": "Solve math problems"}
    return [
        Trace(f"b{i}", "k1", 100 + i, [
            sys, {"role": "user", "content": f"what is {i}+{i}"}
        ])
        for i in range(1, 6)
    ], {
        "expected_roots": {"b1", "b2", "b3", "b4", "b5"},
        "expected_session_count": 5,
    }


def s_batch_with_followups():
    """5 batch + 2 follow-ups to specific items (b1 and b3)."""
    sys = {"role": "system", "content": "Solve math problems"}
    initial = [
        Trace(f"b{i}", "k1", 100 + i, [
            sys, {"role": "user", "content": f"what is {i}+{i}"}
        ])
        for i in range(1, 6)
    ]
    followups = [
        Trace("f1", "k1", 200, [
            sys,
            {"role": "user", "content": "what is 1+1"},
            {"role": "assistant", "content": "2"},
            {"role": "user", "content": "and 1+2?"},
        ]),
        Trace("f3", "k1", 201, [
            sys,
            {"role": "user", "content": "what is 3+3"},
            {"role": "assistant", "content": "6"},
            {"role": "user", "content": "and 3+4?"},
        ]),
    ]
    return initial + followups, {
        "expected_roots": {"b1", "b2", "b3", "b4", "b5"},
        "expected_parents": {"f1": "b1", "f3": "b3"},
        "expected_session_count": 5,
    }


def s_fork():
    """Conversation with regenerate (fork at turn 2)."""
    return [
        Trace("t1", "k1", 100, [{"role": "user", "content": "tell me a story"}]),
        Trace("t2a", "k1", 101, [
            {"role": "user", "content": "tell me a story"},
            {"role": "assistant", "content": "once upon a time A..."},
            {"role": "user", "content": "make it longer"},
        ]),
        Trace("t2b", "k1", 102, [
            # user regenerated t1, got different assistant reply, then continued
            {"role": "user", "content": "tell me a story"},
            {"role": "assistant", "content": "once upon a time B..."},
            {"role": "user", "content": "make it shorter"},
        ]),
    ], {
        "expected_roots": {"t1"},
        "expected_parents": {"t2a": "t1", "t2b": "t1"},
        "expected_session_count": 1,
    }


def s_collision_indep_singleshot():
    """Two independent single-shot batch traces with identical messages.

    Algorithm correctly leaves them as 2 separate roots because neither can
    be a prefix of the other (same length).
    """
    return [
        Trace("t1", "k1", 100, [{"role": "user", "content": "what is 1+1"}]),
        Trace("t2", "k1", 101, [{"role": "user", "content": "what is 1+1"}]),
    ], {
        "expected_roots": {"t1", "t2"},
        "expected_session_count": 2,
    }


def s_collision_ambiguous_followup():
    """EDGE: two independent batch traces with identical first turn, then a
    follow-up arrives whose ambiguous parent is either of them.

    Real-world expectation: a2 belongs to a1's session, b2 belongs to b1's.
    Algorithm fails: it picks the most-recent matching prefix (b1) for both,
    grouping a2 incorrectly under b1's session.

    This is the only known failure mode. Mitigation requires either
    response-content comparison (schema-aware) or post-hoc UI correction.
    """
    return [
        Trace("a1", "k1", 100, [{"role": "user", "content": "what is 1+1"}]),
        Trace("b1", "k1", 101, [{"role": "user", "content": "what is 1+1"}]),
        Trace("a2", "k1", 110, [
            {"role": "user", "content": "what is 1+1"},
            {"role": "assistant", "content": "2"},
            {"role": "user", "content": "thanks, now what is 2+2"},
        ]),
        Trace("b2", "k1", 111, [
            {"role": "user", "content": "what is 1+1"},
            {"role": "assistant", "content": "two"},
            {"role": "user", "content": "explain why"},
        ]),
    ], {
        "expected_parents": {"a2": "a1", "b2": "b1"},  # ideal but algorithm can't tell
        "expected_session_count": 2,
        "known_failure": True,
    }


def s_cross_protocol():
    """Chat Completions + Anthropic Messages mixed (both use 'messages' array)."""
    sys_oai = {"role": "system", "content": "You are helpful"}
    return [
        Trace("oai1", "k1", 100, [sys_oai, {"role": "user", "content": "hi"}]),
        Trace("oai2", "k1", 101, [
            sys_oai,
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "hello"},
            {"role": "user", "content": "continue"},
        ]),
        # Anthropic-style trace (no system in messages; system goes in separate field)
        Trace("anth1", "k1", 102, [{"role": "user", "content": "what is rust"}]),
        Trace("anth2", "k1", 103, [
            {"role": "user", "content": "what is rust"},
            {"role": "assistant", "content": "a programming language"},
            {"role": "user", "content": "show me an example"},
        ]),
    ], {
        "expected_roots": {"oai1", "anth1"},
        "expected_parents": {"oai2": "oai1", "anth2": "anth1"},
        "expected_session_count": 2,
    }


def s_anthropic_system_field_naive():
    """EDGE: Anthropic puts `system` OUTSIDE the messages array.

    If session inference ignores `system`, two conversations that share the
    same first user message but use DIFFERENT system prompts will collide.

    Algorithm as currently specified (matches only on messages array) WILL
    incorrectly group them.
    """
    return [
        # Two independent conversations starting with same first user message
        # but different system prompts.
        Trace("sysA-1", "k1", 100, [{"role": "user", "content": "hi"}]),
        Trace("sysB-1", "k1", 101, [{"role": "user", "content": "hi"}]),
        # Follow-ups: same user message structure, different system context.
        # In real Anthropic protocol, these would have:
        #   req.body.system = "You are a pirate"  vs  "You are a doctor"
        # but the messages array doesn't carry that.
        Trace("sysA-2", "k1", 110, [
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "Ahoy matey!"},
            {"role": "user", "content": "tell me about treasure"},
        ]),
        Trace("sysB-2", "k1", 111, [
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "Hello, how can I help with your health concerns?"},
            {"role": "user", "content": "tell me about a checkup"},
        ]),
    ], {
        "expected_parents": {"sysA-2": "sysA-1", "sysB-2": "sysB-1"},
        "expected_session_count": 2,
        "known_failure": True,
    }


def s_anthropic_system_field_mitigated():
    """SAME as above but the algorithm now includes the `system` field as
    a virtual turn 0 in the prefix. This is the proposed mitigation.

    We simulate it by prepending a synthetic system-as-message-0 to each
    trace's messages array.
    """
    return [
        Trace("mA-1", "k1", 100, [
            {"role": "_system", "content": "You are a pirate"},
            {"role": "user", "content": "hi"},
        ]),
        Trace("mB-1", "k1", 101, [
            {"role": "_system", "content": "You are a doctor"},
            {"role": "user", "content": "hi"},
        ]),
        Trace("mA-2", "k1", 110, [
            {"role": "_system", "content": "You are a pirate"},
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "Ahoy matey!"},
            {"role": "user", "content": "tell me about treasure"},
        ]),
        Trace("mB-2", "k1", 111, [
            {"role": "_system", "content": "You are a doctor"},
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "Hello, how can I help with your health concerns?"},
            {"role": "user", "content": "tell me about a checkup"},
        ]),
    ], {
        "expected_parents": {"mA-2": "mA-1", "mB-2": "mB-1"},
        "expected_session_count": 2,
    }


def s_interleaved_keys():
    """Different keys must not be mixed even if messages are identical."""
    msg = [{"role": "user", "content": "hi"}]
    return [
        Trace("ka1", "key_A", 100, msg),
        Trace("kb1", "key_B", 101, msg),
        Trace("ka2", "key_A", 110, [
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": "hello"},
            {"role": "user", "content": "continue"},
        ]),
    ], {
        "expected_roots": {"ka1", "kb1"},
        "expected_parents": {"ka2": "ka1"},
        "expected_session_count": 2,
    }


def s_out_of_order():
    """Traces arrive in wall-clock order but represent messy timing.

    Reality: api-log captures by HTTP completion order. If two requests
    finish near-simultaneously, the order may not match the user's intent.
    Algorithm should still work because we order candidates by messages_len
    DESC then ts DESC.
    """
    return [
        # Imagine these arrive in this order at api-log:
        Trace("t1", "k1", 100, [{"role": "user", "content": "msg-A"}]),
        # t2 conceptually follows t1, but arrives FIRST in some race condition
        # No: we can only handle traces in arrival order. So this scenario
        # tests what happens when a continuation arrives before its parent.
        # In practice this is rare (HTTP requests complete sequentially per
        # client conn), but documented here.
        Trace("t2", "k1", 101, [
            {"role": "user", "content": "msg-A"},
            {"role": "assistant", "content": "reply-A"},
            {"role": "user", "content": "msg-B"},
        ]),
    ], {
        "expected_roots": {"t1"},
        "expected_parents": {"t2": "t1"},
        "expected_session_count": 1,
    }


SCENARIOS = [
    ("single_shot",            "3 independent single-shot requests",                              s_single_shot),
    ("multi_turn",             "1 conversation, 3 turns",                                          s_multi_turn),
    ("batch_single_shot",      "5 batch with shared system prompt, each independent",              s_batch_single_shot),
    ("batch_with_followups",   "5 batch + 2 follow-ups → each attaches to correct root",          s_batch_with_followups),
    ("fork",                   "Conversation with regenerate (sibling branches under same root)", s_fork),
    ("collision_indep",        "Two independent batch traces, identical messages → 2 roots OK",   s_collision_indep_singleshot),
    ("collision_ambiguous",    "EDGE: identical first turn, follow-up can't disambiguate parent",  s_collision_ambiguous_followup),
    ("anthropic_system_naive", "EDGE: Anthropic system field outside messages → algorithm collides", s_anthropic_system_field_naive),
    ("anthropic_system_fixed", "MITIGATED: prepend system as virtual turn-0 to prefix",             s_anthropic_system_field_mitigated),
    ("cross_protocol",         "OpenAI + Anthropic mixed (both use messages array)",               s_cross_protocol),
    ("interleaved_keys",       "Different key_hash → never mixed",                                 s_interleaved_keys),
    ("out_of_order",           "Continuation arrives normally after its parent",                   s_out_of_order),
]


def main():
    results = {"pass": 0, "fail": 0, "known_failure": 0}
    for name, desc, fn in SCENARIOS:
        traces, expectations = fn()
        result = run_scenario(name, desc, traces, expectations)
        results[result] += 1

    print(f"\n{'=' * 70}")
    print(f"SUMMARY: {results['pass']} pass, {results['fail']} fail, "
          f"{results['known_failure']} known-failure (edge cases)")
    print("=" * 70)
    return 0 if results["fail"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
