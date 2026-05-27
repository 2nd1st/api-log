# Multi-stage build for api-log. Pure Go module (modernc.org/sqlite is
# pure Go, no cgo), so the runtime image can be distroless / static.

FROM golang:1.22-alpine AS build
WORKDIR /src

# Cache module downloads on their own layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, stripped binary; no cgo.
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -trimpath \
    -o /out/api-log \
    ./cmd/api-log

# Pre-create an empty /data dir so when a named volume mounts on top of
# it Docker inherits this dir's ownership (UID 65532 = distroless
# nonroot). Without this trick, fresh named volumes are root-owned and
# the nonroot container can't write into them.
RUN mkdir -p /out/data && chown -R 65532:65532 /out/data

# Distroless runtime: no shell, no package manager, just the binary.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/api-log /usr/local/bin/api-log
COPY --from=build --chown=65532:65532 /out/data /data

# data dir as a writable volume mount point.
USER nonroot:nonroot
VOLUME ["/data"]
ENV APILOG_STORAGE_DATA_DIR=/data

EXPOSE 7861 7862

ENTRYPOINT ["/usr/local/bin/api-log"]
