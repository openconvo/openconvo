SHELL := /bin/bash

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X github.com/openconvo/openconvo/internal/version.Version=$(VERSION) \
           -X github.com/openconvo/openconvo/internal/version.Commit=$(COMMIT) \
           -X github.com/openconvo/openconvo/internal/version.Date=$(DATE)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: web ## Build the openconvo binary with the frontend embedded
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/openconvo ./cmd/openconvo

.PHONY: web
web: ## Build the frontend into internal/web/dist
	rm -rf internal/web/dist && mkdir -p internal/web/dist && touch internal/web/dist/.gitkeep
	cd web && npm install --no-fund --no-audit && npm run build

.PHONY: screenshots
screenshots: web ## Regenerate documentation screenshots with synthetic data (needs Chrome)
	node scripts/screenshots/main.mjs

.PHONY: test
test: ## Run Go tests (database tests skip without TEST_DATABASE_URL)
	go test ./...

.PHONY: test-db
test-db: ## Run all Go tests against an ephemeral PostgreSQL container
	./scripts/test-db.sh

.PHONY: lint
lint: ## gofmt + go vet + frontend type-check
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	cd web && npm run lint

.PHONY: fmt
fmt: ## Format Go code
	gofmt -w .

.PHONY: dev
dev: ## Run the server from source (expects DATABASE_URL in the environment)
	go run ./cmd/openconvo serve

.PHONY: dev-web
dev-web: ## Run the Vite dev server (proxies /api to :8080)
	cd web && npm run dev

.PHONY: docker
docker: ## Build the Docker image
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t openconvo:dev .

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin internal/web/dist/* web/dist
	@touch internal/web/dist/.gitkeep
