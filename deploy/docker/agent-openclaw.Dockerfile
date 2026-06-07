# syntax=docker/dockerfile:1.6
# agent-openclaw: an OpenClaw agent-loop daemon image (M4.22). Bundles /agent
# (the harness driver) + the openclaw CLI + a headless browser for its tools.
# OpenClaw binds its control plane on :18789; a tiny :8080 readiness shim answers
# the serving-path probe. Multiarch: bare ARG TARGETARCH (BuildKit fills it).
#
# DEPLOYMENT-GATED: kata + a long-running Node daemon + headless browser is heavy
# and not broadly live-proven here — smoke-test on a real cluster. Pin the npm
# version. :18789 is the full control plane: in-pod/mesh only.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agent ./cmd/agent

FROM node:24-slim
ARG OPENCLAW_VERSION=latest
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends git ca-certificates chromium; \
    rm -rf /var/lib/apt/lists/*; \
    npm i -g --ignore-scripts openclaw@${OPENCLAW_VERSION}
COPY --from=build /out/agent /agent
# :8080 readiness shim — OpenClaw serves its own control plane on :18789, so the
# serving-path probe needs a separate liveness endpoint. The start shim links the
# operator-rendered openclaw.json into ~/.openclaw before launching the daemon.
COPY deploy/docker/openclaw-entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh
USER 65532:65532
ENTRYPOINT ["/entrypoint.sh"]
