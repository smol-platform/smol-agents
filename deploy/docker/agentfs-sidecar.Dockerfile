# syntax=docker/dockerfile:1.6
#
# The agentfs-sidecar provides AgentFS durable storage for AgentRun pods:
# `init` restores the mount tree from S3, `serve` runs periodic snapshot backups
# to S3 (and a final one on SIGTERM). The default ("tar") backend is pure Go;
# the kopia backend (AGENTFS_BACKEND=kopia) shells out to the kopia binary baked
# below for content-addressed snapshots (dedup, history, diff, rollback).

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agentfs-sidecar ./cmd/agentfs-sidecar

# Fetch the kopia binary (content-addressed snapshot engine) for the target arch.
# KOPIA_VERSION may be pinned; empty resolves the latest release.
FROM alpine:3 AS kopia
ARG TARGETARCH
ARG KOPIA_VERSION=""
RUN set -eux; \
    apk add --no-cache curl tar; \
    case "$TARGETARCH" in amd64) A=x64 ;; arm64) A=arm64 ;; *) echo "unsupported arch: $TARGETARCH"; exit 1 ;; esac; \
    VER="${KOPIA_VERSION:-$(curl -fsSL https://api.github.com/repos/kopia/kopia/releases/latest | sed -nE 's/.*"tag_name": *"v?([^"]+)".*/\1/p' | head -1)}"; \
    echo "kopia version: ${VER} (${A})"; \
    mkdir -p /out; \
    curl -fsSL "https://github.com/kopia/kopia/releases/download/v${VER}/kopia-${VER}-linux-${A}.tar.gz" -o /tmp/k.tgz; \
    tar -xzf /tmp/k.tgz -C /tmp; \
    install -m 0755 "/tmp/kopia-${VER}-linux-${A}/kopia" /out/kopia

# distroless/base (not static): carries glibc so the kopia release binary runs
# regardless of its linking. The agentfs-sidecar (CGO-free) runs here too.
FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/agentfs-sidecar /agentfs-sidecar
COPY --from=kopia /out/kopia /usr/bin/kopia
USER nonroot:nonroot
ENTRYPOINT ["/agentfs-sidecar"]
