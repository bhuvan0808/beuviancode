# Beuvian development tasks.
#
# A Makefile rather than a pile of scripts because it gives one discoverable
# entry point (`make help`) across three modules with different toolchains, and
# because CI and a contributor's laptop should run the identical commands. Where a
# task needs real shell logic (cross-compilation, checksums) it delegates to
# scripts/, which are also usable standalone on Windows.
#
# Windows users: run these under Git Bash or WSL, or use the PowerShell scripts in
# scripts/ directly. `make` is not part of a stock Windows install.

# Fail loudly. -e exits on error, -u catches typo'd variables, pipefail stops a
# failure inside a pipeline being masked by a successful tail.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

MODULES := shared backend agent

# GOWORK=off for every Go target, matching CI and Docker. Building through the
# workspace can succeed while a clean single-module clone fails, and that is
# precisely the failure mode we want to catch locally rather than in CI.
export GOWORK := off

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
DATE    ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)

VERSION_PKG := github.com/bhuvan0808/beuviancode/shared/version
LDFLAGS := -w -s \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).Date=$(DATE)

COMPOSE := docker compose -f docker/docker-compose.yml

.DEFAULT_GOAL := help

# ---------------------------------------------------------------------------

.PHONY: help
help: ## Show this help
	@echo "Beuvian — $(VERSION)"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# --- Verification ----------------------------------------------------------

.PHONY: build
build: ## Build every module independently
	@for m in $(MODULES); do echo "--- build $$m"; (cd $$m && go build ./...); done

.PHONY: test
test: ## Run every test with the race detector
	@for m in $(MODULES); do echo "--- test $$m"; (cd $$m && go test -race ./...); done

.PHONY: test-cover
test-cover: ## Run tests and report coverage per module
	@for m in $(MODULES); do \
		echo "--- cover $$m"; \
		(cd $$m && go test -race -coverprofile=coverage.out -covermode=atomic ./... >/dev/null \
			&& go tool cover -func=coverage.out | tail -n 1); \
	done

.PHONY: vet
vet: ## Run go vet on every module
	@for m in $(MODULES); do echo "--- vet $$m"; (cd $$m && go vet ./...); done

.PHONY: fmt
fmt: ## Format all Go code
	gofmt -w $(MODULES)

.PHONY: fmt-check
fmt-check: ## Fail if any Go file is unformatted (what CI runs)
	@unformatted=$$(gofmt -l $(MODULES)); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-formatted:"; echo "$$unformatted"; \
		echo "run: make fmt"; exit 1; \
	fi; \
	echo "all files formatted"

.PHONY: tidy
tidy: ## Tidy go.mod in every module
	@for m in $(MODULES); do echo "--- tidy $$m"; (cd $$m && go mod tidy); done

.PHONY: lint
lint: ## Run golangci-lint on every module
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed:"; \
		echo "  go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
		exit 1; }
	@for m in $(MODULES); do \
		echo "--- lint $$m"; \
		(cd $$m && golangci-lint run --config=$(CURDIR)/.golangci.yml ./...); \
	done

.PHONY: vuln
vuln: ## Scan for known vulnerabilities
	@command -v govulncheck >/dev/null 2>&1 || go install golang.org/x/vuln/cmd/govulncheck@latest
	@for m in $(MODULES); do echo "--- vuln $$m"; (cd $$m && govulncheck ./...); done

.PHONY: check
check: fmt-check vet build test ## Everything CI checks, in CI's order
	@echo
	@echo "all checks passed"

# --- Cross-module invariants -------------------------------------------------

.PHONY: verify-shared-deps
verify-shared-deps: ## Enforce the shared module's zero-dependency invariant (ADR-0003)
	@if grep -qE '^[[:space:]]*require' shared/go.mod; then \
		echo "ERROR: shared/go.mod has gained a dependency."; \
		echo "shared is imported by both binaries; see docs/adr/0003-shared-module-is-protocol-only.md"; \
		exit 1; \
	fi; \
	echo "shared has no third-party dependencies"

