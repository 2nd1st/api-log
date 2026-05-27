#!/usr/bin/env bash
# Real-upstream bench for api-log: concurrency + latency + write rate.
#
# Brings up the bench stack pointed at a real LLM gateway, runs the
# Go bench tool through the proxy, and reports:
#   - latency percentiles (overall + per-protocol)
#   - counter accounting: sent == /healthz.appended == /healthz.indexed
#   - drop_writer_full / drop_capture_full / truncated_* (must be 0)
#   - disk write: data/ size before/after + bytes/trace
#
# Required env:
#   APILOG_PROXY_UPSTREAM   real upstream URL (e.g. http://cpa.homelab.lan)
#   APILOG_KEY              bearer key to send to that upstream
#
# Optional env:
#   BENCH_CONC              default 50
#   BENCH_COUNT             default 20
#   BENCH_PROTOCOLS         default chat-nostream,chat-stream,anthropic-stream,responses-stream
#   BENCH_CHAT_MODEL        default gpt-4o-mini
#   BENCH_ANTHROPIC_MODEL   default claude-haiku-4-5
#   BENCH_RESPONSES_MODEL   default gpt-4o-mini
#   KEEP_UP=1               leave the stack up after the run

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COMPOSE_FILE="$REPO_ROOT/deploy/bench/docker-compose.yml"
COMPOSE="docker compose -f $COMPOSE_FILE"
HOST_DATA="$REPO_ROOT/deploy/bench/data"
BENCH_BIN="$REPO_ROOT/tools/bench/bench-bin"
SUMMARY_JSON="$REPO_ROOT/deploy/bench/last-run.json"

: "${APILOG_PROXY_UPSTREAM:?set APILOG_PROXY_UPSTREAM to your upstream URL}"
: "${APILOG_KEY:?set APILOG_KEY to the upstream bearer key}"

BENCH_CONC="${BENCH_CONC:-50}"
BENCH_COUNT="${BENCH_COUNT:-20}"
BENCH_PROTOCOLS="${BENCH_PROTOCOLS:-chat-nostream,chat-stream,anthropic-stream,responses-stream}"
BENCH_CHAT_MODEL="${BENCH_CHAT_MODEL:-gpt-4o-mini}"
BENCH_ANTHROPIC_MODEL="${BENCH_ANTHROPIC_MODEL:-claude-haiku-4-5}"
BENCH_RESPONSES_MODEL="${BENCH_RESPONSES_MODEL:-gpt-4o-mini}"

export APILOG_PROXY_UPSTREAM

# ---------- helpers ----------

# Wipe host data via a throwaway root container: files written by the
# distroless nonroot UID (65532) can be unowned by the host user on
# Linux CI; doing it from inside a container sidesteps that.
wipe_host_data() {
  if [ -d "$HOST_DATA" ]; then
    docker run --rm -v "$HOST_DATA":/d alpine sh -c 'rm -rf /d/* /d/.[!.]* 2>/dev/null || true' >/dev/null 2>&1 || true
    rmdir "$HOST_DATA" 2>/dev/null || true
  fi
}

cleanup() {
  if [ "${KEEP_UP:-0}" != "1" ]; then
    echo "--- tearing down bench stack ---"
    $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
    wipe_host_data
  else
    echo "--- KEEP_UP=1 set, leaving stack running ---"
    echo "    proxy   http://localhost:7861"
    echo "    api     http://localhost:7862"
    echo "    data    $HOST_DATA"
  fi
}
trap cleanup EXIT

du_bytes() {
  # `du -sk` is portable; convert KiB → bytes.
  if [ -d "$1" ]; then
    local kib
    kib=$(du -sk "$1" 2>/dev/null | awk '{print $1}')
    echo $((kib * 1024))
  else
    echo 0
  fi
}

# ---------- build bench binary on host ----------

