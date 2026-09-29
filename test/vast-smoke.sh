#!/usr/bin/env bash
# Smoke-test the published runtime image on a real, cheap vast.ai GPU.
#
# It launches exactly the way the app does (ssh_direct, the app's onstart line,
# CP_* env as -e flags), then drives the supervisor the way the app does too:
# curl on the box's loopback over SSH. The instance is ALWAYS destroyed on exit.
#
#   IMAGE=ghcr.io/shunl12324/comfy-portal-cloud-server:sha-abc1234 test/vast-smoke.sh
#   OFFER=45607706 IMAGE=... test/vast-smoke.sh        # pin an offer
#   KEEP=1 ...                                          # leave it running for debugging (you pay)
#
# Needs: VAST_API_KEY, curl, jq, ssh. Costs a few cents.
set -uo pipefail

: "${VAST_API_KEY:?set VAST_API_KEY}"
: "${IMAGE:?set IMAGE, e.g. ghcr.io/shunl12324/comfy-portal-cloud-server:sha-abc1234}"
API=https://console.vast.ai/api/v0
DISK=${DISK:-80}
MAX_PRICE=${MAX_PRICE:-0.15}
LABEL="cp-smoke:$(date +%s)"
TOKEN=$(uuidgen | tr 'A-Z' 'a-z')
INSTANCE=""
FAILURES=0
T0=$(date +%s)

vast() { curl -fsS -H "Authorization: Bearer $VAST_API_KEY" -H 'Content-Type: application/json' "$@"; }
step() { printf '\n== [%3ss] %s\n' "$(( $(date +%s) - T0 ))" "$*"; }
ok()   { echo "  ok   $*"; }
bad()  { echo "  FAIL $*"; FAILURES=$((FAILURES + 1)); }
check() { # <description> <command...>
  local what=$1; shift
  if "$@" >/dev/null 2>&1; then ok "$what"; else bad "$what"; fi
}

cleanup() {
  local code=$?
  if [ -n "$INSTANCE" ]; then
    if [ -n "${KEEP:-}" ]; then
      echo; echo "KEEP set: instance $INSTANCE is still running and billing. ssh: $SSH_CMD"
    else
      echo; echo "destroying instance $INSTANCE"
      for _ in 1 2 3; do vast -X DELETE "$API/instances/$INSTANCE/" >/dev/null 2>&1 && break; sleep 3; done
      LEFT=$(vast "$API/instances/" 2>/dev/null | jq -r --arg l "$LABEL" '[.instances[]|select(.label==$l)]|length')
      [ "${LEFT:-1}" = 0 ] && echo "destroyed" || echo "WARNING: instance $INSTANCE may still exist, check console.vast.ai"
    fi
  fi
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ---- offer ------------------------------------------------------------------
step "pick an offer"
if [ -z "${OFFER:-}" ]; then
  OFFER=$(vast -X POST "$API/bundles/" -d "$(jq -nc --argjson p "$MAX_PRICE" --argjson d "$DISK" '{
      rentable:{eq:true}, type:"on-demand", num_gpus:{eq:1}, disk_space:{gte:($d+20)},
      reliability2:{gte:0.98}, inet_down:{gte:500}, direct_port_count:{gte:4}, cuda_max_good:{gte:12.8},
      gpu_name:{in:["RTX 3060","RTX 3070","RTX 3080","RTX 3090","RTX 4060","RTX 4070","RTX 4060 Ti","RTX 2060S"]},
      dph_total:{lte:$p}, order:[["dph_total","asc"]], limit:5}')" |
    jq -r '.offers[0] | "\(.id)"')
fi
[ -n "$OFFER" ] && [ "$OFFER" != null ] || { echo "no offer under \$$MAX_PRICE/hr"; exit 1; }
vast -X POST "$API/bundles/" -d "$(jq -nc --argjson id "$OFFER" '{id:{eq:$id}, rentable:{eq:true}}')" |
  jq -r '.offers[0] | "  offer \(.id): \(.gpu_name) \(.gpu_ram/1024|floor)GB  $\(.dph_total)/hr  \(.geolocation)  down \(.inet_down|floor) Mbps  driver \(.driver_version)"'

# ---- launch, exactly like the app ------------------------------------------
step "create the instance (this starts billing)"
MANIFEST=$(jq -nc '{version:1,
  models:[
    {url:"https://huggingface.co/madebyollin/taesd/resolve/main/taesd_decoder.safetensors", folder:"vae_approx", filename:"taesd_decoder.safetensors"},
    {url:"https://huggingface.co/madebyollin/taesd/resolve/main/does-not-exist.safetensors", folder:"vae_approx", filename:"gone.safetensors"}],
  extensions:["https://github.com/pythongosssss/ComfyUI-Custom-Scripts"], ollamaModels:[]}' | base64 | tr -d '\n')
