#!/usr/bin/env bash
# Time a realistic launch of the published image on a real vast.ai GPU, from
# "instance created" to "ComfyUI answers", and print where the time went.
#
# It launches the way the app does (ssh_direct, the app's onstart line, CP_* env
# as -e flags) with a realistic environment: three popular custom nodes, one
# 6.9 GB Hugging Face checkpoint and one Civitai LoRA. The instance is ALWAYS
# destroyed on exit.
#
#   IMAGE=ghcr.io/shunl12324/comfy-portal-cloud-server:sha-abc1234 test/vast-bench.sh
#   MACHINE=117918 IMAGE=... test/vast-bench.sh     # same host as a previous run, to compare
#   MANIFEST_JSON=my.json IMAGE=... test/vast-bench.sh
#
# Image pull time is only comparable between runs on the same host, and only
# when neither image was already cached there.
#
# Needs: VAST_API_KEY, curl, jq, ssh. Costs a few cents.
set -uo pipefail

: "${VAST_API_KEY:?set VAST_API_KEY}"
: "${IMAGE:?set IMAGE}"
API=https://console.vast.ai/api/v0
DISK=${DISK:-80}
MAX_PRICE=${MAX_PRICE:-0.25}
LABEL="cp-bench:$(date +%s)"
TOKEN=$(uuidgen | tr 'A-Z' 'a-z')
INSTANCE=""
T0=""

vast() { curl -fsS --retry 4 --retry-all-errors --retry-delay 2 -H "Authorization: Bearer $VAST_API_KEY" -H 'Content-Type: application/json' "$@" | tr -d '\000-\037'; }
at() { printf '[%4ss] %s\n' "$(( $(date +%s) - T0 ))" "$*"; }

