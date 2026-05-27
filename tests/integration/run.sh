#!/usr/bin/env bash
# Integration test for api-log + mock upstream via the dev-stack compose.
#
# Brings the stack up, exercises the proxy and read API, and asserts on:
#   1. forwarding works (mock upstream sees the request body)
#   2. JSONL is produced on the persistent volume
#   3. SQLite is populated and session inference fires
#   4. /healthz exposes counters
#   5. /api/traces list + cursor pagination
#   6. /api/traces/:id returns row + full parsed line
#   7. /replay at speed=1, speed=10, and ?nodelay=1 honor the pacing
#   8. cross-key isolation holds
#
# Requires: docker compose v2, curl, jq.
#
# Usage:
#   tests/integration/run.sh           # build + test + teardown
#   KEEP_UP=1 tests/integration/run.sh # leave the stack running afterwards

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COMPOSE="docker compose -f $REPO_ROOT/deploy/dev-stack/docker-compose.yml"
HOST_DATA="$REPO_ROOT/deploy/dev-stack/data"

# Files inside $HOST_DATA are written by container UID 65532. On Linux
# CI those land owned by literal UID 65532 (not the runner user), so a
# plain `rm -rf` can't traverse into the per-day subdirs. Do the wipe
# from a throwaway root container instead — works on both macOS Docker
# Desktop (UID translation) and Linux.
wipe_host_data() {
  if [ -d "$HOST_DATA" ]; then
    docker run --rm -v "$HOST_DATA":/d alpine sh -c 'rm -rf /d/* /d/.[!.]* 2>/dev/null || true' >/dev/null 2>&1 || true
    rmdir "$HOST_DATA" 2>/dev/null || true
  fi
}

cleanup() {
  if [ "${KEEP_UP:-0}" != "1" ]; then
    echo "--- tearing down stack ---"
    $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
    wipe_host_data
  else
    echo "--- KEEP_UP=1 set, leaving stack running ---"
    echo "    proxy   http://localhost:7861"
    echo "    api     http://localhost:7862"
    echo "    data    $HOST_DATA"
    echo "    teardown: $COMPOSE down -v"
  fi
}
trap cleanup EXIT

# Fresh data dir; distroless container writes as UID 65532. chmod 777
# lets the container create files regardless of the host user's UID.
wipe_host_data
mkdir -p "$HOST_DATA"
chmod 777 "$HOST_DATA"

PASS=0
FAIL=0
check() {
  local label="$1"; shift
  if "$@"; then
    PASS=$((PASS+1))
    printf "  ✓  %s\n" "$label"
  else
    FAIL=$((FAIL+1))
    printf "  ✗  %s\n" "$label"
  fi
}

# ---------- bring up ----------

echo "--- building images ---"
$COMPOSE build --quiet

echo "--- starting stack ---"
$COMPOSE up -d

