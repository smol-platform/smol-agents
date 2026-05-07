# syntax=docker/dockerfile:1.6
#
# The ebpf-loader image is intentionally larger than agent/secret-proxy
# because it ships the compiled BPF objects and runs as a privileged
# DaemonSet. It does NOT need cgo: cilium/ebpf is pure Go.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/ebpf-loader ./cmd/ebpf-loader

# BPF objects are built outside the image (make bpf) and copied in here.
# We package them into the loader image so a single artifact represents
# kernel-side-of-the-world for a given release.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ebpf-loader /ebpf-loader
COPY bpf/build/*.bpf.o /usr/share/knative-agents/bpf/
# DaemonSet needs to write to /sys/fs/bpf and host paths; running as
# nonroot keeps userland boundaries even when CAP_BPF/CAP_SYS_ADMIN
# are granted at the container level.
USER 0:0
ENTRYPOINT ["/ebpf-loader"]