echo "--- building bench tool ---"
if ! command -v go >/dev/null 2>&1; then
  # No go on host; build inside docker.
  docker run --rm -v "$REPO_ROOT":/src -w /src golang:1.22-alpine \
    sh -c 'CGO_ENABLED=0 go build -o tools/bench/bench-bin ./tools/bench'
else
  ( cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o tools/bench/bench-bin ./tools/bench )
fi
chmod +x "$BENCH_BIN"

# ---------- bring up ----------

wipe_host_data
mkdir -p "$HOST_DATA"
chmod 777 "$HOST_DATA"

echo "--- building images ---"
$COMPOSE build --quiet

echo "--- starting stack (upstream=$APILOG_PROXY_UPSTREAM) ---"
$COMPOSE up -d

echo "--- waiting for api-log to listen ---"
for i in $(seq 1 60); do
  STATUS=$(curl -sS -o /dev/null -w "%{http_code}" http://localhost:7862/ 2>/dev/null || true)
  if [ "$STATUS" = "401" ] || [ "$STATUS" = "200" ]; then break; fi
  sleep 0.5
done

TOKEN_FILE="$HOST_DATA/admin_token"
for i in $(seq 1 60); do
  if [ -s "$TOKEN_FILE" ]; then break; fi
  sleep 0.2
done
if [ ! -s "$TOKEN_FILE" ]; then
  echo "FATAL: admin_token never appeared at $TOKEN_FILE"
  $COMPOSE logs --tail=80 api-log || true
  exit 1
fi
TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
AUTH="Authorization: Bearer $TOKEN"
echo "    admin_token=${TOKEN:0:8}..."

# ---------- snapshot BEFORE ----------

echo "--- baseline (before bench) ---"
BEFORE_HEALTHZ=$(curl -sS -H "$AUTH" http://localhost:7862/healthz)
BEFORE_APPENDED=$(echo "$BEFORE_HEALTHZ" | jq -r .counters.appended)
BEFORE_INDEXED=$(echo "$BEFORE_HEALTHZ" | jq -r .counters.indexed)
BEFORE_DROP_WRITER=$(echo "$BEFORE_HEALTHZ" | jq -r .counters.drop_writer_full)
BEFORE_DROP_CAPTURE=$(echo "$BEFORE_HEALTHZ" | jq -r .counters.drop_capture_full)
BEFORE_BYTES=$(du_bytes "$HOST_DATA")
echo "    appended=$BEFORE_APPENDED indexed=$BEFORE_INDEXED drop_writer=$BEFORE_DROP_WRITER drop_capture=$BEFORE_DROP_CAPTURE bytes=$BEFORE_BYTES"

# ---------- run bench ----------

echo "--- running bench (conc=$BENCH_CONC count=$BENCH_COUNT) ---"
"$BENCH_BIN" \
  -upstream http://localhost:7861 \
  -key "$APILOG_KEY" \
  -conc "$BENCH_CONC" \
  -count "$BENCH_COUNT" \
  -protocols "$BENCH_PROTOCOLS" \
  -chat-model "$BENCH_CHAT_MODEL" \
  -anthropic-model "$BENCH_ANTHROPIC_MODEL" \
  -responses-model "$BENCH_RESPONSES_MODEL" \
  -json-out "$SUMMARY_JSON"

# Give the writer a moment to flush the SQLite tail.
sleep 1.5

# ---------- snapshot AFTER ----------

echo "--- post-bench /healthz ---"
AFTER_HEALTHZ=$(curl -sS -H "$AUTH" http://localhost:7862/healthz)
AFTER_APPENDED=$(echo "$AFTER_HEALTHZ" | jq -r .counters.appended)
AFTER_INDEXED=$(echo "$AFTER_HEALTHZ" | jq -r .counters.indexed)
AFTER_DROP_WRITER=$(echo "$AFTER_HEALTHZ" | jq -r .counters.drop_writer_full)
AFTER_DROP_CAPTURE=$(echo "$AFTER_HEALTHZ" | jq -r .counters.drop_capture_full)
AFTER_TRUNC_REQ=$(echo "$AFTER_HEALTHZ" | jq -r '.counters.truncated_req_total // 0')
AFTER_TRUNC_RESP=$(echo "$AFTER_HEALTHZ" | jq -r '.counters.truncated_resp_total // 0')
AFTER_BYTES=$(du_bytes "$HOST_DATA")

DELTA_APPENDED=$((AFTER_APPENDED - BEFORE_APPENDED))
DELTA_INDEXED=$((AFTER_INDEXED - BEFORE_INDEXED))
DELTA_DROP_WRITER=$((AFTER_DROP_WRITER - BEFORE_DROP_WRITER))
DELTA_DROP_CAPTURE=$((AFTER_DROP_CAPTURE - BEFORE_DROP_CAPTURE))
DELTA_BYTES=$((AFTER_BYTES - BEFORE_BYTES))

TOTAL_SENT=$((BENCH_CONC * BENCH_COUNT))

echo
echo "--- counter accounting ---"
printf "    sent (bench):        %d\n" "$TOTAL_SENT"
printf "    appended (Δ):        %d\n" "$DELTA_APPENDED"
printf "    indexed (Δ):         %d\n" "$DELTA_INDEXED"
printf "    drop_writer_full Δ:  %d\n" "$DELTA_DROP_WRITER"
printf "    drop_capture_full Δ: %d\n" "$DELTA_DROP_CAPTURE"
printf "    truncated_req:       %s\n" "$AFTER_TRUNC_REQ"
printf "    truncated_resp:      %s\n" "$AFTER_TRUNC_RESP"
printf "    disk bytes written:  %d  (%.2f KB/trace)\n" "$DELTA_BYTES" \
  "$(python3 -c "print($DELTA_BYTES / max($DELTA_APPENDED, 1) / 1024)")"

# ---------- assertions ----------

PASS=0; FAIL=0
check() {
  local label="$1"; shift
  if "$@"; then PASS=$((PASS+1)); printf "  ✓  %s\n" "$label"
  else FAIL=$((FAIL+1)); printf "  ✗  %s\n" "$label"; fi
}

echo
echo "--- assertions ---"
check "appended matches sent ($DELTA_APPENDED == $TOTAL_SENT)" test "$DELTA_APPENDED" -eq "$TOTAL_SENT"
check "indexed matches sent ($DELTA_INDEXED == $TOTAL_SENT)"   test "$DELTA_INDEXED" -eq "$TOTAL_SENT"
check "no writer drops"                                        test "$DELTA_DROP_WRITER" -eq 0
check "no capture drops"                                       test "$DELTA_DROP_CAPTURE" -eq 0
check "disk grew (data/ wrote bytes)"                          test "$DELTA_BYTES" -gt 0
check "JSONL files exist on disk"                              test -n "$(find "$HOST_DATA" -name '*.jsonl' -print -quit)"
check "SQLite index file exists"                               test -f "$HOST_DATA/index.sqlite"

echo
echo "--- result: $PASS passed, $FAIL failed ---"
echo "--- bench JSON summary saved to $SUMMARY_JSON ---"

# Surface streaming SSE error notes if any 4xx/5xx — easy to miss in
# the human-readable summary above when most requests succeed.
ERR_COUNT=$(jq -r '.error_count // 0' "$SUMMARY_JSON")
HTTP4XX=$(jq -r '.http_4xx_count // 0' "$SUMMARY_JSON")
HTTP5XX=$(jq -r '.http_5xx_count // 0' "$SUMMARY_JSON")
if [ "$ERR_COUNT" -gt 0 ] || [ "$HTTP4XX" -gt 0 ] || [ "$HTTP5XX" -gt 0 ]; then
  echo
  echo "NOTE: bench saw $HTTP4XX×4xx, $HTTP5XX×5xx, $ERR_COUNT×transport errors."
  echo "      (Upstream rejections still count as recorded traces — api-log captures the rejection. This is expected during model/rate limit testing.)"
fi

exit $FAIL
