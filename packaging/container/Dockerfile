# syntax=docker/dockerfile:1

# The release image: one container runs one syncthis daemon.
#
# This is not internal/e2edocker/Dockerfile. That image exists for the
# end-to-end suite: its PID 1 is `sleep infinity` so the harness can kill and
# restart the daemon inside it, and it carries iproute2 and iptables so a test
# can slow or partition a link. None of that belongs in a release image, where
# PID 1 is the daemon itself and the only tools present are the ones it needs.
#
# Build from the repository root:
#   docker build -f packaging/container/Dockerfile \
#     --build-arg VERSION=$(git describe --tags --always --dirty) \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) -t syncthis:dev .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG COMMIT=unknown
# CGO_ENABLED=0 for the same reason the release target uses it: the binary must
# not name an interpreter that only the build machine has.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/wesbragagt/syncthis/internal/cli.Version=${VERSION} -X github.com/wesbragagt/syncthis/internal/cli.Commit=${COMMIT}" \
      -o /out/syncthis ./cmd/syncthis

FROM alpine:3.21
# tzdata only: sync windows are wall-clock times in the daemon's local zone, so
# without it every TZ setting reads as UTC. Nothing else is installed.
RUN apk add --no-cache tzdata \
 && adduser -D -u 1000 -h /data syncthis \
 && mkdir -p /data /folders \
 && chown syncthis:syncthis /data /folders
COPY --from=build /out/syncthis /usr/local/bin/syncthis

USER syncthis
ENV SYNCTHIS_DATA_DIR=/data
# /data is the device identity and database, /folders the synchronized trees.
VOLUME ["/data", "/folders"]
# The daemon binds this port for peers. It refuses a public bind without
# --allow-public, so inside a container give it the container address:
#   docker run ... syncthis:dev daemon run --interface eth0
EXPOSE 22000
ENTRYPOINT ["/usr/local/bin/syncthis"]
CMD ["daemon", "run"]
