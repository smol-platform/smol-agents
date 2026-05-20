# syntax=docker/dockerfile:1.6
#
# ebpf-probe: one-shot Pod image for the L2 EBPF-DROP / EBPF-REDIR
# scenarios. Carries the same CO-RE .bpf.o objects the ebpf-loader
# ships, loads them at runtime, attaches the cgroup-class programs
# to its own cgroup, then exercises the egress filter from the host
# side. Privileged DaemonSet-style (CAP_BPF + CAP_SYS_ADMIN).

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/ebpf-probe ./cmd/ebpf-probe

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ebpf-probe /ebpf-probe
COPY bpf/build/*.bpf.o /usr/share/smol-agents/bpf/
USER 0:0
ENTRYPOINT ["/ebpf-probe"]
