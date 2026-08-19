GO ?= go
PG_IMAGE ?= postgres:17-alpine
# Debian rather than Alpine: -race needs cgo, and so a C toolchain.
GO_IMAGE ?= golang:1.26.6
TEST_NET ?= imapped-test
TEST_PG ?= imapped-pgtest
TEST_PG_URL ?= postgres://imapped:imapped@$(TEST_PG):5432/postgres?sslmode=disable

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary
	$(GO) build -o bin/imapped ./cmd/imapped

.PHONY: test
test: ## Run unit tests (hermetic, no Docker needed)
	$(GO) test ./... -race -shuffle=on

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w .

.PHONY: lint
lint: ## Run vet and golangci-lint
	$(GO) vet ./...
	golangci-lint run

.PHONY: tidy
tidy: ## Tidy module dependencies
	$(GO) mod tidy

## Integration tests
##
## These need a real Postgres. If IMAPPED_TEST_PG_URL is exported the tests use
## it directly; otherwise `test-integration` starts a throwaway server and runs
## the suite inside a container attached to the same Docker network.
##
## The container-in-network arrangement exists because published container ports
## are not always reachable from the host running the tests (sandboxed shells,
## rootless Docker, some CI runners). Addressing Postgres by container name on a
## user-defined network sidesteps that entirely.

.PHONY: pg-up
pg-up: ## Start the throwaway Postgres used by integration tests
	@docker network create $(TEST_NET) >/dev/null 2>&1 || true
	@docker rm -f $(TEST_PG) >/dev/null 2>&1 || true
	@docker run -d --rm --name $(TEST_PG) --network $(TEST_NET) \
		-e POSTGRES_USER=imapped -e POSTGRES_PASSWORD=imapped -e POSTGRES_DB=postgres \
		$(PG_IMAGE) >/dev/null
	@printf 'waiting for postgres'
	@# First start runs initdb and restarts the server, which on a busy host can
	@# take well over half a minute, so allow two full minutes before giving up.
	@for i in $$(seq 1 240); do \
		docker exec $(TEST_PG) pg_isready -U imapped >/dev/null 2>&1 && { echo ' ready'; exit 0; }; \
		printf '.'; sleep 0.5; \
	done; echo ' timed out'; docker logs --tail 20 $(TEST_PG); exit 1

.PHONY: pg-down
pg-down: ## Stop the throwaway Postgres
	@docker rm -f $(TEST_PG) >/dev/null 2>&1 || true
	@docker network rm $(TEST_NET) >/dev/null 2>&1 || true

.PHONY: test-integration
test-integration: pg-up ## Run integration tests against a throwaway Postgres
	@docker run --rm --network $(TEST_NET) \
		-v "$(PWD)":/src -w /src \
		-v "$(HOME)/go/pkg/mod":/go/pkg/mod \
		-e IMAPPED_TEST_PG_URL="$(TEST_PG_URL)" \
		-e GOFLAGS=-buildvcs=false \
		$(GO_IMAGE) go test -tags integration ./... -race; \
	status=$$?; $(MAKE) pg-down; exit $$status

.PHONY: docker
docker: ## Build the container image
	docker build -t imapped:dev .

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin/ cover.out
