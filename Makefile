.PHONY: help tidy build build-server build-sidecar run-server run-sidecar test test-unit test-integration vet fmt fmt-check docker-up docker-down migrate-postgres migrate-sqlite migrate-reset-postgres clean

# --- Defaults ---
DB_URL ?= postgresql://cuadra:cuadra_dev@localhost:5432/cuadra?sslmode=disable
SIDECAR_DB ?= ./tmp/cuadra.db

help:
	@echo "Targets:"
	@echo "  build              Build both binaries (server + sidecar)"
	@echo "  build-server       Build cmd/server with -tags server"
	@echo "  build-sidecar      Build cmd/sidecar with -tags sidecar"
	@echo "  run-server         Run cloud server"
	@echo "  run-sidecar        Run local sidecar"
	@echo "  test               Run all tests"
	@echo "  test-unit          Run only unit tests (skip integration)"
	@echo "  test-integration   Run integration tests (requires Postgres)"
	@echo "  vet                go vet ./..."
	@echo "  fmt                gofmt -w ."
	@echo "  fmt-check          gofmt -l ."
	@echo "  docker-up          Start Postgres via docker compose"
	@echo "  docker-down        Stop docker services"
	@echo "  migrate-postgres   Apply all postgres migrations"
	@echo "  migrate-sqlite     Apply all sqlite migrations to local file"
	@echo "  clean              Remove tmp/ and bin/"

tidy:
	go mod tidy

build: build-server build-sidecar

build-server:
	go build -tags server -o bin/cuadra-server ./cmd/server

build-sidecar:
	go build -tags sidecar -o bin/cuadra-sidecar ./cmd/sidecar

run-server:
	go run -tags server ./cmd/server

run-sidecar:
	mkdir -p tmp
	go run -tags sidecar ./cmd/sidecar

test:
	go test -tags "server sidecar" ./...

test-unit:
	go test -short ./...

test-integration:
	go test -tags server -run Integration ./...

vet:
	go vet -tags server ./...
	go vet -tags sidecar ./...

fmt:
	gofmt -w .

fmt-check:
	@if [ -n "$$(gofmt -l .)" ]; then \
		echo "Files not formatted:"; \
		gofmt -l .; \
		exit 1; \
	fi

docker-up:
	docker compose up -d

docker-down:
	docker compose down

migrate-postgres:
	@for f in db_migrations/postgres/*.sql; do \
		echo "applying $$f"; \
		psql "$(DB_URL)" -v ON_ERROR_STOP=1 -f $$f; \
	done

migrate-sqlite:
	mkdir -p $$(dirname $(SIDECAR_DB))
	@for f in db_migrations/sqlite/*.sql; do \
		echo "applying $$f"; \
		sqlite3 $(SIDECAR_DB) < $$f; \
	done

migrate-reset-postgres:
	psql "$(DB_URL)" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"

clean:
	rm -rf tmp bin
