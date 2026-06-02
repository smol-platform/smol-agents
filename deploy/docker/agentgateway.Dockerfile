# syntax=docker/dockerfile:1.6
#
# agentgateway: the stateless HTTP front door for AgentSession turns. It
# publishes incoming turns to NATS JetStream and (optionally) waits for results.
# Deployed as a Knative Service — autoscaled on HTTP concurrency, scale-to-zero
# when idle. Pure Go (CGO-free), so distroless/static is enough.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agentgateway ./cmd/agentgateway

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agentgateway /agentgateway
USER nonroot:nonroot
ENTRYPOINT ["/agentgateway"]
