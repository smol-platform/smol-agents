# syntax=docker/dockerfile:1.6
#
# fake-github — stand-in GitHub App API + REST resource for the
# secretless-egress fullstack-e2e scenario. Implements R-E2E-SCN-SECRETLESS.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags '-s -w' -o /out/fake-github ./cmd/fake-github

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fake-github /fake-github
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/fake-github"]
