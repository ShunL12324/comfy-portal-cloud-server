# syntax=docker/dockerfile:1.7
#
# Comfy Portal runtime image.
#
# Everything that used to be installed on the rented machine at boot — apt,
# git clone, pip, torch — is baked in here instead. vast has no persistent
# volume, so the old script repeated all of it on every launch, on an arbitrary
# host, with the GPU billing the whole time. That was the largest source of
# both failures and cost, and none of it could be reproduced locally.
#
# What stays at runtime is only what genuinely varies per launch: the template's
# models, its extensions, and its Ollama models.
#
# Versions are pinned. A rented instance should get the exact stack that was
# tested, not whatever master happened to be that morning.
#
# The image is pulled on every launch, so its size and shape are launch time.
# Measured on vast, a host fetches one layer from GHCR at roughly 30-40 MB/s
# however fast its link is, and docker fetches three layers at once. So: no
# byte that torch's wheels already ship (the CUDA libraries), and no layer much
# over a gigabyte, so the three streams stay busy until the end.

# The CUDA flavour: base image, the apt suffix of its nvcc, and torch's index.
# cu130 needs a host driver of 580 or newer.
ARG CUDA_TAG=12.8.1-base-ubuntu22.04
ARG UV_VERSION=0.12.24

# The supervisor is one static Go binary, built in its own stage so the CUDA
# image carries no toolchain and a code change never invalidates the torch layers.
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
# vast's GPU hosts are x86 only.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/cpd ./cmd/cpd

FROM ghcr.io/astral-sh/uv:${UV_VERSION} AS uv

FROM nvidia/cuda:${CUDA_TAG}

# Links the GHCR package to this repo, so it inherits the repo's access and
# visibility and this repo's workflow can publish it.
LABEL org.opencontainers.image.source="https://github.com/ShunL12324/comfy-portal-cloud-server"

ARG COMFYUI_REF=v0.39.2
ARG MANAGER_REF=f39cbd56fecae0b27a446c0cd450cd591f3a8bea
ARG ENDPOINT_REF=63dcd2678996634d082d5a7bbfee957cce087d6e
ARG CUDA_APT=12-8
ARG TORCH_CUDA=12.8
ARG TORCH_INDEX=https://download.pytorch.org/whl/cu128
ARG TORCH_VERSION=2.11.0
ARG TORCHVISION_VERSION=0.26.0
ARG TORCHAUDIO_VERSION=2.11.0
ARG UV_VERSION

# uv installs every Python package (3-8x faster than pip, measured, see the
# README). UV_COMPILE_BYTECODE matches what pip did: .pyc files are written at
# install, on every core, not one module at a time during ComfyUI's first
# import. unsafe-best-match is pip's index behaviour, for ComfyUI-Manager and
# extensions that add an --extra-index-url.
ENV DEBIAN_FRONTEND=noninteractive \
    PIP_NO_CACHE_DIR=1 \
    PYTHONUNBUFFERED=1 \
    VIRTUAL_ENV=/opt/comfyui/venv \
    UV_COMPILE_BYTECODE=1 \
    UV_INDEX_STRATEGY=unsafe-best-match \
    UV_PYTHON_DOWNLOADS=never \
    PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright \
    COMFY_DIR=/opt/comfyui \
    CP_WORKSPACE=/workspace \
    COMFY_PORT=8188 \
    CP_PORT=8189
ENV PATH=/opt/comfyui/venv/bin:$PATH

