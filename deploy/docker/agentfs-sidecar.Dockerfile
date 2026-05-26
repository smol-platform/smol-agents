# syntax=docker/dockerfile:1.6
#
# The agentfs-sidecar provides AgentFS durable storage for AgentRun pods:
# `init` restores the mount tree from S3, `serve` runs periodic full-snapshot
# backups to S3. Pure Go (no CGO, no eBPF).

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agentfs-sidecar ./cmd/agentfs-sidecar

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agentfs-sidecar /agentfs-sidecar
USER nonroot:nonroot
ENTRYPOINT ["/agentfs-sidecar"]
