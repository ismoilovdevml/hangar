# syntax=docker/dockerfile:1.7
#
# hangar-scanner: Hangar's image scan adapter on the official Trivy image.
# It runs the Trivy client (client/server mode) for the images Hangar asks
# to scan; the Trivy server is a separate container. Build from the
# repository root:
#
#   docker build -f deploy/scanner/Dockerfile -t hangar-scanner:dev \
#     --build-arg VERSION=$(git describe --tags --always) .
#
# TRIVY_IMAGE pins the Trivy client; keep it on the same version as the
# Trivy server (deploy/compose/compose.yaml, the Helm chart).

ARG GO_IMAGE=golang:1.27-alpine
ARG TRIVY_IMAGE=aquasec/trivy:0.76.1

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=""
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/hangar-scanner ./cmd/hangar-scanner
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    export CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOFLAGS=-buildvcs=false; \
    go build -trimpath \
      -ldflags "-s -w -buildid= \
        -X github.com/ismoilovdevml/hangar/internal/version.Version=${VERSION} \
        -X github.com/ismoilovdevml/hangar/internal/version.Commit=${COMMIT}" \
      -o /out/usr/local/bin/hangar-scanner ./cmd/hangar-scanner; \
    mkdir -p /out/var/lib/hangar-scanner

# No RUN below: the target-platform stage only copies, so multi-arch builds
# need no emulation.
FROM ${TRIVY_IMAGE}
ARG VERSION=dev
ARG COMMIT=""
LABEL org.opencontainers.image.title="Hangar scanner" \
      org.opencontainers.image.description="Image scan adapter for Hangar (Trivy client/server mode)" \
      org.opencontainers.image.source="https://github.com/ismoilovdevml/hangar" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
COPY --from=build /out/usr/local/bin/hangar-scanner /usr/local/bin/hangar-scanner
COPY --from=build --chown=10001:10001 /out/var/lib/hangar-scanner /var/lib/hangar-scanner
ENV HANGAR_SCANNER_ADDR=":4955" \
    HANGAR_SCANNER_CACHE_DIR="/var/lib/hangar-scanner" \
    HOME="/var/lib/hangar-scanner"
USER 10001:10001
WORKDIR /var/lib/hangar-scanner
EXPOSE 4955
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
  CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:4955/healthz"]
ENTRYPOINT ["/usr/local/bin/hangar-scanner"]
