# syntax=docker/dockerfile:1.6
#
# harness-goose: a ready-to-run bundle for harness.kind=goose. Carries the Block
# Goose CLI (`goose`) + git + the /agent driver, so an AgentRun can use
# kind=goose with NO custom harness.image. The operator overrides the entrypoint
# to `/agent run`, which spawns `goose run --instructions ...`.
#
# Goose ships as a prebuilt binary (no npm/pip); we fetch the stable release per
# target arch.

FROM golang:1.26 AS agent
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agent ./cmd/agent

FROM debian:stable-slim
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends git curl ca-certificates bzip2 \
    && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) A=x86_64 ;; \
      arm64) A=aarch64 ;; \
      *) echo "unsupported arch: $TARGETARCH"; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/block/goose/releases/download/stable/goose-${A}-unknown-linux-gnu.tar.bz2" -o /tmp/goose.tar.bz2; \
    tar -xjf /tmp/goose.tar.bz2 -C /usr/local/bin; \
    rm /tmp/goose.tar.bz2; \
    chmod 0755 /usr/local/bin/goose; \
    /usr/local/bin/goose --version
COPY --from=agent /out/agent /agent
ENV HOME=/tmp
USER 65532:65532
ENTRYPOINT ["/agent"]
