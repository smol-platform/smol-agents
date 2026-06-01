# syntax=docker/dockerfile:1.6
#
# harness-codex: a ready-to-run bundle for harness.kind=codex. Carries the
# OpenAI Codex CLI (`codex`) + git + a shell + the /agent driver, so an AgentRun
# can use kind=codex with NO custom harness.image. The operator overrides the
# entrypoint to `/agent run`, which spawns `codex exec ...`.

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
ARG CODEX_VERSION=latest
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force \
    && codex --version
COPY --from=agent /out/agent /agent
ENV HOME=/tmp
USER 65532:65532
ENTRYPOINT ["/agent"]
