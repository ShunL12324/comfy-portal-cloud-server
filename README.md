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

## Setup speed

A launch is: vast pulls the image, starts the container, and `cpd` installs the template
(models, extensions, Ollama models) and starts ComfyUI. Measured on vast with three popular
custom nodes (VideoHelperSuite, rgthree-comfy, KJNodes), the 6.9 GB SDXL base checkpoint
from Hugging Face and a Civitai LoRA (`make vast-bench`), before (`sha-db9df15`) and after,
each pair on one host:

| | fast host (Norway, 8.8 Gbps) | typical host (Poland, 0.9 Gbps) |
|---|---|---|
| Image, compressed | 11.0 → 5.9 GB (largest layer 4.0 → 0.9 GB) | same |
| Created → container running (pull + vast's start) | 263 → 156 s | 463 → 207 s |
| `cpd` start → ready | 104 → 37 s | 185 → 77 s |
| **Created → ready** | **382 → 194 s** | **654 → 283 s** |

After the change, ComfyUI's own startup (21-32 s) and the extensions (4-10 s) finish while
the model is still downloading, so `cpd`'s time is the download's: 6.9 GB at 210-340 MB/s
on the fast host and ~100 MB/s on the typical one.

Where the time went, and what changed:

- **The image pull.** A vast host fetched one layer from GHCR at 30-40 MB/s however fast
  its link (4.0 GB in 137 s), and docker fetches three layers at once. The image is now
  built on `nvidia/cuda:*-base` with only nvcc, gcc and `python3-dev` added: the devel
  image's 5 GB were static libraries, profilers and second copies of cuBLAS, cuDNN etc.,
  which torch's wheels ship and load themselves. torch is installed from a lock over six
  layers so none is much over a gigabyte. (The devel image also had no `Python.h`, so
  nothing could build an extension anyway; now one compiles, which the build checks.)
- **ComfyUI's startup.** The endpoint node installed Playwright's Chromium and ~290 apt
  packages on every import: 65 s of every launch. They are in the image now; what is left
  of its import (4-21 s) is the `apt-get update` its `playwright install-deps` still runs,
  which only the endpoint node can skip. ComfyUI is
  also started as soon as the extensions are in rather than after the models, since it
  does not need them to start; the `comfyui-start` step covers only what is left to wait
  for once the downloads finish (0 s when they take longer than ComfyUI's startup).
- **Python installs.** [uv](https://github.com/astral-sh/uv) instead of pip, in the image
  and at runtime. On the same host: torch 23 s vs 87-92 s, ComfyUI's requirements 8-12 s
  vs 36-41 s, the three nodes' requirements 1.5 s (one uv call) vs 11-12 s (pip, one at a
  time). micromamba from conda-forge took 51 s for torch and installs a different build
  (CUDA 12.9, not the official cu128 wheel), so it lost on both counts.
- **Extensions** are cloned in parallel and their requirements resolved in one uv call,
  which also picks versions every extension accepts. They also get the link first: the
  models wait for them (at most `CP_EXTENSIONS_FIRST_SECONDS`), because ComfyUI waits on
  the extensions and not on the models. On the typical host, sharing the link with the
  6.9 GB download had stretched the three extensions to 87 s; first, they take 10 s. torch, torchvision and torchaudio are
  constrained to the image's builds (`/opt/comfyui/venv/constraints.txt`), so an extension
  cannot replace the CUDA wheel with a PyPI one.
- **Downloads** were already overlapped with everything else; aria2's settings stay. aria2
  at 16 connections reached 220-380 MB/s from Hugging Face's CDN; other split sizes,
  `falloc`, a larger disk cache, `geom` piece selection, `hf_xet` (226-471 MB/s) and
  several files at once all fell inside the same run-to-run noise, and a single connection
  managed 86 MB/s. Civitai's signed URL gave 150-170 MB/s with aria2 against 30-70 with curl.

`cpd report` prints a launch's timings (each phase, each step with its start offset, each
model with its average rate, ComfyUI's time to answer), and `cpd` logs the same lines when
the launch settles. The same numbers are on `/v2`: `phases` on `/v2/state`, `startedAt` on
steps, `startedAt`/`finishedAt`/`avgSpeed` on models and `readyMs` on services.
`make vast-bench IMAGE=...` measures a whole launch on a rented GPU.

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
cpd report                 # how long each phase and step took, each model's average rate
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
| `CP_EXTENSIONS_FIRST_SECONDS` | Default 45. How long model downloads wait for the extensions to install, so ComfyUI's dependencies get the link first; `0` starts both at once |
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
make image    # the full CUDA image (amd64 host, ~6 GB compressed)
make vast-smoke IMAGE=ghcr.io/shunl12324/comfy-portal-cloud-server:sha-abc1234   # a few cents
make vast-bench IMAGE=... [MACHINE=117918]   # time a realistic launch, a few cents
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

`test/vast-bench.sh` launches the same way with a realistic template (three popular custom
nodes, a 6.9 GB Hugging Face checkpoint, a Civitai LoRA) and prints the pull timeline from
vast's status, each phase as it is reached, `cpd report` and ComfyUI's slowest imports. Pin
`MACHINE` to compare two images on one host; pull times only compare when neither image was
cached there.

CI (`.github/workflows/runtime-image.yml`) runs gofmt, vet, `go test -race` (with the app's
TypeScript client as a contract check), golangci-lint and the e2e before it builds and
publishes the image.

### Image build

Python packages are installed with uv (`UV_COMPILE_BYTECODE=1`, pip's index behaviour via
`UV_INDEX_STRATEGY=unsafe-best-match`, no cache left in the image). The venv still has pip.
uv is also a module in the venv, so ComfyUI-Manager uses it (`use_uv = True`), and
`/etc/uv/uv.toml` points a shell's `uv pip install` at the venv.

Build arguments: `COMFYUI_REF`, `MANAGER_REF`, `ENDPOINT_REF`, `TORCH_VERSION`,
`TORCHVISION_VERSION`, `TORCHAUDIO_VERSION`, `UV_VERSION`, and the CUDA flavour
`CUDA_TAG` / `CUDA_APT` / `TORCH_CUDA` / `TORCH_INDEX` (cu128 by default).

A manual run of the workflow takes two choices, both published only under a tag suffix
(never `v1`):

- `cuda: cu130` builds torch for CUDA 13 (`-cu130`), which ComfyUI's fast INT8/NVFP4
  kernels need. It needs a host driver of 580 or newer (`cuda_max_good >= 13.0` on vast).
- `compression: zstd` (`-zstd`) is 14% smaller and unpacks 2.5x faster, but needs Docker
  23+ on the host, and on vast the pull is bound by GHCR's per-layer rate rather than
  unpacking: it was no faster there, so gzip stays the default.

## Ollama

`Dockerfile.ollama` adds Ollama for workflows whose custom nodes call a local LLM. Those
nodes reach it on `127.0.0.1:11434` **inside** the container, so the port is deliberately
not published: an open Ollama endpoint on a public IP is free compute for whoever finds it.
