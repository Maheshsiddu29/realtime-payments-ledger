# Realtime Payments Ledger — developer entrypoints.
# Run `make help` for the full list.

MODULE      := github.com/Maheshsiddu29/realtime-payments-ledger
BINARY      := api
BIN_DIR     := bin
COMPOSE     := docker compose

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildDate=$(BUILD_DATE)

GO_PACKAGES := ./...

# Integration tests are guarded by the `integration` build tag, so the default
# `go test ./...` needs no infrastructure and stays fast. Anything tagged
# requires a running PostgreSQL (make infra-up).
INTEGRATION_TAG := integration

# The deterministic concurrency tests. These run in CI; the large stress
# scenarios live in cmd/stress and are run on demand.
CONCURRENCY_TESTS := 'TestConcurrent|TestOpposing|TestLockOrder|TestMultiAccount|TestProgressive|TestBusinessRejections|TestValidationFailures|TestCancelledContext'

# The Redis-backed idempotency tests, including the same-key concurrency case.
IDEMPOTENCY_TESTS := 'TestSameKey|TestDifferentKeys|TestRetryAfter|TestLostResponse|TestRedis|TestCrash|TestStale|TestPostgresUniquenessAlone|TestRejected|TestInvalidIdempotency|TestIdempotentPosts'

# The gRPC transport tests: authentication, authorization and the API contract.
GRPC_TESTS := 'TestGRPC'

# Protobuf toolchain. Plugin versions are pinned so generated code is
# reproducible; they must match the module versions in go.mod.
PROTOC_GEN_GO_VERSION      := v1.36.12
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1
PROTO_FILES := $(shell find api/proto -name '*.proto')
GOBIN := $(shell go env GOPATH)/bin

# Stress runner defaults, overridable: make stress ATTEMPTS=500 SCENARIO=ring
ATTEMPTS   ?= 100
CONCURRENCY ?= 0
SCENARIO   ?= oneway
AMOUNT     ?= 100

.DEFAULT_GOAL := help

## help: List the available targets.
.PHONY: help
help:
	@echo "Realtime Payments Ledger"
	@echo
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort
	@echo

# ---------------------------------------------------------------------------
# Code quality
# ---------------------------------------------------------------------------

## fmt: Format all Go source in place.
.PHONY: fmt
fmt:
	gofmt -s -w .

## fmt-check: Fail if any Go source is unformatted (used by CI).
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		echo "Run 'make fmt'."; \
		exit 1; \
	fi
	@echo "gofmt: clean"

## vet: Run go vet across the module, including integration-tagged files.
.PHONY: vet
vet:
	go vet $(GO_PACKAGES)
	go vet -tags=$(INTEGRATION_TAG) $(GO_PACKAGES)

## tidy: Reconcile go.mod and go.sum with the imports in the tree.
.PHONY: tidy
tidy:
	go mod tidy

## tidy-check: Fail if go.mod or go.sum would change (used by CI).
.PHONY: tidy-check
tidy-check:
	@cp go.mod go.mod.bak
	@[ -f go.sum ] && cp go.sum go.sum.bak || true
	@go mod tidy
	@if ! diff -q go.mod go.mod.bak >/dev/null; then \
		mv go.mod.bak go.mod; \
		[ -f go.sum.bak ] && mv go.sum.bak go.sum || true; \
		echo "go.mod is out of date. Run 'make tidy'."; \
		exit 1; \
	fi
	@rm -f go.mod.bak go.sum.bak
	@echo "go mod tidy: clean"

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

## test: Run the fast suites (no infrastructure required).
.PHONY: test
test:
	go test $(GO_PACKAGES)

## test-race: Run the fast suites with the race detector enabled.
.PHONY: test-race
test-race:
	go test -race $(GO_PACKAGES)

## test-integration: Run the PostgreSQL integration tests (needs make infra-up).
.PHONY: test-integration
test-integration:
	go test -tags=$(INTEGRATION_TAG) -count=1 $(GO_PACKAGES)

## test-integration-race: Integration tests under the race detector.
.PHONY: test-integration-race
test-integration-race:
	go test -tags=$(INTEGRATION_TAG) -race -count=1 $(GO_PACKAGES)

