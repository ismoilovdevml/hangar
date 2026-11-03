# syntax=docker/dockerfile:1.7
# Multi-stage build: web UI (if web/ exists) -> Go binary -> distroless runtime.

ARG GO_VERSION=1.27
ARG NODE_VERSION=22

# ---- Web UI ---------------------------------------------------------------
# Builds web/dist when web/package.json exists; otherwise produces an empty
# directory and the binary serves its built-in fallback page.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS web
WORKDIR /src
RUN --mount=type=bind,source=.,target=/ctx \
    --mount=type=cache,target=/root/.npm \
    mkdir -p /out && \
    if [ -f /ctx/web/package.json ]; then \
      mkdir -p web && cp -R /ctx/web/. web/ && rm -rf web/node_modules web/dist && \
      cd web && npm ci --no-audit --no-fund && npm run build && cp -R dist/. /out/; \
    else \
      echo "web/package.json not found: building without the web UI"; \
    fi

# ---- Go build ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=""
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /out ./internal/webui/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags webui \
      -ldflags "-s -w \
        -X github.com/ismoilovdevml/hangar/internal/version.Version=${VERSION} \
        -X github.com/ismoilovdevml/hangar/internal/version.Commit=${COMMIT} \
        -X github.com/ismoilovdevml/hangar/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /out/hangar ./cmd/hangar && \
    mkdir -p /out/data/blobs /out/data/nats

# ---- Runtime ----------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="Hangar" \
      org.opencontainers.image.description="Self-hosted package registry" \
      org.opencontainers.image.source="https://github.com/ismoilovdevml/hangar"
COPY --from=build /out/hangar /usr/local/bin/hangar
COPY --from=build --chown=65532:65532 /out/data /var/lib/hangar
ENV HANGAR_SERVER_ADDR=":8080" \
    HANGAR_STORAGE_FS_PATH="/var/lib/hangar/blobs" \
    HANGAR_NATS_DATA_DIR="/var/lib/hangar/nats"
USER 65532:65532
VOLUME ["/var/lib/hangar"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/hangar"]
CMD ["serve"]