# A compiler and nvcc, but not the CUDA devel image. Custom nodes do build
# extensions on install (C++ ones like insightface, and the odd CUDA kernel), so
# gcc, Python.h and nvcc stay. The rest of the devel image — 5 GB compressed of
# static libraries, profilers and a second copy of cuBLAS, cuDNN and friends —
# duplicates what torch's wheels already ship, and torch loads its own copies.
# Headers for those libraries come from the same wheels (linked in below).
#
# The second line is what vast's ssh_direct wrapper apt-installs into every
# container on first start; already present, its install is a no-op.
RUN apt-get update -qq && \
    apt-get install -y -qq --no-install-recommends \
        python3 python3-venv python3-dev build-essential \
        git curl ca-certificates aria2 openssh-server tini libgl1 libglib2.0-0 \
        tmux wget less locales sudo software-properties-common rsync \
        cuda-nvcc-${CUDA_APT} cuda-cudart-dev-${CUDA_APT} && \
    rm -rf /var/lib/apt/lists/*

RUN rm -f /etc/ssh/ssh_host_*

# The venv keeps pip for anyone who wants it; uv (in the venv, so ComfyUI-Manager
# picks it up as `python -m uv` too) is what installs.
RUN --mount=from=uv,source=/uv,target=/usr/local/bin/uv \
    uv venv --seed --python /usr/bin/python3 /opt/comfyui/venv && \
    uv pip install --no-cache uv==${UV_VERSION}

# torch, pinned, and resolved once against the torch index into a lock; the
# lock is then installed in several layers so no single layer is 4 GB. The
# CUDA libraries go first, grouped by size; torch itself last.
# cp-layer.sh installs the lock's lines that match a pattern, if there are any.
RUN printf '%s\n' '#!/bin/sh' 'set -e' \
        'grep -E "$1" /opt/comfyui/venv/torch.lock > /tmp/l || true' \
        '[ -s /tmp/l ] && uv pip install --no-cache --no-deps --index-url "$TORCH_INDEX" -r /tmp/l' \
        'rm -f /tmp/l' > /opt/cp-layer.sh && chmod 755 /opt/cp-layer.sh
RUN printf 'torch==%s\ntorchvision==%s\ntorchaudio==%s\n' \
        "${TORCH_VERSION}" "${TORCHVISION_VERSION}" "${TORCHAUDIO_VERSION}" > /opt/comfyui/venv/constraints.txt && \
    uv pip compile --no-cache --quiet --no-header --no-annotate --index-url ${TORCH_INDEX} \
        /opt/comfyui/venv/constraints.txt -o /opt/comfyui/venv/torch.lock
RUN /opt/cp-layer.sh '^nvidia-cudnn'
RUN /opt/cp-layer.sh '^nvidia-cublas'
RUN /opt/cp-layer.sh '^nvidia-(cusparselt|cusolver)'
RUN /opt/cp-layer.sh '^nvidia-(nccl|cusparse)'
RUN /opt/cp-layer.sh '^(nvidia-|triton=)'
RUN uv pip install --no-cache --index-url ${TORCH_INDEX} -r /opt/comfyui/venv/torch.lock

# Headers for the CUDA libraries above, for extensions that compile against
# torch: symlinked into CUDA_HOME's include directory, which nvcc and torch's
# cpp_extension already search, without overwriting cudart's own.
RUN for d in /opt/comfyui/venv/lib/python3.10/site-packages/nvidia/*/include \
             /opt/comfyui/venv/lib/python3.10/site-packages/nvidia/*/*/include; do \
        [ -d "$d" ] && cp -rsn "$d/." /usr/local/cuda/include/; \
    done; true

# ComfyUI's frontend and workflow templates are mostly data and pinned in its
# requirements; they get their own layer, and the rest of the requirements theirs.
RUN git clone --depth 1 --branch ${COMFYUI_REF} \
        https://github.com/Comfy-Org/ComfyUI /tmp/comfyui && \
    cp -a /tmp/comfyui/. /opt/comfyui/ && \
    rm -rf /tmp/comfyui /opt/comfyui/.git && \
    grep -E '^comfyui-(frontend-package|workflow-templates|embedded-docs)==' /opt/comfyui/requirements.txt > /tmp/data.txt && \
    uv pip install --no-cache -c /opt/comfyui/venv/constraints.txt -r /tmp/data.txt && \
    rm /tmp/data.txt
RUN uv pip install --no-cache -c /opt/comfyui/venv/constraints.txt -r /opt/comfyui/requirements.txt

