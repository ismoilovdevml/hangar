# Hangar build and development tasks. Run `make help` for the list.

SHELL := /bin/bash
.DEFAULT_GOAL := help

GO          ?= go
NPM         ?= npm
COMPOSE     ?= docker compose -f deploy/docker-compose.yml
BIN_DIR     := bin
BINARY      := $(BIN_DIR)/hangar
PKG         := github.com/ismoilovdevml/hangar
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
               -X $(PKG)/internal/version.Version=$(VERSION) \
               -X $(PKG)/internal/version.Commit=$(COMMIT) \
               -X $(PKG)/internal/version.Date=$(DATE)
IMAGE       ?= hangar:dev
# Explicit package roots: web/node_modules may contain stray Go packages.
GO_PACKAGES := ./cmd/... ./internal/... ./test/...
WEB_PRESENT := $(wildcard web/package.json)

# Local development defaults for dev/run/migrate (values already in the
# environment win). Matches deploy/docker-compose.yml.
DEV_ENV := HANGAR_DATABASE_URL="$${HANGAR_DATABASE_URL:-postgres://hangar:hangar@localhost:5432/hangar?sslmode=disable}" \
           HANGAR_STORAGE_FS_PATH="$${HANGAR_STORAGE_FS_PATH:-./data/blobs}" \
           HANGAR_NATS_DATA_DIR="$${HANGAR_NATS_DATA_DIR:-./data/nats}" \
           HANGAR_LOG_FORMAT="$${HANGAR_LOG_FORMAT:-text}"

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## ---- Development -------------------------------------------------------

.PHONY: dev
dev: dev-deps ## Start dependencies, then hangar (live reload if `air` is installed) and the web dev server
	@trap 'kill 0' INT TERM EXIT; \
	if [ -n "$(WEB_PRESENT)" ]; then $(NPM) --prefix web run dev & fi; \
	if command -v air >/dev/null 2>&1; then \
		$(DEV_ENV) air --build.cmd "$(GO) build -o $(BINARY) ./cmd/hangar" --build.bin "$(BINARY) serve --migrate" \
		    --build.include_ext "go,sql,yaml" --build.exclude_dir "web,bin,data,deploy,docs"; \
	else \
		echo "tip: install github.com/air-verse/air for live reload"; \
		$(DEV_ENV) $(GO) run ./cmd/hangar serve --migrate; \
	fi

.PHONY: dev-deps
dev-deps: ## Start postgres, minio, nats and trivy in docker
	$(COMPOSE) up -d --wait postgres minio nats trivy
	$(COMPOSE) up minio-init

.PHONY: dev-down
dev-down: ## Stop the docker dependencies (keeps volumes)
	$(COMPOSE) down

.PHONY: run
run: build ## Build and run hangar serve --migrate against local dependencies
	$(DEV_ENV) $(BINARY) serve --migrate

.PHONY: migrate
migrate: ## Apply database migrations (HANGAR_DATABASE_URL)
	$(DEV_ENV) $(GO) run ./cmd/hangar migrate up

.PHONY: migrate-status
migrate-status: ## Show migration status
	$(DEV_ENV) $(GO) run ./cmd/hangar migrate status

## ---- Build -------------------------------------------------------------

.PHONY: build
build: ## Build bin/hangar (without the embedded web UI)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/hangar

.PHONY: build-embed
build-embed: web-build ## Build bin/hangar with web/dist embedded
	find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R web/dist/. internal/webui/dist/
	CGO_ENABLED=0 $(GO) build -trimpath -tags webui -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/hangar

.PHONY: docker
docker: ## Build the container image ($(IMAGE))
	docker build -t $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

## ---- Quality -----------------------------------------------------------

.PHONY: test
test: test-go test-web ## Run Go and web unit tests

.PHONY: test-go
test-go: ## go test ./... (set HANGAR_TEST_DATABASE_URL for database tests)
	$(GO) test -race -count=1 $(GO_PACKAGES)

.PHONY: cover
cover: ## Go tests with coverage report (coverage.out)
	$(GO) test -count=1 -coverprofile=coverage.out $(GO_PACKAGES)
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: e2e
e2e: ## Real-client acceptance tests against HANGAR_E2E_URL (needs docker)
	@if [ -z "$$HANGAR_E2E_URL" ]; then echo "set HANGAR_E2E_URL (e.g. http://localhost:8080)"; exit 1; fi
	$(GO) test -tags e2e -count=1 -v ./test/e2e/...

.PHONY: lint
lint: lint-go lint-web ## golangci-lint, eslint and tsc

.PHONY: lint-go
lint-go: ## golangci-lint
	golangci-lint run $(GO_PACKAGES)

.PHONY: vuln
vuln: ## govulncheck
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest $(GO_PACKAGES)

.PHONY: fmt
fmt: ## Format Go code (gofmt + goimports via golangci-lint)
	golangci-lint fmt $(GO_PACKAGES)
	@if [ -n "$(WEB_PRESENT)" ] && $(NPM) --prefix web run | grep -q '^  format$$'; then $(NPM) --prefix web run format; fi

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

## ---- Web UI (web/) -----------------------------------------------------

define web_guard
	@if [ -z "$(WEB_PRESENT)" ]; then echo "web/package.json not found; skipping $(1)"; exit 0; fi;
endef

.PHONY: web-install
web-install: ## npm ci in web/
	$(call web_guard,web-install) $(NPM) --prefix web ci

.PHONY: web-dev
web-dev: ## Vite dev server
	$(call web_guard,web-dev) $(NPM) --prefix web run dev

.PHONY: web-build
web-build: ## Production build into web/dist
	$(call web_guard,web-build) $(NPM) --prefix web run build

.PHONY: web-lint
web-lint: ## eslint
	$(call web_guard,web-lint) $(NPM) --prefix web run lint

.PHONY: web-typecheck
web-typecheck: ## tsc --noEmit
	$(call web_guard,web-typecheck) $(NPM) --prefix web run typecheck

.PHONY: web-test
web-test: ## vitest
	$(call web_guard,web-test) $(NPM) --prefix web run test

.PHONY: test-web
test-web: web-test

.PHONY: lint-web
lint-web: web-lint web-typecheck

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR) coverage.out
	find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
