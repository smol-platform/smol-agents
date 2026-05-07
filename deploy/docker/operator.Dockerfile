# syntax=docker/dockerfile:1.6
#
# The operator image runs the Kubebuilder-built control plane. It needs
# no eBPF, no Kata, no SPIRE — just network egress to the api-server.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/operator ./operator/cmd/manager

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/operator /operator
USER nonroot:nonroot
ENTRYPOINT ["/operator"]
