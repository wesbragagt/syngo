# syntax=docker/dockerfile:1

# The image one end-to-end device runs in. PID 1 is `sleep infinity` so the
# harness can start, kill and restart the daemon inside a container that stays
# up, and can still run `syncthis status` after a crash.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=e2e
ARG COMMIT=e2e
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/wesbragagt/syncthis/internal/cli.Version=${VERSION} -X github.com/wesbragagt/syncthis/internal/cli.Commit=${COMMIT}" \
      -o /out/syncthis ./cmd/syncthis

FROM alpine:3.21
# iproute2 for tc netem, iptables for pairwise partitions, coreutils for the
# `touch -d` the clock test needs.
RUN apk add --no-cache iproute2 iptables coreutils
COPY --from=build /out/syncthis /usr/local/bin/syncthis
ENV SYNCTHIS_DATA_DIR=/data
VOLUME ["/data", "/docs"]
EXPOSE 22000
ENTRYPOINT ["sleep", "infinity"]
