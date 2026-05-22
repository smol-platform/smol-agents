# syntax=docker/dockerfile:1.6
#
# fake-tts — stand-in Tokenetes Transaction Token Service (RFC 8693 token
# exchange + JWKS) for the secretless-egress fullstack-e2e scenario.
# Implements R-E2E-SCN-SECRETLESS.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/fake-tts ./cmd/fake-tts

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fake-tts /fake-tts
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/fake-tts"]
