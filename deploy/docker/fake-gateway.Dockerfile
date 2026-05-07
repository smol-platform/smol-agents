# syntax=docker/dockerfile:1.6
#
# fake-gateway — SVID-aware echo gateway for the fullstack-e2e suite.
# Implements R-E2E-L0-4.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=arm64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/fake-gateway ./cmd/fake-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fake-gateway /fake-gateway
EXPOSE 8080 8443
USER nonroot:nonroot
ENTRYPOINT ["/fake-gateway"]
