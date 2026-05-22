# syntax=docker/dockerfile:1.6
#
# memory-worker — retrieval worker data plane for smol-agents-memory.
# Serves the internal retrieval API (HTTP+JSON over mTLS), owns
# embedding, chunking, indexing, ranking, and backend adapters.
# Listens on :8444 by default. Implements R-MEM-WORK-1 / R-MEM-WORK-2.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=arm64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/memory-worker ./cmd/memory-worker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/memory-worker /memory-worker
EXPOSE 8444
USER nonroot:nonroot
ENTRYPOINT ["/memory-worker"]
