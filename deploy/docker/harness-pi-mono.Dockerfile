# syntax=docker/dockerfile:1.6
# harness-pi-mono: bundles /agent + /pi-bridge + the pi coding agent CLI (M4.17).
# pi-mono runs as a harness — /agent run drives the pi-bridge over loopback HTTP.
# Multiarch: bare ARG TARGETARCH (BuildKit fills it) — no default.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agent ./cmd/agent && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/pi-bridge ./cmd/pi-bridge

FROM node:22-slim
ARG PI_VERSION=latest
RUN npm i -g --ignore-scripts @earendil-works/pi-coding-agent@${PI_VERSION} \
    && pi --version
COPY --from=build /out/agent /agent
COPY --from=build /out/pi-bridge /pi-bridge
# Non-root, matching the run-pod PSA (uid 65532).
USER 65532:65532
ENTRYPOINT ["/agent"]
