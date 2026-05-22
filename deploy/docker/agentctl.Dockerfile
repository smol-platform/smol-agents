# syntax=docker/dockerfile:1.6
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/agentctl ./cmd/agentctl

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agentctl /agentctl
USER nonroot:nonroot
ENTRYPOINT ["/agentctl"]
