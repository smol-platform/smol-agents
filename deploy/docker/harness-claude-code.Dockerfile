# syntax=docker/dockerfile:1.6
#
# harness-claude-code: a ready-to-run bundle for harness.kind=claude-code.
# Carries the Claude Code CLI (`claude`) + git + a shell + the /agent driver, so
# an AgentRun can use kind=claude-code with NO custom harness.image. The operator
# overrides the entrypoint to `/agent run`, which spawns `claude --print ...`.
#
# Runs as the pod's non-root uid (65532, set by the operator securityContext);
# HOME points at a writable path so the CLI can keep its config/cache.

FROM golang:1.26 AS agent
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agent ./cmd/agent

FROM node:22-slim
ARG CLAUDE_CODE_VERSION=latest
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && npm cache clean --force \
    && claude --version
COPY --from=agent /out/agent /agent
ENV HOME=/tmp
USER 65532:65532
ENTRYPOINT ["/agent"]
