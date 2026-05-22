# syntax=docker/dockerfile:1.6
#
# fake-llm — deterministic LLM server for the fullstack-e2e L0 ring.
# Implements R-E2E-L0-3.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# TARGETOS/TARGETARCH are auto-populated by BuildKit from the build platform
# (host arch for a plain `docker build`, or each --platform for buildx). Do NOT
# give TARGETARCH a default — a hardcoded default overrides BuildKit's value and
# silently cross-builds the wrong arch (e.g. an arm64 binary on an amd64 runner
# -> "exec format error"). GOARCH falls back to the host arch when unset.
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/fake-llm ./cmd/fake-llm

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fake-llm /fake-llm
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/fake-llm"]