.PHONY: verify-modules-standalone
verify-modules-standalone: ## Prove each module builds with no workspace
	@for m in $(MODULES); do \
		echo "--- standalone $$m"; \
		(cd $$m && GOWORK=off go build ./...); \
	done
	@echo "every module compiles independently"

# --- Running ----------------------------------------------------------------

.PHONY: run-backend
run-backend: ## Run the backend from source
	cd backend && go run ./cmd/server

.PHONY: run-agent
run-agent: ## Run the Desktop Agent from source
	cd agent && go run ./cmd/beuvian-agent

.PHONY: config-check
config-check: ## Validate both binaries' configuration and exit
	cd backend && go run ./cmd/server -check
	cd agent   && go run ./cmd/beuvian-agent -check

.PHONY: detect
detect: ## List AI coding agents installed on this machine
	cd agent && go run ./cmd/beuvian-agent -detect

.PHONY: migrate
migrate: ## Apply pending database migrations
	cd backend && go run ./cmd/server -migrate

.PHONY: devtoken
devtoken: ## Mint a development access token (refuses outside development)
	@cd backend && go run ./cmd/devtoken

.PHONY: register-agent
register-agent: ## Register this machine as a device (needs a token on stdin)
	@echo "Paste an access token from \`make devtoken\`:"
	cd agent && go run ./cmd/beuvian-agent -register

.PHONY: test-integration
test-integration: ## Run integration tests (needs `make infra` first)
	@if [ -z "$$BEUVIAN_TEST_DB_URL" ]; then \
		echo "set BEUVIAN_TEST_DB_URL, e.g."; \
		echo "  export BEUVIAN_TEST_DB_URL='postgres://beuvian:beuvian_local_dev@127.0.0.1:5432/beuvian?sslmode=disable'"; \
		exit 1; \
	fi
	cd backend && go test -tags=integration -count=1 ./...

# --- Artifacts --------------------------------------------------------------

.PHONY: build-agent
build-agent: ## Build the agent for the host platform
	./scripts/build-agent.sh

.PHONY: build-agent-all
build-agent-all: ## Cross-compile the agent for all six release targets
	./scripts/build-agent.sh --target all --version $(VERSION)

.PHONY: docker-build
docker-build: ## Build the backend image
	docker build -f docker/backend.Dockerfile \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(DATE) \
		-t beuvian-backend:$(VERSION) -t beuvian-backend:dev .

# --- Local stack ------------------------------------------------------------

.PHONY: up
up: ## Start Postgres, Redis and the backend
	$(COMPOSE) up -d --build
	@echo
	@$(COMPOSE) ps

.PHONY: down
down: ## Stop the local stack (volumes preserved)
	$(COMPOSE) down

.PHONY: reset
reset: ## Stop the local stack and DELETE all local data
	@echo "This deletes the local Postgres and Redis volumes."
	@read -p "Continue? [y/N] " ok && [ "$$ok" = "y" ] || exit 1
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail local stack logs
	$(COMPOSE) logs -f

.PHONY: infra
infra: ## Start ONLY Postgres and Redis (run the backend from source)
	$(COMPOSE) up -d postgres redis

# --- Knowledge graph --------------------------------------------------------

.PHONY: graph
graph: ## Rebuild the graphify knowledge graph (AST only, no LLM tokens)
	@command -v graphify >/dev/null 2>&1 || { echo "pip install graphifyy && graphify install"; exit 1; }
	graphify update || graphify .

# --- Housekeeping -----------------------------------------------------------

.PHONY: clean
clean: ## Remove build artifacts and coverage files
	rm -rf dist bin
	@for m in $(MODULES); do rm -f $$m/coverage.out $$m/coverage.html; done
	@echo "cleaned"

.PHONY: version
version: ## Print resolved build metadata
	@echo "version : $(VERSION)"
	@echo "commit  : $(COMMIT)"
	@echo "date    : $(DATE)"
