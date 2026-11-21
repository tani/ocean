FROM golang:1.26-bookworm

ARG TARGETARCH
ENV FLAKE_LEAN_ROOT=/opt/lean
ENV PATH=/opt/lean/bin:/opt/flake/bin:$PATH
RUN apt-get update && apt-get install -y --no-install-recommends zstd time && rm -rf /var/lib/apt/lists/*
RUN set -eu; case "$TARGETARCH" in arm64) platform=linux_aarch64;; amd64) platform=linux;; *) exit 1;; esac; \
    mkdir -p /opt/lean; \
    curl -fsSL --retry 3 "https://github.com/leanprover/lean4/releases/download/v4.35.0/lean-4.35.0-${platform}.tar.zst" -o /tmp/lean.tar.zst; \
    tar --zstd -xf /tmp/lean.tar.zst -C /opt/lean --strip-components=1; rm /tmp/lean.tar.zst

WORKDIR /opt/flake
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN LEAN_NUM_THREADS=8 scripts/build
CMD ["sleep", "infinity"]
