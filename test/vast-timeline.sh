#!/usr/bin/env bash
# Record what vast and cpd expose, and when, during a real launch: every ~1.5 s
# the instance's actual_status, status_msg and placement fields, and once SSH
# works, cpd's phase and steps the way the app reads them. One JSON object per
# line on stdout-file $OUT; a readable log on stdout. The instance is ALWAYS
# destroyed on exit.
#
#   IMAGE=ghcr.io/shunl12324/comfy-portal-cloud-server:sha-abc1234 OUT=run.jsonl test/vast-timeline.sh
#   MACHINE=19241 IMAGE=... OUT=... test/vast-timeline.sh
#
# status_msg is recorded as the app sees it: with control characters removed.
# Needs: VAST_API_KEY, curl, jq, ssh. Costs a few cents.
set -uo pipefail

: "${VAST_API_KEY:?set VAST_API_KEY}"
: "${IMAGE:?set IMAGE}"
: "${OUT:?set OUT, the JSONL file to write}"
API=https://console.vast.ai/api/v0
DISK=${DISK:-80}
MAX_PRICE=${MAX_PRICE:-0.25}
MAX_SECONDS=${MAX_SECONDS:-1500}
INTERVAL=${INTERVAL:-1.5}
LABEL="cp-timeline:$(date +%s)"
TOKEN=$(uuidgen | tr 'A-Z' 'a-z')
INSTANCE=""

vastraw() { curl -fsS -m 20 --retry 3 --retry-all-errors --retry-delay 1 -H "Authorization: Bearer $VAST_API_KEY" -H 'Content-Type: application/json' "$@"; }
vast() { vastraw "$@" | tr -d '\000-\037'; }
now_ms() { python3 -c 'import time;print(int(time.time()*1000))' 2>/dev/null || echo $(( $(date +%s) * 1000 )); }