## test-concurrency: Run the deterministic concurrency tests (needs make infra-up).
.PHONY: test-concurrency
test-concurrency:
	go test -tags=$(INTEGRATION_TAG) -count=1 -timeout 15m -v \
		-run $(CONCURRENCY_TESTS) ./tests/

## test-concurrency-race: Concurrency tests under the race detector.
.PHONY: test-concurrency-race
test-concurrency-race:
	go test -tags=$(INTEGRATION_TAG) -race -count=1 -timeout 20m -v \
		-run $(CONCURRENCY_TESTS) ./tests/

## test-idempotency: Run the Redis idempotency tests (needs make infra-up).
.PHONY: test-idempotency
test-idempotency:
	go test -tags=$(INTEGRATION_TAG) -count=1 -timeout 15m -v \
		-run $(IDEMPOTENCY_TESTS) ./tests/

## test-idempotency-race: Idempotency tests under the race detector.
.PHONY: test-idempotency-race
test-idempotency-race:
	go test -tags=$(INTEGRATION_TAG) -race -count=1 -timeout 20m -v \
		-run $(IDEMPOTENCY_TESTS) ./tests/

## test-grpc: Run the gRPC transport, authentication and authorization tests.
.PHONY: test-grpc
test-grpc:
	go test -tags=$(INTEGRATION_TAG) -count=1 -timeout 15m -v \
		-run $(GRPC_TESTS) ./tests/

## test-grpc-race: gRPC tests under the race detector.
.PHONY: test-grpc-race
test-grpc-race:
	go test -tags=$(INTEGRATION_TAG) -race -count=1 -timeout 20m -v \
		-run $(GRPC_TESTS) ./tests/

## test-all: Run every suite, fast and integration.
.PHONY: test-all
test-all: test test-integration

## cover: Produce coverage.out and a human-readable summary.
.PHONY: cover
cover:
	go test -coverprofile=coverage.out -covermode=atomic $(GO_PACKAGES)
	go tool cover -func=coverage.out | tail -1

## cover-html: Render the coverage profile as coverage.html.
.PHONY: cover-html
cover-html: cover
	go tool cover -html=coverage.out -o coverage.html
	@echo "Wrote coverage.html"

# ---------------------------------------------------------------------------
# Build and run
# ---------------------------------------------------------------------------

## build: Compile every package.
.PHONY: build
build:
	go build $(GO_PACKAGES)

## binary: Build the API binary into bin/api with version metadata.
.PHONY: binary
binary:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) ./cmd/api
	@echo "Built $(BIN_DIR)/$(BINARY) ($(VERSION), $(COMMIT))"

## run: Run the API from source using the local environment.
.PHONY: run
run:
	go run ./cmd/api

## clean: Remove build and coverage artifacts.
.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html

# ---------------------------------------------------------------------------
# Protobuf
# ---------------------------------------------------------------------------
#
# Generated .pb.go files ARE committed: cloning and building must not require
# a protobuf toolchain. CI regenerates and diffs to prove they are current.

## proto-tools: Install the pinned protoc plugins.
.PHONY: proto-tools
proto-tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

## proto: Regenerate Go code from the protobuf definitions.
.PHONY: proto
proto:
	@command -v protoc >/dev/null || { echo "protoc is not installed; see docs/API.md"; exit 1; }
	PATH="$(GOBIN):$$PATH" protoc \
		--proto_path=api/proto \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		$(PROTO_FILES)
	@echo "Generated code is in internal/gen/"

## proto-check: Fail if the committed generated code is out of date (used by CI).
.PHONY: proto-check
proto-check: proto
	@if ! git diff --quiet -- internal/gen; then \
		echo "Generated protobuf code is out of date. Run 'make proto' and commit the result:"; \
		git --no-pager diff --stat -- internal/gen; \
		exit 1; \
	fi
	@echo "generated protobuf code: up to date"

## devtoken: Mint a development JWT and print the matching public key.
.PHONY: devtoken
devtoken:
	@go run ./cmd/devtoken -out ./.devkeys

# ---------------------------------------------------------------------------
# Stress and load testing
# ---------------------------------------------------------------------------
#
# cmd/stress is development tooling: it drives internal/transfer directly and
# creates and funds its own accounts. Point it at a scratch database, never at
# anything you care about:
#
#   POSTGRES_DB=ledger_stress go run ./cmd/migrate up
#   make stress-1000 POSTGRES_DB=ledger_stress

