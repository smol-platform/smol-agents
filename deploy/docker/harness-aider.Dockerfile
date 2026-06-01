# syntax=docker/dockerfile:1.6
#
# harness-aider: a ready-to-run bundle for harness.kind=aider. Carries the Aider
# CLI (`aider`) + git (Aider requires it) + the /agent driver, so an AgentRun can
# use kind=aider with NO custom harness.image. The operator overrides the
# entrypoint to `/agent run`, which spawns `aider --message ... --no-pretty --yes`.

FROM golang:1.26 AS agent
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agent ./cmd/agent

FROM python:3.12-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir aider-chat \
    && aider --version
COPY --from=agent /out/agent /agent
ENV HOME=/tmp
USER 65532:65532
ENTRYPOINT ["/agent"]
