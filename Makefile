VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/jamshids/rillock/internal/version.Version=$(VERSION)
ADDR ?= 127.0.0.1:8082
DATABASE_URL ?= postgres://rillock:rillock@127.0.0.1:55432/rillock?sslmode=disable
KEYS_FILE ?= .rillock/keys.yaml
LINT_VERSION := v2.14.0

.PHONY: build install tools test test-db lint fmt run dev-key reload-keys migrate db-up db-down db-reset psql python-test check

build:
	go build -ldflags "$(LDFLAGS)" -o bin/rillock ./cmd/rillock

# Installs rillock into ~/go/bin so you can type `rillock` anywhere.
# Run it again after code changes, or you will run an old version.
install:
	go install -ldflags "$(LDFLAGS)" ./cmd/rillock

# Database tests skip themselves unless RILLOCK_TEST_DATABASE_URL is set.
test:
	go test -race ./...

# Runs every test, including database tests. Needs `make db-up`.
test-db:
	RILLOCK_TEST_DATABASE_URL="$(DATABASE_URL)" go test -race ./...

# Installs development tools into ~/go/bin.
tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINT_VERSION)

# Runs go vet, staticcheck, and the other linters in .golangci.yml.
lint:
	@command -v golangci-lint >/dev/null || (echo "golangci-lint not found: run 'make tools'"; exit 1)
	golangci-lint run ./...

# Formats code and sorts imports.
fmt:
	golangci-lint fmt ./...

# Runs the server against the dev database. Needs `make db-up`, `make migrate`, and `make dev-key`.
run: build
	./bin/rillock server --addr $(ADDR) --database-url "$(DATABASE_URL)" --keys-file $(KEYS_FILE)

# Tells a running server to re-read its keys file (SIGHUP). The pattern
# matches only processes whose first word is the rillock binary, not the
# shell `make run` starts it from: a shell would exit on SIGHUP.
reload-keys:
	pkill -HUP -f '^[^ ]*rillock server' || (echo "no running rillock server found"; exit 1)

# Creates an admin + approver key for local use and prints it.
dev-key: build
	@mkdir -p $(dir $(KEYS_FILE))
	./bin/rillock keys generate --name dev --roles admin,approver --keys-file $(KEYS_FILE)

migrate: build
	./bin/rillock migrate --database-url "$(DATABASE_URL)"

db-up:
	docker compose up -d --wait postgres

db-down:
	docker compose down

# Deletes all local data.
db-reset:
	docker compose down -v

psql:
	docker compose exec postgres psql -U rillock rillock

# The same Python checks CI runs.
python-test:
	cd python && ruff check . && python -m pytest

check: lint test python-test
