# syntax=docker/dockerfile:1.6
# spiffe-probe — in-cluster SPIFFE assertion runner for the L1 e2e ring.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/spiffe-probe ./cmd/spiffe-probe

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/spiffe-probe /spiffe-probe
USER nonroot:nonroot
ENTRYPOINT ["/spiffe-probe"]