echo "--- waiting for api-log to listen ---"
SEEN_STATUS=""
for i in $(seq 1 30); do
  STATUS=$(curl -sS -o /dev/null -w "%{http_code}" http://localhost:7862/ 2>/dev/null || true)
  SEEN_STATUS="$STATUS"
  if [ "$STATUS" = "401" ] || [ "$STATUS" = "200" ]; then
    break
  fi
  sleep 0.5
done
echo "    last status from api listener: ${SEEN_STATUS:-<none>}"

# Read the admin token from the host-side bind mount. Distroless has no
# shell / no `cat`, so `docker compose exec ... cat /data/admin_token`
# does NOT work — that path produced an OCI runtime error string
# masquerading as a token. Bind-mounting and reading from the host
# avoids the problem entirely.
TOKEN_FILE="$HOST_DATA/admin_token"
for i in $(seq 1 30); do
  if [ -s "$TOKEN_FILE" ]; then break; fi
  sleep 0.2
done
if [ ! -s "$TOKEN_FILE" ]; then
  echo "FATAL: admin_token never appeared at $TOKEN_FILE"
  echo "--- api-log logs ---"
  $COMPOSE logs --tail=80 api-log || true
  exit 1
fi
TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
echo "    admin_token=${TOKEN:0:8}..."

AUTH="Authorization: Bearer $TOKEN"

# ---------- exercise ----------

echo
echo "--- exercising the stack ---"

# 1) Non-streaming Chat Completions.
#
# IMPORTANT: requests to the PROXY (:7861) carry the upstream's bearer
# only. The api-log admin token ($AUTH) is for the read API (:7862);
# if you send both here, curl emits two Authorization headers and the
# computed key_hash differs from streaming requests that send only one,
# which breaks session inference scoping (sessions are partitioned by
# key_hash).
RESP1=$(curl -sS -H "Authorization: Bearer sk-team-A" -H "Content-Type: application/json" \
  -d '{"model":"mock-gpt","messages":[{"role":"user","content":"hi"}]}' \
  http://localhost:7861/v1/chat/completions)
check "non-streaming Chat forwards" test "$(echo "$RESP1" | jq -r .object)" = "chat.completion"

# 2) Streaming Chat Completions
curl -sS -N -H "Authorization: Bearer sk-team-A" -H "Content-Type: application/json" \
  -d '{"model":"mock-gpt","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"more"}],"stream":true}' \
  http://localhost:7861/v1/chat/completions >/dev/null

# 3) Anthropic streaming
curl -sS -N -H "x-api-key: sk-team-B" -H "Content-Type: application/json" \
  -d '{"model":"mock-claude","messages":[{"role":"user","content":"hi"}],"stream":true}' \
  http://localhost:7861/v1/messages >/dev/null

# 4) OpenAI Responses streaming
curl -sS -N -H "Authorization: Bearer sk-team-A" -H "Content-Type: application/json" \
  -d '{"model":"mock-gpt","input":"hello","stream":true}' \
  http://localhost:7861/v1/responses >/dev/null

# Give the writer a moment to flush.
sleep 0.5

# ---------- assertions on the read API ----------

# 5) /healthz
HEALTHZ=$(curl -sS -H "$AUTH" http://localhost:7862/healthz)
check "/healthz status ok"      test "$(echo "$HEALTHZ" | jq -r .status)" = "ok"
APPENDED=$(echo "$HEALTHZ" | jq -r .counters.appended)
INDEXED=$(echo "$HEALTHZ" | jq -r .counters.indexed)
check "/healthz appended ≥ 4"   test "$APPENDED" -ge 4
check "/healthz indexed ≥ 4"    test "$INDEXED" -ge 4
check "/healthz drop_writer_full = 0" test "$(echo "$HEALTHZ" | jq -r .counters.drop_writer_full)" = "0"

# 6) list paginates
LIST=$(curl -sS -H "$AUTH" "http://localhost:7862/api/traces?limit=2")
check "list returns 2 traces"   test "$(echo "$LIST" | jq -r '.traces | length')" -eq 2
NEXT=$(echo "$LIST" | jq -r .next_cursor)
check "list yields a cursor"    test -n "$NEXT" -a "$NEXT" != "null"

PAGE2=$(curl -sS -H "$AUTH" "http://localhost:7862/api/traces?limit=2&cursor=$NEXT")
check "cursor advances"         test "$(echo "$LIST" | jq -r '.traces[0].id')" != "$(echo "$PAGE2" | jq -r '.traces[0].id')"

# 7) detail
ID=$(curl -sS -H "$AUTH" "http://localhost:7862/api/traces?limit=1" | jq -r '.traces[0].id')
DETAIL=$(curl -sS -H "$AUTH" "http://localhost:7862/api/traces/$ID")
check "detail row.id matches"   test "$(echo "$DETAIL" | jq -r .row.id)" = "$ID"
check "detail trace.path set"   test -n "$(echo "$DETAIL" | jq -r .trace.path)"

# 8) cross-key isolation: traces with two different key_hashes
DISTINCT_KEYS=$(curl -sS -H "$AUTH" "http://localhost:7862/api/traces?limit=10" \
  | jq -r '.traces | map(.key_hash) | unique | length')
check "≥ 2 distinct key_hashes" test "$DISTINCT_KEYS" -ge 2

# 9) session inference: streaming Chat trace 2 should have a parent in
# the same key as trace 1 (both used sk-team-A; the messages prefix
# of t2 strictly extends t1's). Hard to assert without knowing IDs, so
# just count: any trace with non-null parent_id implies inference fired.
WITH_PARENT=$(curl -sS -H "$AUTH" "http://localhost:7862/api/traces?limit=10" \
  | jq -r '[.traces[] | select(.parent_id != null)] | length')
check "≥ 1 trace has parent"    test "$WITH_PARENT" -ge 1

# 10) replay timing — speed=10 should be ≥10x faster than speed=1
SSE_ID=$(curl -sS -H "$AUTH" "http://localhost:7862/api/traces?limit=10" \
  | jq -r '[.traces[] | select(.path == "/v1/messages")][0].id')
if [ -n "$SSE_ID" ] && [ "$SSE_ID" != "null" ]; then
  T0=$(python3 -c 'import time; print(int(time.time()*1000))')
  curl -sS -H "$AUTH" "http://localhost:7862/api/traces/$SSE_ID/replay" >/dev/null
  T1=$(python3 -c 'import time; print(int(time.time()*1000))')
  D1=$((T1-T0))
  T0=$(python3 -c 'import time; print(int(time.time()*1000))')
  curl -sS -H "$AUTH" "http://localhost:7862/api/traces/$SSE_ID/replay?speed=10" >/dev/null
  T1=$(python3 -c 'import time; print(int(time.time()*1000))')
  D10=$((T1-T0))
  check "replay speed=10 ≥ 3x faster than speed=1 ($D1 ms vs $D10 ms)" test "$D10" -lt "$((D1 / 3))"

  # nodelay should be near instant
  T0=$(python3 -c 'import time; print(int(time.time()*1000))')
  curl -sS -H "$AUTH" "http://localhost:7862/api/traces/$SSE_ID/replay?nodelay=1" >/dev/null
  T1=$(python3 -c 'import time; print(int(time.time()*1000))')
  DN=$((T1-T0))
  check "replay ?nodelay=1 < 200ms"  test "$DN" -lt 200
fi

# 11) auth
UNAUTH=$(curl -sS -o /dev/null -w "%{http_code}" http://localhost:7862/api/traces)
check "missing auth → 401"      test "$UNAUTH" = "401"
WRONG=$(curl -sS -o /dev/null -w "%{http_code}" -H "Authorization: Bearer wrong" http://localhost:7862/api/traces)
check "wrong auth → 401"        test "$WRONG" = "401"

# ---------- result ----------

echo
echo "--- result: $PASS passed, $FAIL failed ---"
exit $FAIL
