# syntax=docker/dockerfile:1.6
#
# memory-mcp — MCP gateway for the smol-agents memory subsystem.
# Serves streamable-HTTP MCP, enforces JWT-SVID auth, per-retriever
# policy, quota, and forwards to the retrieval worker over the internal
# HTTP+JSON API (mTLS in production). Listens on :8443 by default.
# Implements R-MEM-MCP-1 / R-MEM-MCP-2 / R-MEM-AUTH-1 / R-MEM-AUTH-2.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=arm64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/memory-mcp ./cmd/memory-mcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/memory-mcp /memory-mcp
EXPOSE 8443
USER nonroot:nonroot
ENTRYPOINT ["/memory-mcp"]
