# syntax=docker/dockerfile:1.6
# agentterminal: the M4.10 terminal attach gateway. Multiarch: bare ARG
# TARGETARCH (BuildKit fills it) — no default (see memory: multiarch_images).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agentterminal ./cmd/agentterminal

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agentterminal /agentterminal
USER nonroot:nonroot
ENTRYPOINT ["/agentterminal"]
