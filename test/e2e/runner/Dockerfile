# syntax=docker/dockerfile:1.7
#
# Every client the real-client acceptance tests (test/e2e) call, in one
# image: docker CLI with buildx, npm, pnpm, yarn, python3 (pip, venv), uv,
# poetry, mvn, gradle, go, helm, dotnet, cargo, crane, cosign, curl, wget.
# test/e2e/run-docker.sh builds it and runs `go test -tags e2e` inside it
# against a Hangar container; the host's Docker daemon is reached through
# /var/run/docker.sock.
#
#   docker build -t hangar-e2e-runner:dev test/e2e/runner
#
# Versions are pinned by build argument; bump them together with the tests.

ARG GO_IMAGE=golang:1.27-bookworm
ARG NODE_IMAGE=node:22-bookworm-slim
ARG DOCKER_CLI_IMAGE=docker:28-cli
ARG UV_IMAGE=ghcr.io/astral-sh/uv:0.9.5

FROM ${NODE_IMAGE} AS node
FROM ${DOCKER_CLI_IMAGE} AS dockercli
FROM ${UV_IMAGE} AS uv

FROM ${GO_IMAGE}
ARG TARGETARCH
ARG HELM_VERSION=v4.3.1
ARG GRADLE_VERSION=8.14.3
ARG DOTNET_CHANNEL=10.0
ARG COSIGN_VERSION=v2.6.1
ARG CRANE_VERSION=v0.20.6
ARG PNPM_VERSION=10.20.0
ARG YARN_VERSION=1.22.22
ARG POETRY_VERSION=2.2.1

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git unzip xz-utils jq \
      python3 python3-venv python3-pip \
      openjdk-17-jdk-headless maven \
      libicu72 \
 && rm -rf /var/lib/apt/lists/*

# Node.js (npm, npx, corepack), pnpm and yarn classic.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && npm install -g --no-audit --no-fund "pnpm@${PNPM_VERSION}" "yarn@${YARN_VERSION}"

# Docker CLI and the buildx plugin (the daemon is the host's).
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx

# uv and poetry.
COPY --from=uv /uv /uvx /usr/local/bin/
ENV UV_TOOL_DIR=/opt/uv-tools UV_TOOL_BIN_DIR=/usr/local/bin
RUN uv tool install "poetry==${POETRY_VERSION}"

# Gradle.
RUN curl -fsSL -o /tmp/gradle.zip "https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip" \
 && unzip -q /tmp/gradle.zip -d /opt \
 && ln -s "/opt/gradle-${GRADLE_VERSION}/bin/gradle" /usr/local/bin/gradle \
 && rm /tmp/gradle.zip

# .NET SDK (the NuGet test targets net10.0).
ENV DOTNET_ROOT=/usr/share/dotnet DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
RUN curl -fsSL -o /tmp/dotnet-install.sh https://dot.net/v1/dotnet-install.sh \
 && bash /tmp/dotnet-install.sh --channel "${DOTNET_CHANNEL}" --install-dir "${DOTNET_ROOT}" \
 && ln -s "${DOTNET_ROOT}/dotnet" /usr/local/bin/dotnet \
 && rm /tmp/dotnet-install.sh

# Rust (cargo).
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo PATH=/usr/local/cargo/bin:$PATH
RUN curl -fsSL https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain stable --no-modify-path

# Helm, crane and cosign.
RUN curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz" | tar -xz -C /tmp \
 && mv "/tmp/linux-${TARGETARCH}/helm" /usr/local/bin/helm \
 && rm -r "/tmp/linux-${TARGETARCH}" \
 && curl -fsSL -o /usr/local/bin/cosign "https://github.com/sigstore/cosign/releases/download/${COSIGN_VERSION}/cosign-linux-${TARGETARCH}" \
 && chmod +x /usr/local/bin/cosign \
 && GOBIN=/usr/local/bin go install "github.com/google/go-containerregistry/cmd/crane@${CRANE_VERSION}" \
 && go clean -cache -modcache

# Every client must answer; a broken install fails the build, not the tests.
RUN set -e; for c in docker npm pnpm yarn python3 uv poetry mvn gradle go helm dotnet cargo crane cosign curl wget; do \
      command -v "$c" >/dev/null || { echo "missing $c" >&2; exit 1; }; \
    done; docker buildx version; helm version; dotnet --version; cargo --version

WORKDIR /src