# ComfyUI-Manager, and the endpoint node the app's /cpe/* calls depend on —
# without the latter the app cannot push workflows once the instance answers.
RUN mkdir -p /opt/comfyui/custom_nodes && \
    git clone https://github.com/Comfy-Org/ComfyUI-Manager \
        /opt/comfyui/custom_nodes/ComfyUI-Manager && \
    git -C /opt/comfyui/custom_nodes/ComfyUI-Manager checkout -q ${MANAGER_REF} && \
    git clone https://github.com/ShunL12324/comfy-portal-endpoint \
        /opt/comfyui/custom_nodes/comfy-portal-endpoint && \
    git -C /opt/comfyui/custom_nodes/comfy-portal-endpoint checkout -q ${ENDPOINT_REF} && \
    uv pip install --no-cache -c /opt/comfyui/venv/constraints.txt \
        -r /opt/comfyui/custom_nodes/ComfyUI-Manager/requirements.txt \
        -r /opt/comfyui/custom_nodes/comfy-portal-endpoint/requirements.txt

# The endpoint node makes sure Playwright's Chromium and its system packages
# are installed every time it is imported. Measured on vast, that was 65 s of
# every launch, inside ComfyUI's startup. Installed here, its check finds
# everything present and returns in seconds. Its `install-deps` covers every
# browser, not just Chromium, so this installs the same set: Chromium's alone
# still left 227 packages and 36 s to every launch.
RUN apt-get update -qq && \
    python -m playwright install-deps && \
    python -m playwright install chromium && \
    rm -rf /var/lib/apt/lists/*

# No SSH host keys in the image: baked-in keys would be shared by every instance
# that pulls it, and the private half would be public. But something has to make
# them before sshd will accept a connection, and vast starts sshd from its own
# entrypoint, which does not (its openssh-server install is a no-op because the
# package is already here). So sshd itself generates them on first start, however
# it is launched: vast, RunPod, or cpd.
RUN mv /usr/sbin/sshd /usr/sbin/sshd.real && \
    printf '%s\n' '#!/bin/sh' \
        '[ -e /etc/ssh/ssh_host_ed25519_key ] || ssh-keygen -A >/dev/null 2>&1' \
        'exec /usr/sbin/sshd.real "$@"' > /usr/sbin/sshd && \
    chmod 755 /usr/sbin/sshd

# The same uv settings for a shell on the box, where the image's ENV does not
# reach (vast's sshd starts sessions without it): `uv pip install` there goes
# into ComfyUI's venv, like it does for cpd and ComfyUI-Manager.
RUN mkdir -p /etc/uv && printf '%s\n' \
        'compile-bytecode = true' \
        'index-strategy = "unsafe-best-match"' \
        'python-downloads = "never"' \
        '[pip]' \
        'python = "/opt/comfyui/venv/bin/python"' > /etc/uv/uv.toml

COPY --from=build /out/cpd /usr/local/bin/cpd
# App builds that predate the Go supervisor launch /opt/cp/supervisor.py.
COPY compat/supervisor.py /opt/cp/supervisor.py

# Fail the build rather than the rental if the stack is broken: torch is the
# pinned CUDA build, pip and uv both work in the venv, and an extension that
# includes torch's CUDA headers compiles with what is here.
RUN python -c "import torch, sys; print('torch', torch.__version__); assert torch.version.cuda == '${TORCH_CUDA}', torch.version.cuda" && \
    pip --version && python -m uv --version && \
    TI=/opt/comfyui/venv/lib/python3.10/site-packages/torch/include && \
    printf '%s\n' '#include <torch/extension.h>' '#include <ATen/cuda/CUDAContext.h>' \
        '__global__ void k(float *x) { x[threadIdx.x] *= 2; }' \
        'void run(torch::Tensor t) { k<<<1, 32, 0, at::cuda::getCurrentCUDAStream()>>>(t.data_ptr<float>()); }' > /tmp/k.cu && \
    nvcc -c /tmp/k.cu -o /tmp/k.o -std=c++17 -arch=sm_86 -I$TI -I$TI/torch/csrc/api/include -I/usr/include/python3.10 && \
    rm -f /tmp/k.cu /tmp/k.o && \
    cpd version

EXPOSE 22 8188 8189
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["cpd", "health"]
# tini reaps the children ComfyUI and custom nodes orphan; cpd is not an init.
ENTRYPOINT ["tini", "--", "cpd", "serve"]
