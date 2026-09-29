# Comfy Portal cloud server

The Docker image a rented GPU boots into, and `cpd`, the program that runs inside it.

**This repository shares no code with the app.** It is Go and Dockerfiles; nothing here
is bundled into the React Native build. The only contract between the two is the HTTP
API below, which the app consumes through
[`src/services/cloud-supervisor.ts`](https://github.com/ShunL12324/comfy-portal/blob/main/src/services/cloud-supervisor.ts).

## Why an image rather than a script

Everything that used to be installed at boot (apt, `git clone`, pip, torch) is baked in.
vast has no persistent volume, so installing at boot repeated all of it on every launch,
on an arbitrary host, with the GPU billing throughout. What stays at runtime is only what
varies per launch: the template's models, extensions and Ollama models.

## Why a supervisor rather than a shell script

A shell script can install, but it cannot be asked anything. `cpd` installs *and* serves
an API, so the app can show which model is downloading, how many bytes in, what failed
and why. It stays resident after ComfyUI starts, so a crash is something the app can see
rather than infer from a dead socket.

It is one static Go binary with no dependencies outside the standard library. It drives a
long-lived `aria2c` over JSON-RPC for downloads and keeps ComfyUI (and Ollama) running with
backoff.

## API

Served on `:8189`. Every route except `/v2/health` and `/v2/openapi.json` requires
`Authorization: Bearer $CP_TOKEN`; the port is published on a public IP, and `cpd`
refuses to start without a token. The full contract is [`internal/api/openapi.json`](internal/api/openapi.json),
served at `GET /v2/openapi.json`; a test fails if a route is missing from it.

Errors are `application/problem+json` (RFC 9457) with a stable `code` and, where useful, a `hint`.

| Route | |
|---|---|
| `GET /v2/health` | Unauthenticated liveness, answers the moment the port is open |
| `GET /v2/state` | Phase, totals, stalled flag, error |
| `GET /v2/snapshot` | Everything in one response (one round trip over SSH) |
| `GET /v2/steps` | Install steps with durations |
| `GET /v2/models`, `GET /v2/models/{id}` | Per-model bytes, speed, state, error |
| `POST /v2/models/{id}/retry`, `POST /v2/models/retry-failed` | Re-queue failed downloads |
| `GET /v2/services`, `GET /v2/services/{name}` | comfyui, ollama, aria2: state, pid, restarts, last exit |
| `POST /v2/services/{name}/restart` | `comfyui` or `ollama` |
| `GET /v2/manifest`, `PUT /v2/manifest` | What was requested; add more to a running instance |
| `GET /v2/logs/{stream}?tail=N` | Redacted tail of `supervisor`, `comfyui`, `ollama`, `aria2` |
| `GET /v2/events` | Server-Sent Events, see below |

Model IDs are stable (a hash of folder and file name) and never contain a slash.

**Events.** A new client first receives a `snapshot` event with the full state, then one
typed event per change: `phase.changed`, `step.updated`, `model.updated`,
`service.updated`, `stalled.changed`, `restart-required.changed`. Each has an `id`; a
client that reconnects with `Last-Event-ID` receives only what it missed (or a fresh
snapshot if it fell too far behind). `: ping` comments keep the stream alive.

**Applying a manifest.** `PUT /v2/manifest` is additive and idempotent, and only accepted
once the phase is `ready`. New models download, new extensions and Ollama models install in
the background. Extensions load only after ComfyUI restarts, so `restartRequired` turns true
and `POST /v2/services/comfyui/restart` clears it.

### `/v1` (deprecated)

App builds that predate `/v2` still talk to `/v1/status`, `/v1/log`, `/v1/events`,
`/v1/models/retry` and `/v1/comfyui/restart`. They are served from the same state with the
exact shapes the Python supervisor sent, marked `Deprecation: true`, and pinned by tests
(including a check against the app's TypeScript types). Remove them, and
`compat/supervisor.py`, once no released app build uses them.

## CLI

The subcommands exist so a remote shell can ask questions without hand-writing curl:

```sh
cpd status [--json]        # phase, models, services (reads CP_TOKEN and CP_PORT)
cpd logs comfyui -n 100
cpd health                 # exit 0 if it answers; the image's HEALTHCHECK
cpd version
```

## Environment

| | |
|---|---|
| `CP_TOKEN` | Required. Random per launch. (`CP_ALLOW_NO_AUTH=1` disables auth for local development.) |
| `CP_MANIFEST` | base64 of `{models, extensions, ollamaModels}`: see below |
| `HF_TOKEN`, `CIVITAI_API_KEY` | Optional, for gated downloads. `HF_TOKEN` is only ever sent to huggingface.co |
| `CP_SSH_PUBLIC_KEY` | Optional. Starts a key-only sshd (RunPod; vast supplies its own) |
| `COMFY_PORT`, `CP_PORT`, `CP_LISTEN` | Default 8188 / 8189 / `:8189` |
| `CP_MAX_DOWNLOADS`, `CP_STALL_SECONDS` | Default 4 / 300 |
| `CP_WORKSPACE`, `COMFY_DIR` | Default `/workspace` / `/opt/comfyui` |

The manifest is base64 because vast takes container env as one docker-flag string split on
whitespace: a multi-line value silently loses everything after its first line.

```json
{
  "version": 1,
  "models": [{"url": "...", "folder": "loras", "filename": "x.safetensors", "sizeBytes": 0}],
  "extensions": ["https://github.com/user/ComfyUI-Something"],
  "ollamaModels": ["llama3.1:8b"]
}
```

Entries are validated before anything downloads: http(s) URLs only, and `folder` and
`filename` cannot climb out of the models directory.

## Behaviour worth knowing

- **One bad model never stops a launch.** A 404 marks that model `error` and the rest carry
  on; ComfyUI still starts, and the app offers a retry.
- **Restarts resume.** `/workspace` survives, so a complete file is skipped and a partial one
  (with aria2's `.aria2` control file beside it) is continued rather than re-fetched.
- **Secrets never leave the box.** `CP_TOKEN`, `HF_TOKEN`, `CIVITAI_API_KEY` and aria2's RPC
  secret are stripped from the supervisor log, every log endpoint and every error.
- **Graceful shutdown.** SIGTERM stops ComfyUI, Ollama and aria2 (process groups, then SIGKILL
  after 10 s) before `cpd` exits. The image runs under `tini` to reap orphaned children.
- **A failed launch stays inspectable.** The API keeps serving with `phase: "failed"` and the
  reason, rather than the port going dark.

## Layout

```
cmd/cpd/            main, serve, and the status/logs/health subcommands
internal/config     environment
internal/manifest   manifest parsing, validation, stable model IDs
internal/state      the single source of truth + event hub (SSE ring buffer)
internal/aria2      JSON-RPC client
internal/downloads  queueing, progress polling, retry, Civitai/HF handling
internal/procs      process supervision with backoff and readiness probes
internal/install    directory links, extensions, Ollama, sshd
internal/pipeline   the launch sequence, and applying manifests later
internal/api        /v2, the /v1 shim, SSE, problem+json, openapi.json
internal/redact     secret scrubbing
internal/logs       log tailing and logger setup
compat/             supervisor.py: a 3-line exec shim for older app builds
test/e2e.sh         real cpd + real aria2c + stub ComfyUI
test/rig/           the stub ComfyUI and fixture server
```

## Developing

```sh
make test     # go vet + go test -race
make e2e      # needs aria2c, curl, jq; picks free ports, no network, no GPU
make build    # bin/cpd
make image    # the full CUDA image (amd64 host, ~20 GB)
make vast-smoke IMAGE=ghcr.io/shunl12324/comfy-portal-cloud-server:sha-abc1234   # a few cents
```

`test/e2e.sh` covers the failures that have already happened: a model 404 must not stop the
other models or ComfyUI starting; killing ComfyUI must bring it back with `restarts`
incremented; a restart must skip files already on disk; and no secret may appear in any log.

### On a real GPU

`test/vast-smoke.sh` rents the cheapest reliable RTX 30/40-series card on vast.ai, launches
the published image **exactly the way the app does** (`ssh_direct`, the app's `onstart` line,
`CP_*` env as `-e` flags), drives the supervisor over SSH-loopback curl like the app, and
always destroys the instance. It checks boot to `ready` with a real HF model and a real
extension, CUDA visibility, the v1 and v2 APIs, secret redaction, `kill -9` recovery, applying
a manifest, graceful shutdown and a resuming relaunch. Needs `VAST_API_KEY`; a run costs a few
cents. Hosts whose docker daemon is broken (`unresolvable CDI devices`, containerd errors,
never starting) are detected, destroyed and skipped automatically.

It exists because two bugs were invisible to every other test:

- vast starts its own `sshd`, which needs host keys. The image ships none (baked-in keys
  would be shared by everyone who pulls it), so `/usr/sbin/sshd` is a wrapper that generates
  them on first start.
- On vast `/workspace` is a different filesystem from the image layer, so moving the image's
  `custom_nodes` and `models` there with `rename(2)` failed with `EXDEV`. It now falls back to
  copy-then-delete, through a `.partial` directory so an interrupted copy is never trusted.

CI (`.github/workflows/runtime-image.yml`) runs gofmt, vet, `go test -race` (with the app's
TypeScript client as a contract check), golangci-lint and the e2e before it builds and
publishes the image.

## Ollama

`Dockerfile.ollama` adds Ollama for workflows whose custom nodes call a local LLM. Those
nodes reach it on `127.0.0.1:11434` **inside** the container, so the port is deliberately
not published: an open Ollama endpoint on a public IP is free compute for whoever finds it.