ONSTART=$'mkdir -p /workspace\nnohup /opt/comfyui/venv/bin/python /opt/cp/supervisor.py >> /workspace/supervisor-boot.log 2>&1 &'
ENVSTR="-e CP_TOKEN=$TOKEN -e CP_MANIFEST=$MANIFEST -e COMFY_PORT=8188 -e CP_PORT=8189 -p 8188:8188 -p 8189:8189"
CREATED=$(vast -X PUT "$API/asks/$OFFER/" -d "$(jq -nc --arg image "$IMAGE" --argjson disk "$DISK" --arg label "$LABEL" \
  --arg env "$ENVSTR" --arg onstart "$ONSTART" \
  '{image:$image, disk:$disk, runtype:"ssh_direct", label:$label, env:$env, onstart:$onstart}')") || { echo "create failed"; exit 1; }
INSTANCE=$(echo "$CREATED" | jq -r .new_contract)
[ -n "$INSTANCE" ] && [ "$INSTANCE" != null ] || { echo "no instance id: $CREATED"; exit 1; }
echo "  instance $INSTANCE (label $LABEL)"

# ---- wait for the box -------------------------------------------------------
step "wait for the container (image pull happens here)"
IP=""; SSHPORT=""; PUB8189=""; PUB8188=""
for i in $(seq 1 180); do
  J=$(vast "$API/instances/$INSTANCE/" 2>/dev/null | jq -c '.instances // empty' 2>/dev/null || true)
  STATUS=$(echo "$J" | jq -r '.actual_status // "?"' 2>/dev/null)
  if [ "$STATUS" = running ]; then
    IP=$(echo "$J" | jq -r '.public_ipaddr'); SSHPORT=$(echo "$J" | jq -r '.ports["22/tcp"][0].HostPort // empty')
    PUB8188=$(echo "$J" | jq -r '.ports["8188/tcp"][0].HostPort // empty'); PUB8189=$(echo "$J" | jq -r '.ports["8189/tcp"][0].HostPort // empty')
    [ -n "$SSHPORT" ] && break
  fi
  [ $((i % 6)) = 0 ] && echo "  status=$STATUS  $(echo "$J" | jq -r '.status_msg // ""' | tr -d '\n' | cut -c1-110)"
  case "$STATUS" in exited|offline) bad "instance entered $STATUS"; exit 1;; esac
  sleep 10
done
[ -n "$SSHPORT" ] || { bad "instance never became reachable in 30 min"; exit 1; }
PULL_S=$(( $(date +%s) - T0 ))
SSH_CMD="ssh -p $SSHPORT root@$IP"
SSH=(ssh -p "$SSHPORT" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=20 -o ServerAliveInterval=15 -o LogLevel=ERROR "root@$IP")
for i in $(seq 1 30); do "${SSH[@]}" true 2>/dev/null && break; sleep 5; done
"${SSH[@]}" true 2>/dev/null && ok "ssh works after ${PULL_S}s" || { bad "ssh never worked"; exit 1; }

# The app's transport: curl on the loopback, over SSH.
api() { "${SSH[@]}" "curl -fsS -m 10 -H 'Authorization: Bearer $TOKEN' $* "; }
apicode() { "${SSH[@]}" "curl -s -m 10 -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer $TOKEN' $*"; }
wait_for() { local what=$1 t=$2; shift 2; for _ in $(seq 1 $((t / 3))); do "$@" >/dev/null 2>&1 && return 0; sleep 3; done; bad "timed out waiting for $what"; return 1; }
phase_is() { [ "$(api http://127.0.0.1:8189/v2/state 2>/dev/null | jq -r .phase)" = "$1" ]; }
svc_field() { api "http://127.0.0.1:8189/v2/services/$1" | jq -r ".$2"; }

step "the supervisor answers after onstart runs the compat shim"
wait_for "supervisor health" 120 "${SSH[@]}" "curl -fs -m 5 http://127.0.0.1:8189/v2/health" \
  && ok "/v2/health answers" \
  || { "${SSH[@]}" 'tail -30 /workspace/supervisor-boot.log; ls -la /workspace | head' 2>&1 | sed 's/^/    /'; exit 1; }
