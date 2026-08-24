SHELL := /bin/sh

GO ?= go
COMPOSE ?= docker compose
REDOCLY ?= npx --yes @redocly/cli@2.9.0
COMPOSE_FILE := deploy/compose/compose.yaml
COMPOSE_WAIT_TIMEOUT ?= 120
SERVICES := payment router gateway simulator
BIN_DIR := bin

.DEFAULT_GOAL := help

.PHONY: help fmt format tidy-check vet test test-race test-integration test-e2e contracts-check build \
	docker-build compose-config compose-up compose-down compose-logs clean ci

help: ## Show the available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Fail when Go source files are not formatted.
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*')"; \
	if [ -n "$$files" ]; then \
		unformatted="$$(gofmt -l $$files)"; \
		if [ -n "$$unformatted" ]; then \
			printf '%s\n' "$$unformatted"; \
			exit 1; \
		fi; \
	fi

format: ## Format all Go source files in place.
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*')"; \
	if [ -n "$$files" ]; then gofmt -w $$files; fi

tidy-check: ## Verify that module metadata is already tidy.
	$(GO) mod tidy -diff

vet: ## Run the standard Go static analysis.
	$(GO) vet ./...

test: ## Run the unit test suite.
	$(GO) test ./...

test-race: ## Run tests with the race detector.
	$(GO) test -race ./...

test-integration: ## Run tests that require real infrastructure.
	$(GO) test -tags=integration ./...

test-e2e: ## Run the end-to-end payment flow against the Compose stack.
	$(GO) test -count=1 -tags=e2e ./test/e2e

contracts-check: ## Validate every OpenAPI description with Redocly CLI.
	$(REDOCLY) lint contracts/public/payment-api.openapi.yaml \
		contracts/internal/router-api.openapi.yaml \
		contracts/internal/provider-gateway-api.openapi.yaml \
		contracts/internal/provider-simulator.openapi.yaml

build: ## Build every deployable service binary.
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -o "$(BIN_DIR)/payment" ./services/payment/cmd/api
	$(GO) build -trimpath -o "$(BIN_DIR)/router" ./services/router/cmd/server
	$(GO) build -trimpath -o "$(BIN_DIR)/gateway" ./services/provider-gateway/cmd/server
	$(GO) build -trimpath -o "$(BIN_DIR)/simulator" ./tools/provider-simulator/cmd/server
	@if [ -d ./services/payment/cmd/worker ]; then \
		$(GO) build -trimpath -o "$(BIN_DIR)/payment-worker" ./services/payment/cmd/worker; \
	fi

docker-build: ## Build every service container image.
	@set -eu; for service in $(SERVICES); do \
		docker build --build-arg "SERVICE=$$service" \
			-f deploy/Dockerfile -t "payments/$$service:local" .; \
	done

compose-config: ## Validate the rendered Compose configuration.
	$(COMPOSE) -f $(COMPOSE_FILE) config --quiet

compose-up: ## Start the complete local stack.
	$(COMPOSE) -f $(COMPOSE_FILE) up --build --wait --wait-timeout $(COMPOSE_WAIT_TIMEOUT)

compose-down: ## Stop the local stack without deleting database data.
	$(COMPOSE) -f $(COMPOSE_FILE) down

compose-logs: ## Follow logs from the local stack.
	$(COMPOSE) -f $(COMPOSE_FILE) logs --follow

clean: ## Remove local build output.
	rm -rf $(BIN_DIR) coverage

ci: fmt tidy-check vet test test-race build compose-config ## Run the pull-request verification suite.
