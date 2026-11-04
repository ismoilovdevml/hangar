# syntax=docker/dockerfile:1.7
#
# Hangar container image: web UI build -> Go build (UI embedded) -> distroless.
#
#   docker build -t hangar:dev \
#     --build-arg VERSION=$(git describe --tags --always) \
#     --build-arg COMMIT=$(git rev-parse --short=12 HEAD) \
#     --build-arg SOURCE_DATE_EPOCH=$(git log -1 --format=%ct) .
#
# Reproducible: the same sources, build args and base images produce the same
# binaries (-trimpath, no VCS stamping, build date taken from
# SOURCE_DATE_EPOCH). Pin the base images by digest (NODE_IMAGE, GO_IMAGE,
# RUNTIME_IMAGE) for byte-identical layers.

ARG NODE_IMAGE=node:22-alpine
ARG GO_IMAGE=golang:1.27-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

# ---- Web UI ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund
COPY web/ ./
RUN rm -rf dist && npm run build

# ---- Go build ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=""
# Unix time of the commit; empty leaves the build date unset ("unknown").
ARG SOURCE_DATE_EPOCH=""
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY deploy/healthcheck ./deploy/healthcheck
COPY --from=web /src/web/dist ./internal/webui/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    test -f internal/webui/dist/index.html; \
    date=""; \
    if [ -n "${SOURCE_DATE_EPOCH}" ]; then date=$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ); fi; \
    export CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOFLAGS=-buildvcs=false; \
    go build -trimpath -tags webui \
      -ldflags "-s -w -buildid= \
        -X github.com/ismoilovdevml/hangar/internal/version.Version=${VERSION} \
        -X github.com/ismoilovdevml/hangar/internal/version.Commit=${COMMIT} \
        -X github.com/ismoilovdevml/hangar/internal/version.Date=${date}" \
      -o /out/usr/local/bin/hangar ./cmd/hangar; \
    go build -trimpath -ldflags "-s -w -buildid=" \
      -o /out/usr/local/bin/hangar-healthcheck ./deploy/healthcheck; \
    mkdir -p /out/var/lib/hangar/blobs /out/var/lib/hangar/nats /out/etc/hangar

# ---- Runtime ----------------------------------------------------------------
FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
ARG COMMIT=""
LABEL org.opencontainers.image.title="Hangar" \
      org.opencontainers.image.description="Self-hosted package registry" \
      org.opencontainers.image.source="https://github.com/ismoilovdevml/hangar" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
COPY --from=build /out/usr/local/bin/ /usr/local/bin/
COPY --from=build --chown=65532:65532 /out/var/lib/hangar /var/lib/hangar
COPY --from=build /out/etc/hangar /etc/hangar
# Single-node defaults: filesystem blobs and embedded NATS under /var/lib/hangar.
# Set HANGAR_DATABASE_URL and HANGAR_SERVER_BASE_URL; see hangar.example.yaml.
ENV HANGAR_SERVER_ADDR=":8080" \
    HANGAR_STORAGE_FS_PATH="/var/lib/hangar/blobs" \
    HANGAR_NATS_DATA_DIR="/var/lib/hangar/nats"
USER 65532:65532
WORKDIR /var/lib/hangar
VOLUME ["/var/lib/hangar"]
EXPOSE 8080
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=15s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/hangar-healthcheck"]
ENTRYPOINT ["/usr/local/bin/hangar"]
CMD ["serve"]