check "/v1/health answers (old app builds)" "${SSH[@]}" "curl -fs -m 5 http://127.0.0.1:8189/v1/health"
[ "$(apicode http://127.0.0.1:8189/v2/state)" = 200 ] && ok "authed request works (env reached cpd)" || bad "CP_TOKEN did not reach cpd"
"${SSH[@]}" "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8189/v2/state" | grep -q 401 && ok "unauthenticated request is 401" || bad "unauthenticated request was not 401"

step "boot to ready (real models, real extension, real ComfyUI on the GPU)"
wait_for "ready" 900 phase_is ready
SNAP=$(api http://127.0.0.1:8189/v2/snapshot 2>/dev/null || echo '{}')
echo "$SNAP" | jq -r '"  phase=\(.phase) elapsed=\(.elapsed)s  version=\(.version)"'
echo "$SNAP" | jq -r '.steps[] | "  step \(.id) \(.state) \((.ms // 0)/1000)s"'
[ "$(echo "$SNAP" | jq -r .phase)" = ready ] && ok "phase ready" || { bad "not ready: $(echo "$SNAP" | jq -c .error)"; "${SSH[@]}" 'tail -40 /workspace/supervisor.log' | sed 's/^/    /'; }
echo "$SNAP" | jq -e '[.models[]|select(.name=="taesd_decoder.safetensors" and .state=="done")]|length==1' >/dev/null && ok "real HF model downloaded" || bad "HF model not done"
echo "$SNAP" | jq -e '[.models[]|select(.name=="gone.safetensors" and .state=="error" and .errorCode=="model_not_found")]|length==1' >/dev/null && ok "404 model classified, launch not blocked" || bad "404 model not reported correctly"
"${SSH[@]}" 'test -d /workspace/custom_nodes/ComfyUI-Custom-Scripts' && ok "extension cloned into the workspace" || bad "extension missing"
for n in ComfyUI-Manager comfy-portal-endpoint; do
  "${SSH[@]}" "test -d /opt/comfyui/custom_nodes/$n" && ok "image's $n survived the symlink move" || bad "$n was lost"
done
"${SSH[@]}" 'test -L /opt/comfyui/models && test -L /opt/comfyui/custom_nodes' && ok "ComfyUI dirs are linked into /workspace" || bad "dirs not linked"

step "ComfyUI is really on the GPU"
STATS=$("${SSH[@]}" 'curl -fsS -m 10 http://127.0.0.1:8188/system_stats' 2>/dev/null || echo '{}')
echo "$STATS" | jq -r '.devices[]? | "  device: \(.name)  vram \((.vram_total/1073741824*10|floor)/10) GB"'
echo "$STATS" | jq -e '.devices[0].type=="cuda"' >/dev/null && ok "CUDA device visible to torch" || bad "no CUDA device in /system_stats"
"${SSH[@]}" 'grep -c "Traceback" /workspace/comfyui.log || true' | { read -r n; [ "${n:-0}" = 0 ] && ok "no traceback in comfyui.log" || { bad "$n traceback(s) in comfyui.log"; "${SSH[@]}" 'grep -B2 -A8 Traceback /workspace/comfyui.log | head -40' | sed 's/^/    /'; }; }
"${SSH[@]}" 'curl -fsS -m 10 http://127.0.0.1:8188/object_info' | jq -e 'keys|map(select(test("pysssss")))|length>0' >/dev/null && ok "the cloned extension's nodes loaded" || bad "extension nodes not in /object_info"
"${SSH[@]}" 'nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader; /opt/comfyui/venv/bin/python -c "import torch;print(\"torch\",torch.__version__,\"cuda\",torch.version.cuda,torch.cuda.get_device_name(0))"' 2>&1 | sed 's/^/  /'

step "the same shapes the app parses (v1) and the new API (v2)"
V1=$(api http://127.0.0.1:8189/v1/status 2>/dev/null || echo '{}')
echo "$V1" | jq -e '.supervisorVersion and .phase=="ready" and (.models|length)==2 and (.services|has("comfyui")) and (.steps|type)=="array" and .totals.bytes>=0' >/dev/null && ok "/v1/status keeps the legacy shape" || bad "/v1/status shape"
api http://127.0.0.1:8189/v2/openapi.json | jq -e '.openapi' >/dev/null && ok "/v2/openapi.json served" || bad "openapi"
for s in supervisor comfyui aria2; do
  L=$(api "http://127.0.0.1:8189/v2/logs/$s?tail=300" 2>/dev/null || true)
  case $L in *"$TOKEN"*) bad "token leaked in $s log";; *) ok "no token in $s log (${#L} bytes)";; esac