## stress: Run the stress scenario (ATTEMPTS, CONCURRENCY, SCENARIO, AMOUNT).
.PHONY: stress
stress:
	go run ./cmd/stress \
		-scenario $(SCENARIO) -attempts $(ATTEMPTS) \
		-concurrency $(CONCURRENCY) -amount $(AMOUNT)

## stress-1000: Run 1000 simultaneous transfers against one account.
.PHONY: stress-1000
stress-1000:
	go run ./cmd/stress -scenario oneway -attempts 1000 -concurrency 1000

## stress-json: Run the stress scenario and emit JSON on stdout.
.PHONY: stress-json
stress-json:
	@go run ./cmd/stress \
		-scenario $(SCENARIO) -attempts $(ATTEMPTS) \
		-concurrency $(CONCURRENCY) -amount $(AMOUNT) -json

## stress-suite: Run the progressive scenarios used for docs/results.
.PHONY: stress-suite
stress-suite:
	@for n in 10 25 50 100 500 1000; do \
		echo "=== $$n attempts ==="; \
		go run ./cmd/stress -scenario oneway -attempts $$n || exit 1; \
		echo; \
	done

# ---------------------------------------------------------------------------
# Database migrations
# ---------------------------------------------------------------------------
#
# Migrations are an explicit operator step. The API process never migrates on
# start-up. Connection settings come from the POSTGRES_* environment variables.

## migrate-up: Apply all pending migrations.
.PHONY: migrate-up
migrate-up:
	go run ./cmd/migrate up

## migrate-down: Roll back exactly one migration.
.PHONY: migrate-down
migrate-down:
	go run ./cmd/migrate down

## migrate-down-all: Roll back every migration (destroys all data).
.PHONY: migrate-down-all
migrate-down-all:
	go run ./cmd/migrate down-all

## migrate-version: Print the current schema version.
.PHONY: migrate-version
migrate-version:
	go run ./cmd/migrate version

## migrate-redo: Roll everything back and re-apply, verifying both directions.
.PHONY: migrate-redo
migrate-redo: migrate-down-all migrate-up

## psql: Open a psql shell against the configured database.
.PHONY: psql
psql:
	$(COMPOSE) exec postgres psql -U $${POSTGRES_USER:-ledger} -d $${POSTGRES_DB:-ledger}

# ---------------------------------------------------------------------------
# Local infrastructure
# ---------------------------------------------------------------------------

## compose-config: Validate and render the Docker Compose configuration.
.PHONY: compose-config
compose-config:
	$(COMPOSE) config --quiet
	@echo "docker compose config: valid"

## infra-up: Start PostgreSQL, Redis and Kafka and wait until they are healthy.
.PHONY: infra-up
infra-up:
	$(COMPOSE) up -d --wait postgres redis kafka

## redis-up: Start Redis alone and wait until it is healthy.
.PHONY: redis-up
redis-up:
	$(COMPOSE) up -d --wait redis

## redis-cli: Open a redis-cli session against the Compose Redis.
.PHONY: redis-cli
redis-cli:
	$(COMPOSE) exec redis redis-cli

## up: Start the full stack, including the API container.
.PHONY: up
up:
	$(COMPOSE) up -d --build --wait

## down: Stop the stack, leaving volumes intact.
.PHONY: down
down:
	$(COMPOSE) down

## down-volumes: Stop the stack and delete its data volumes.
.PHONY: down-volumes
down-volumes:
	$(COMPOSE) down --volumes

## logs: Follow the logs of every running service.
.PHONY: logs
logs:
	$(COMPOSE) logs -f

## ps: Show the status of the stack.
.PHONY: ps
ps:
	$(COMPOSE) ps

## docker-build: Build the API container image.
.PHONY: docker-build
docker-build:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t payments-ledger/api:$(VERSION) -t payments-ledger/api:latest .

# ---------------------------------------------------------------------------
# Aggregate gates
# ---------------------------------------------------------------------------

## ci: Run the full pre-commit gate (fmt, vet, build, test, race).
.PHONY: ci
ci: fmt-check vet build test test-race
	@echo "All checks passed."

## verify: Full verification: the CI gate, Compose validation and integration tests.
.PHONY: verify
verify: ci compose-config test-integration test-integration-race
	@echo "Full verification passed."

