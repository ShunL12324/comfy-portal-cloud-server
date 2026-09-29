#!/usr/bin/env bash
# End-to-end test: the real cpd binary, real aria2c, a stub ComfyUI and fixture
# models over loopback. No network, no GPU, no Python. Needs aria2c, curl, jq.
#
#   test/e2e.sh                # builds cpd and the rig from source
#   CPD=/path/cpd RIG=/path/rig test/e2e.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
TOKEN=e2e-token-abcdefgh
HFTOKEN=hf_e2e_secret_value
free_port() { # a port nothing is listening on
  local p
  while :; do p=$((20000 + RANDOM % 20000)); nc -z 127.0.0.1 "$p" 2>/dev/null || { echo "$p"; return; }; done
}
API_PORT=${API_PORT:-$(free_port)}
COMFY_PORT=${COMFY_PORT:-$(free_port)}
FIX_PORT=${FIX_PORT:-$(free_port)}
BASE=http://127.0.0.1:$API_PORT
CPD_PID=""
PIDS=()

cleanup() {
  if [ -n "$CPD_PID" ]; then kill "$CPD_PID" 2>/dev/null || true; wait "$CPD_PID" 2>/dev/null || true; fi
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && { kill "$p" 2>/dev/null; wait "$p" 2>/dev/null; } || true; done
  [ -n "${KEEP:-}" ] && echo "kept $WORK" || rm -rf "$WORK"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*"; echo "--- supervisor log"; tail -40 "$WORK/ws/supervisor.log" 2>/dev/null || true; exit 1; }
api() { curl -fsS -H "Authorization: Bearer $TOKEN" "$@"; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
acode() { code -H "Authorization: Bearer $TOKEN" "$@"; }
wait_for() { # <description> <timeout-s> <command...>
  local what=$1 t=$2; shift 2
  for _ in $(seq 1 $((t * 5))); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done
  fail "timed out waiting for $what"
}
phase_is() { [ "$(api "$BASE/v2/state" | jq -r .phase)" = "$1" ]; }

if [ -z "${CPD:-}" ]; then (cd "$ROOT" && go build -o "$WORK/cpd" ./cmd/cpd); CPD=$WORK/cpd; fi
if [ -z "${RIG:-}" ]; then (cd "$ROOT" && go build -o "$WORK/rig" ./test/rig); RIG=$WORK/rig; fi

step "fixtures and a fake ComfyUI checkout"
mkdir -p "$WORK/fixtures" "$WORK/ws" "$WORK/comfy/venv/bin"
"$RIG" fixtures --dir "$WORK/fixtures" --port "$FIX_PORT" tiny.safetensors=8 second.safetensors=1 &
PIDS+=($!)
printf '#!/bin/sh\nexec %s comfy "$@"\n' "$RIG" > "$WORK/comfy/venv/bin/python"
chmod +x "$WORK/comfy/venv/bin/python"
wait_for "fixtures" 10 curl -fs "http://127.0.0.1:$FIX_PORT/second.safetensors" -o /dev/null

MANIFEST=$(jq -nc --arg u "http://127.0.0.1:$FIX_PORT" '{version:1,models:[
  {url:($u+"/tiny.safetensors"),folder:"checkpoints",filename:"t.safetensors"},
  {url:($u+"/second.safetensors"),folder:"loras",filename:"s.safetensors"},
  {url:($u+"/missing.safetensors"),folder:"loras",filename:"gone.safetensors"}],
  extensions:[],ollamaModels:[]}' | base64 | tr -d '\n')

start_cpd() {
  CP_TOKEN=$TOKEN HF_TOKEN=$HFTOKEN CP_MANIFEST=$MANIFEST CP_WORKSPACE=$WORK/ws COMFY_DIR=$WORK/comfy \
    COMFY_PORT=$COMFY_PORT CP_PORT=$API_PORT CP_LISTEN=127.0.0.1:$API_PORT "$CPD" serve >"$WORK/cpd.out" 2>&1 &
  CPD_PID=$!
}
start_cpd

step "health answers before anything else, unauthenticated"
wait_for "health" 10 curl -fs "$BASE/v2/health"

step "boots to ready even though one model 404s"
wait_for "ready" 60 phase_is ready
SNAP=$(api "$BASE/v2/snapshot")
echo "$SNAP" | jq -e '.models | length == 3' >/dev/null || fail "expected 3 models: $SNAP"
echo "$SNAP" | jq -e '[.models[] | select(.state=="done")] | length == 2' >/dev/null || fail "expected 2 done"
FAILED=$(echo "$SNAP" | jq -r '.models[] | select(.state=="error") | .id')
[ -n "$FAILED" ] || fail "the 404 model should be in error"
echo "$SNAP" | jq -e '.models[] | select(.state=="error") | .errorCode == "model_not_found"' >/dev/null || fail "error not classified"
[ "$(sha256sum "$WORK/ws/models/checkpoints/t.safetensors" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$WORK/ws/models/checkpoints/t.safetensors" | cut -d' ' -f1)" = \
  "$(sha256sum "$WORK/fixtures/tiny.safetensors" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$WORK/fixtures/tiny.safetensors" | cut -d' ' -f1)" ] || fail "downloaded file differs from the source"

step "auth"
[ "$(code "$BASE/v2/state")" = 401 ] || fail "unauthenticated /v2/state must be 401"
[ "$(code "$BASE/v1/status")" = 401 ] || fail "unauthenticated /v1/status must be 401"

step "v1 shim still answers with the legacy shape"
api "$BASE/v1/status" | jq -e '.supervisorVersion == 2 and .phase == "ready" and (.models|length==3) and (.services|has("comfyui"))' >/dev/null || fail "v1 shape"

step "retrying the missing model fails again, cleanly"
[ "$(acode -X POST "$BASE/v2/models/$FAILED/retry")" = 202 ] || fail "retry should be accepted"
wait_for "model back in error" 20 bash -c "curl -fs -H 'Authorization: Bearer $TOKEN' $BASE/v2/models/$FAILED | jq -e '.state==\"error\"'"

step "killing ComfyUI brings it back with restarts incremented"
PID=$(api "$BASE/v2/services/comfyui" | jq -r .pid)
kill -9 "$PID"
wait_for "comfyui restart" 30 bash -c "curl -fs -H 'Authorization: Bearer $TOKEN' $BASE/v2/services/comfyui | jq -e '.restarts>=1 and .state==\"running\"'"

step "restart through the API"
[ "$(acode -X POST "$BASE/v2/services/comfyui/restart")" = 202 ] || fail "restart"
[ "$(acode -X POST "$BASE/v2/services/aria2/restart")" = 409 ] || fail "aria2 must not be restartable"
wait_for "comfyui running again" 30 bash -c "curl -fs -H 'Authorization: Bearer $TOKEN' $BASE/v2/services/comfyui | jq -e '.restarts>=2 and .state==\"running\"'"

step "applying a manifest adds a model without a relaunch"
BODY=$(jq -nc --arg u "http://127.0.0.1:$FIX_PORT" '{models:[{url:($u+"/second.safetensors"),folder:"loras",filename:"extra.safetensors"}]}')
[ "$(acode -X PUT -H 'Content-Type: application/json' -d "$BODY" "$BASE/v2/manifest")" = 202 ] || fail "put manifest"
wait_for "extra model done" 30 bash -c "curl -fs -H 'Authorization: Bearer $TOKEN' $BASE/v2/models | jq -e '[.items[]|select(.name==\"extra.safetensors\" and .state==\"done\")]|length==1'"

step "events: a fresh client gets a snapshot first"
FIRST=$(curl -sN -m 2 -H "Authorization: Bearer $TOKEN" "$BASE/v2/events" | head -3 || true)
echo "$FIRST" | grep -q '^event: snapshot' || fail "first SSE event should be a snapshot: $FIRST"

step "no secret reaches any log stream"
for s in supervisor comfyui aria2; do
  LOG=$(api "$BASE/v2/logs/$s?tail=500")
  case $LOG in *"$TOKEN"*|*"$HFTOKEN"*) fail "secret leaked in $s log" ;; esac
done
LOG=$(api "$BASE/v1/log?stream=supervisor&tail=500")
case $LOG in *"$TOKEN"*) fail "secret leaked in v1 log" ;; esac

step "the CLI reads the same state"
OUT=$(CP_TOKEN=$TOKEN CP_PORT=$API_PORT "$CPD" status) || fail "cpd status"
case $OUT in *phase=ready*) ;; *) fail "cpd status output: $OUT" ;; esac
CP_TOKEN=$TOKEN CP_PORT=$API_PORT "$CPD" health || fail "cpd health"

step "a restart resumes: complete files are skipped, not re-downloaded"
kill -TERM "$CPD_PID"; wait "$CPD_PID" 2>/dev/null || true; CPD_PID=""
! curl -fs "$BASE/v2/health" >/dev/null 2>&1 || fail "port should be closed after shutdown"
pgrep -f "aria2c.*rpc-listen-port=6800" >/dev/null && fail "aria2 outlived the supervisor" || true
start_cpd
wait_for "ready after restart" 60 phase_is ready
grep -q 'skip, already on disk' "$WORK/ws/supervisor.log" || fail "second boot should skip files already on disk"

echo; echo "e2e passed"