done
FILE_LEAK=$("${SSH[@]}" "grep -rl '$TOKEN' /workspace/*.log 2>/dev/null | head -3" || true)
[ -z "$FILE_LEAK" ] && ok "token absent from the log files on disk too" || bad "token found on disk in: $FILE_LEAK"

step "public reachability (the ports the app can't rely on, reported not asserted)"
for p in "8189:$PUB8189" "8188:$PUB8188"; do
  code=$(curl -s -m 8 -o /dev/null -w '%{http_code}' "http://$IP:${p#*:}/$( [ "${p%%:*}" = 8189 ] && echo v2/health )" || true)
  echo "  container :${p%%:*} -> $IP:${p#*:} answers HTTP $code"
done

step "resilience"
PID=$(svc_field comfyui pid); "${SSH[@]}" "kill -9 $PID"
wait_for "comfyui to restart after kill -9" 120 bash -c "$(declare -f api svc_field); SSH=(${SSH[*]@Q}); TOKEN=$TOKEN; [ \"\$(svc_field comfyui state)\" = running ] && [ \"\$(svc_field comfyui restarts)\" -ge 1 ]" && ok "kill -9 ComfyUI: supervisor brought it back" || true
[ "$(apicode -X POST http://127.0.0.1:8189/v2/services/comfyui/restart)" = 202 ] && ok "API restart accepted" || bad "API restart"
sleep 5
wait_for "comfyui running after API restart" 120 bash -c "$(declare -f api svc_field); SSH=(${SSH[*]@Q}); TOKEN=$TOKEN; [ \"\$(svc_field comfyui state)\" = running ] && [ \"\$(svc_field comfyui restarts)\" -ge 2 ]" && ok "ComfyUI back after API restart" || true

step "apply a manifest to the running box"
BODY='{"extensions":["https://github.com/pythongosssss/ComfyUI-Custom-Scripts"],"models":[{"url":"https://huggingface.co/madebyollin/taesd/resolve/main/taesdxl_decoder.safetensors","folder":"vae_approx"}]}'
[ "$(apicode -X PUT -H 'Content-Type:application/json' -d "'$BODY'" http://127.0.0.1:8189/v2/manifest)" = 202 ] && ok "PUT /v2/manifest accepted" || bad "PUT /v2/manifest"
wait_for "new model" 120 bash -c "$(declare -f api); SSH=(${SSH[*]@Q}); TOKEN=$TOKEN; api http://127.0.0.1:8189/v2/models | jq -e '[.items[]|select(.name==\"taesdxl_decoder.safetensors\" and .state==\"done\")]|length==1'" && ok "model added without a relaunch" || true

step "graceful shutdown, then a relaunch resumes from disk"
"${SSH[@]}" "pkill -TERM -f 'cpd serve'; sleep 1; for i in \$(seq 1 20); do pgrep -f 'cpd serve' >/dev/null || exit 0; sleep 1; done; exit 1" && ok "cpd exited on SIGTERM" || bad "cpd ignored SIGTERM"
"${SSH[@]}" "pgrep -f 'venv/bin/python main.py' >/dev/null" && bad "ComfyUI outlived the supervisor" || ok "ComfyUI stopped with it"
"${SSH[@]}" "pgrep aria2c >/dev/null" && bad "aria2c outlived the supervisor" || ok "aria2c stopped with it"
"${SSH[@]}" "export CP_TOKEN=$TOKEN CP_MANIFEST=$MANIFEST COMFY_PORT=8188 CP_PORT=8189; nohup /opt/comfyui/venv/bin/python /opt/cp/supervisor.py >> /workspace/supervisor-boot.log 2>&1 &"
wait_for "ready after relaunch" 300 phase_is ready && ok "ready again after relaunch" || true
"${SSH[@]}" "grep -c 'skip, already on disk' /workspace/supervisor.log" | { read -r n; [ "${n:-0}" -ge 1 ] && ok "relaunch skipped files already on disk" || bad "relaunch re-downloaded"; }

# ---- verdict ----------------------------------------------------------------
step "summary"
echo "  image     $IMAGE"
echo "  offer     $OFFER   pull+start ${PULL_S}s   total $(( $(date +%s) - T0 ))s"
if [ "$FAILURES" = 0 ]; then echo "  ALL CHECKS PASSED"; else echo "  $FAILURES CHECK(S) FAILED"; exit 1; fi