cleanup() {
  local code=$?
  if [ -n "$INSTANCE" ]; then
    for _ in 1 2 3; do vast -X DELETE "$API/instances/$INSTANCE/" >/dev/null 2>&1 && break; sleep 3; done
    LEFT=$(vast "$API/instances/" 2>/dev/null | jq -r --arg l "$LABEL" '[.instances[]|select(.label==$l)]|length')
    [ "${LEFT:-1}" = 0 ] && echo "destroyed instance $INSTANCE" || echo "WARNING: instance $INSTANCE may still exist, check console.vast.ai"
  fi
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

if [ -n "${MANIFEST_JSON:-}" ]; then
  M=$(jq -c . "$MANIFEST_JSON")
else
  M=$(jq -nc '{version:1,
    models:[
      {url:"https://huggingface.co/stabilityai/stable-diffusion-xl-base-1.0/resolve/main/sd_xl_base_1.0.safetensors", folder:"checkpoints", filename:"sd_xl_base_1.0.safetensors"},
      {url:"https://civitai.com/api/download/models/135867", folder:"loras", filename:"add-detail-xl.safetensors"}],
    extensions:["https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite","https://github.com/rgthree/rgthree-comfy","https://github.com/kijai/ComfyUI-KJNodes"],
    ollamaModels:[]}')
fi
MANIFEST=$(printf '%s' "$M" | base64 | tr -d '\n')
ONSTART=$'mkdir -p /workspace\nnohup /opt/comfyui/venv/bin/python /opt/cp/supervisor.py >> /workspace/supervisor-boot.log 2>&1 &'
ENVSTR="-e CP_TOKEN=$TOKEN -e CP_MANIFEST=$MANIFEST -e COMFY_PORT=8188 -e CP_PORT=8189 -p 8188:8188 -p 8189:8189"

QUERY=$(jq -nc --argjson p "$MAX_PRICE" --argjson d "$DISK" --arg m "${MACHINE:-}" '{
  rentable:{eq:true}, type:"on-demand", num_gpus:{eq:1}, disk_space:{gte:($d+20)},
  reliability2:{gte:0.98}, inet_down:{gte:500}, direct_port_count:{gte:4}, cuda_max_good:{gte:12.8},
  gpu_name:{in:["RTX 3060","RTX 3060 Ti","RTX 3070","RTX 3080","RTX 3090","RTX 4060","RTX 4060 Ti","RTX 4070","RTX 4070S","RTX 4080","RTX 4090","RTX 5060","RTX 5060 Ti","RTX 5070"]},
  dph_total:{lte:$p}, order:[["dph_total","asc"]], limit:30}
  + (if $m != "" then {machine_id:{eq:($m|tonumber)}} else {} end)')
PICK=$(vast -X POST "$API/bundles/" -d "$QUERY" | jq -c --arg ex "${EXCLUDE_MACHINES:-}" --arg cn "${ALLOW_CN:-0}" '
  ($ex | split(" ") | map(select(length > 0))) as $bad
  | [.offers[] | . as $o | select(($bad | index($o.machine_id | tostring)) | not)
     | select($cn == "1" or (($o.geolocation // "") | test(", CN$") | not))] | .[0] // empty')
[ -n "$PICK" ] || { echo "no offer under \$$MAX_PRICE/hr"; exit 1; }
echo "$PICK" | jq -r '"offer \(.id) machine \(.machine_id): \(.gpu_name)  $\(.dph_total)/hr  \(.geolocation)  down \(.inet_down|floor) Mbps"'

T0=$(date +%s)
CREATED=$(vast -X PUT "$API/asks/$(echo "$PICK" | jq -r .id)/" -d "$(jq -nc --arg image "$IMAGE" --argjson disk "$DISK" --arg label "$LABEL" \
  --arg env "$ENVSTR" --arg onstart "$ONSTART" '{image:$image, disk:$disk, runtype:"ssh_direct", label:$label, env:$env, onstart:$onstart}')")
INSTANCE=$(echo "$CREATED" | jq -r .new_contract)
[ -n "$INSTANCE" ] && [ "$INSTANCE" != null ] || { echo "create failed: $CREATED"; INSTANCE=""; exit 1; }
at "created instance $INSTANCE ($IMAGE)"

# Pull and container start. vast's status line shows docker's per-layer progress.
LAST=""
for _ in $(seq 1 360); do
  J=$(vast "$API/instances/$INSTANCE/" 2>/dev/null | jq -c '.instances // empty' 2>/dev/null || true)
  ST=$(echo "$J" | jq -r '.actual_status // "?"'); SSHPORT=$(echo "$J" | jq -r '.ports["22/tcp"][0].HostPort // empty')
  MSG=$(echo "$J" | jq -r '.status_msg // ""' | tr '\n' ' ' | cut -c1-90)
  [ "$ST $MSG" != "$LAST" ] && at "vast: $ST  $MSG"; LAST="$ST $MSG"
  [ "$ST" = running ] && [ -n "$SSHPORT" ] && break
  case "$ST" in exited|offline) echo "instance entered $ST"; exit 1;; esac
  sleep 5
done
[ -n "$SSHPORT" ] || { echo "never started"; exit 1; }
RUNNING_AT=$(( $(date +%s) - T0 ))
IP=$(echo "$J" | jq -r .public_ipaddr)
SSH=(ssh -p "$SSHPORT" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 -o LogLevel=ERROR -o BatchMode=yes "root@$IP")
for _ in $(seq 1 120); do "${SSH[@]}" true 2>/dev/null && break; sleep 5; done
at "container running after ${RUNNING_AT}s (image pull + vast's start)"

LASTP=""
for _ in $(seq 1 600); do
  S=$("${SSH[@]}" "curl -fsS -m 5 -H 'Authorization: Bearer $TOKEN' http://127.0.0.1:8189/v2/state" 2>/dev/null) || { sleep 2; continue; }
  P=$(echo "$S" | jq -r .phase)
  [ "$P" != "$LASTP" ] && at "phase $P"; LASTP=$P
  case "$P" in ready|failed) break;; esac
  sleep 2
done
READY_AT=$(( $(date +%s) - T0 ))

echo; echo "== cpd report"
# Images from before `cpd report` existed: the steps are in the snapshot.
"${SSH[@]}" "CP_TOKEN=$TOKEN cpd report 2>/dev/null || curl -fsS -H 'Authorization: Bearer $TOKEN' http://127.0.0.1:8189/v2/snapshot" |
  { read -r first; case $first in '{'*) { echo "$first"; cat; } | jq -r '"total \(.elapsed)s phase=\(.phase)", (.steps[] | "step \((.ms // 0) / 1000)s \(.state) \(.id)")';; *) echo "$first"; cat;; esac; } | sed 's/^/  /'
echo; echo "== ComfyUI's slowest custom node imports"
"${SSH[@]}" "grep -oE '[0-9.]+ seconds: .*' /workspace/comfyui.log | sort -g -r | head -5" 2>/dev/null | sed 's/^/  /'
echo
echo "summary  image=$IMAGE"
echo "summary  container running at ${RUNNING_AT}s, phase=$LASTP at ${READY_AT}s after the instance was created"
[ "$LASTP" = ready ]