cleanup() {
  local code=$?
  if [ -n "$INSTANCE" ]; then
    for _ in 1 2 3; do vast -X DELETE "$API/instances/$INSTANCE/" >/dev/null 2>&1 && break; sleep 3; done
    LEFT=$(vast "$API/instances/" 2>/dev/null | jq -r --arg l "$LABEL" '[.instances[]|select(.label==$l)]|length')
    [ "${LEFT:-1}" = 0 ] && echo "destroyed instance $INSTANCE" || echo "WARNING: instance $INSTANCE may still exist, check console.vast.ai"
  fi
  [ -n "${PROBE_PID:-}" ] && kill "$PROBE_PID" 2>/dev/null
  [ -n "${RELAY_PID:-}" ] && kill "$RELAY_PID" 2>/dev/null
  rm -f "$OUT.last"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

M=$(jq -nc '{version:1,
  models:[
    {url:"https://huggingface.co/stabilityai/stable-diffusion-xl-base-1.0/resolve/main/sd_xl_base_1.0.safetensors", folder:"checkpoints", filename:"sd_xl_base_1.0.safetensors"},
    {url:"https://civitai.com/api/download/models/135867", folder:"loras", filename:"add-detail-xl.safetensors"}],
  extensions:["https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite","https://github.com/rgthree/rgthree-comfy","https://github.com/kijai/ComfyUI-KJNodes"],
  ollamaModels:[]}')
MANIFEST=$(printf '%s' "$M" | base64 | tr -d '\n')
ONSTART=$'mkdir -p /workspace\nnohup /opt/comfyui/venv/bin/python /opt/cp/supervisor.py >> /workspace/supervisor-boot.log 2>&1 &'
ENVSTR="-e CP_TOKEN=$TOKEN -e CP_MANIFEST=$MANIFEST -e COMFY_PORT=8188 -e CP_PORT=8189 -p 8188:8188 -p 8189:8189"

QUERY=$(jq -nc --argjson p "$MAX_PRICE" --argjson d "$DISK" --arg m "${MACHINE:-}" --argjson down "${MIN_DOWN:-500}" '{
  rentable:{eq:true}, type:"on-demand", num_gpus:{eq:1}, disk_space:{gte:($d+20)},
  reliability2:{gte:0.98}, inet_down:{gte:$down}, direct_port_count:{gte:4}, cuda_max_good:{gte:12.8},
  gpu_name:{in:["RTX 3060","RTX 3060 Ti","RTX 3070","RTX 3080","RTX 3090","RTX 4060","RTX 4060 Ti","RTX 4070","RTX 4070S","RTX 4080","RTX 4090","RTX 5060","RTX 5060 Ti","RTX 5070"]},
  dph_total:{lte:$p}, order:[["dph_total","asc"]], limit:30}
  + (if $m != "" then {machine_id:{eq:($m|tonumber)}} else {} end)')
PICK=$(vast -X POST "$API/bundles/" -d "$QUERY" | jq -c --arg ex "${EXCLUDE_MACHINES:-}" --arg cn "${ALLOW_CN:-0}" '
  ($ex | split(" ") | map(select(length > 0))) as $bad
  | [.offers[] | . as $o | select(($bad | index($o.machine_id | tostring)) | not)
     | select($cn == "1" or (($o.geolocation // "") | test(", CN$") | not))] | .[0] // empty')
[ -n "$PICK" ] || { echo "no offer under \$$MAX_PRICE/hr"; exit 1; }
echo "$PICK" | jq -r '"offer \(.id) machine \(.machine_id): \(.gpu_name)  $\(.dph_total)/hr  \(.geolocation)  down \(.inet_down|floor) Mbps"'
echo "$PICK" | jq -c '{event:"offer", id, machine_id, gpu_name, dph_total, geolocation, inet_down}' > "$OUT"

T0=$(now_ms)
at() { printf '[%6.1fs] %s\n' "$(echo "($(now_ms) - $T0) / 1000" | bc -l)" "$*"; }
rec() { jq -c --argjson t "$(( $(now_ms) - T0 ))" '. + {t: $t}' >> "$OUT"; }

CREATED=$(vast -X PUT "$API/asks/$(echo "$PICK" | jq -r .id)/" -d "$(jq -nc --arg image "$IMAGE" --argjson disk "$DISK" --arg label "$LABEL" \
  --arg env "$ENVSTR" --arg onstart "$ONSTART" '{image:$image, disk:$disk, runtype:"ssh_direct", label:$label, env:$env, onstart:$onstart}')")
INSTANCE=$(echo "$CREATED" | jq -r .new_contract)
[ -n "$INSTANCE" ] && [ "$INSTANCE" != null ] || { echo "create failed: $CREATED"; INSTANCE=""; exit 1; }
at "created instance $INSTANCE ($IMAGE)"
jq -nc --arg i "$INSTANCE" --arg image "$IMAGE" '{event:"created", instance:($i|tonumber), image:$image}' | rec

# cpd's view over SSH, in the background once the box is reachable: the first
# answer is what the app's "installing" stage starts from.
probe() {
  local ip=$1 port=$2
  local SSH=(ssh -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 -o LogLevel=ERROR -o BatchMode=yes "root@$ip")
  local seen_ssh="" last=""
  while :; do
    if [ -z "$seen_ssh" ]; then
      "${SSH[@]}" true 2>/dev/null || { sleep 1; continue; }
      seen_ssh=1; jq -nc '{event:"ssh"}' | rec; at "ssh works"
      # What is the container's PID 1, and does a line written to its stdout
      # (what docker logs, and vast tails) show up in status_msg?
      INFO=$("${SSH[@]}" 'cat /proc/1/cmdline | tr "\0" " "; echo; ls -l /proc/1/fd/1 /proc/1/fd/2; echo "CP probe=stdout" > /proc/1/fd/1 && echo wrote1; echo "CP probe=stderr" > /proc/1/fd/2 && echo wrote2' 2>&1)
      jq -nc --arg i "$INFO" '{event:"pid1", info:$i}' | rec; at "pid1: $(echo "$INFO" | tr '\n' '|')"
      # A line every 2 s on the container's stdout for two minutes: does any of
      # it reach status_msg, or vast's container log (request_logs)?
      "${SSH[@]}" 'nohup sh -c "for i in \$(seq 1 60); do echo CP probe \$i > /proc/1/fd/1; sleep 2; done" >/dev/null 2>&1 &'
      ( sleep 20; U=$(vast -X PUT "$API/instances/request_logs/$INSTANCE/" -d '{"tail":"40"}' | jq -r '.result_url // empty')
        sleep 8; L=$(curl -fsS -m 20 "$U" 2>/dev/null | tail -40)
        jq -nc --arg l "$L" --arg u "${U%%\?*}" '{event:"request_logs", url:$u, log:$l}' | rec; at "request_logs: $(echo "$L" | tail -3 | tr '\n' '|')" ) &
    fi
    S=$("${SSH[@]}" "curl -fsS -m 4 -H 'Authorization: Bearer $TOKEN' http://127.0.0.1:8189/v1/status" 2>/dev/null) || { sleep 1; continue; }
    echo "$S" | jq -c '{event:"cpd", phase, elapsed, steps:[.steps[]|{id,state,ms}], models:[.models[]|{name,state,bytes,total,speed}]}' 2>/dev/null | rec
    P=$(echo "$S" | jq -r .phase 2>/dev/null)
    [ "$P" != "$last" ] && at "cpd phase $P"; last=$P
    sleep 2
  done
}

# The app reaches the box through vast's SSH relay (ssh_host:ssh_port), which
# can lag the direct port.
relay() {
  for _ in $(seq 1 300); do
    ssh -p "$2" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 -o LogLevel=ERROR -o BatchMode=yes "root@$1" \
      "curl -s -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:8189/v1/health" 2>/dev/null | grep -q 200 &&
      { jq -nc '{event:"relay-cpd"}' | rec; at "cpd answers through the relay"; return; }
    sleep 1
  done
}

LAST=""; READY_AT=""; PROBE_PID=""; RELAY_PID=""
while :; do
  ELAPSED=$(( ($(now_ms) - T0) / 1000 ))
  [ "$ELAPSED" -ge "$MAX_SECONDS" ] && { at "giving up after ${MAX_SECONDS}s"; break; }
  vastraw "$API/instances/$INSTANCE/" > "$OUT.last" 2>/dev/null
  J=$(tr -d '\000-\037' < "$OUT.last" | jq -c '.instances // empty' 2>/dev/null || true)
  # Keep a few raw responses that carry control characters, to see what they are.
  if [ "${RAWS:-0}" -lt 6 ] && LC_ALL=C grep -q '[[:cntrl:]]' "$OUT.last"; then
    RAWS=$(( ${RAWS:-0} + 1 )); cp "$OUT.last" "$OUT.raw$RAWS"
  fi
  if [ -n "$J" ]; then
    [ ! -f "$OUT.full.json" ] && echo "$J" > "$OUT.full.json"
    echo "$J" | jq -c '{event:"vast", actual_status, intended_status, cur_state, next_state, status_msg, machine_id, host_id,
      public_ipaddr, ssh_host, ssh_port, ports: (.ports // {} | with_entries(.value = (.value[0].HostPort // null))),
      start_date, image_runtype, inet_down, uptime_mins}' | rec
    ST=$(echo "$J" | jq -r '.actual_status // "-"'); MSG=$(echo "$J" | jq -r '.status_msg // ""' | cut -c1-110)
    [ "$ST $MSG" != "$LAST" ] && at "vast: $ST  $MSG"; LAST="$ST $MSG"
    IP=$(echo "$J" | jq -r '.public_ipaddr // empty'); SSHPORT=$(echo "$J" | jq -r '.ports["22/tcp"][0].HostPort // empty')
    if [ -z "$PROBE_PID" ] && [ -n "$IP" ] && [ -n "$SSHPORT" ]; then
      echo "$J" > "$OUT.running.json"
      probe "$IP" "$SSHPORT" & PROBE_PID=$!
      relay "$(echo "$J" | jq -r .ssh_host)" "$(echo "$J" | jq -r .ssh_port)" & RELAY_PID=$!
    fi
    case "$ST" in exited|offline) at "instance entered $ST"; break;; esac
  fi
  if [ -z "$READY_AT" ] && tail -n 5 "$OUT" | grep -q '"phase":"ready"'; then READY_AT=$ELAPSED; at "ready"; fi
  # Keep watching status_msg for a while after ready: does it follow the log?
  [ -n "$READY_AT" ] && [ $((ELAPSED - READY_AT)) -ge "${AFTER_READY:-30}" ] && break
  sleep "$INTERVAL"
done
echo "timeline in $OUT"
