.PHONY: help tidy build build-server build-sidecar run-server run-sidecar dev-server dev-sidecar test test-unit test-integration vet fmt fmt-check docker-up docker-down migrate-postgres migrate-sqlite migrate-reset-postgres migrate-reset-sqlite migrate-reset seed import-gym clean

# --- Defaults ---
DB_URL ?= postgresql://tinta:tinta_dev@localhost:5432/tinta?sslmode=disable
SIDECAR_DB ?= ./tmp/tinta.db

# Rutas alternas donde el sidecar SQLite puede vivir en dev:
#   - ./tmp/tinta.db                       — cuando lo corres con
#                                             `make run-sidecar` (Makefile-relativo)
#   - ../tmp/cuadra.db                     — historical / repo-root cwd
#   - ../cuadra-desktop/tmp/cuadra.db      — cuando Tauri (pnpm tauri dev)
#                                             spawnea al sidecar; su cwd queda
#                                             en cuadra-desktop, y el sidecar
#                                             defaultea a ./tmp/cuadra.db
# Las tres se limpian en `migrate-reset-sqlite`. La principal (la que
# se vuelve a poblar con migraciones) sigue siendo $(SIDECAR_DB).
EXTRA_SIDECAR_DBS := ../tmp/tinta.db ../cuadra-desktop/tmp/tinta.db

help:
	@echo "Targets:"
	@echo "  build              Build both binaries (server + sidecar)"
	@echo "  build-server       Build cmd/server with -tags server"
	@echo "  build-sidecar      Build cmd/sidecar with -tags sidecar"
	@echo "  run-server         Run cloud server"
	@echo "  run-sidecar        Run local sidecar"
	@echo "  dev-server         Run cloud server with auto-reload (air)"
	@echo "  dev-sidecar        Run local sidecar with auto-reload (air)"
	@echo "  test               Run all tests"
	@echo "  test-unit          Run only unit tests (skip integration)"
	@echo "  test-integration   Run integration tests (requires Postgres)"
	@echo "  vet                go vet ./..."
	@echo "  fmt                gofmt -w ."
	@echo "  fmt-check          gofmt -l ."
	@echo "  docker-up          Start Postgres via docker compose"
	@echo "  docker-down        Stop docker services"
	@echo "  migrate-postgres        Apply all postgres migrations"
	@echo "  migrate-sqlite          Apply all sqlite migrations to local file"
	@echo "  migrate-reset-postgres  Drop public schema and re-apply postgres migrations"
	@echo "  migrate-reset-sqlite    Remove sidecar SQLite file and re-apply migrations"
	@echo "  migrate-reset           Reset both databases (postgres + sqlite) — for tests"
	@echo "  import-gym              Migrate legacy phpMyAdmin gym dump (see cmd/import-gym)"
	@echo "  clean              Remove tmp/ and bin/"

tidy:
	go mod tidy

build: build-server build-sidecar

build-server:
	go build -tags server -o bin/tinta-server ./cmd/server

build-sidecar:
	go build -tags sidecar -o bin/cuadra-sidecar ./cmd/sidecar

run-server:
	go run -tags server ./cmd/server

run-sidecar:
	mkdir -p tmp
	go run -tags sidecar ./cmd/sidecar

# `make dev-*` — auto-reload via air (https://github.com/air-verse/air).
# Watches *.go and rebuilds + restarts the binary on save. Configs live in
# .air.server.toml / .air.sidecar.toml. We resolve the binary path directly
# because make's shell often runs without the user's interactive PATH (so a
# `command -v air` lookup misses an air installed via `go install`).
AIR := $(shell command -v air 2>/dev/null)
ifeq ($(AIR),)
AIR := $(shell go env GOPATH)/bin/air
endif

dev-server:
	@test -x "$(AIR)" || { echo "air not found at $(AIR) — install with: go install github.com/air-verse/air@latest"; exit 1; }
	$(AIR) -c .air.server.toml

dev-sidecar:
	@test -x "$(AIR)" || { echo "air not found at $(AIR) — install with: go install github.com/air-verse/air@latest"; exit 1; }
	mkdir -p tmp
	$(AIR) -c .air.sidecar.toml

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

# `migrate-reset-*` — destructivo. Pensado para volver a una BD vacía
# durante pruebas (tests de integración + smoke E2E de retos, etc.).
# Nunca correr contra producción.
migrate-reset-postgres:
	@echo "→ dropping schema public on $(DB_URL)"
	psql "$(DB_URL)" -v ON_ERROR_STOP=1 -c "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;"
	@$(MAKE) --no-print-directory migrate-postgres

migrate-reset-sqlite:
	@# Hay que matar al sidecar antes del rm. Por defecto, en Unix el
	@# rm borra el dirent pero el inode persiste mientras el proceso
	@# tenga el archivo abierto — el sidecar sigue escribiendo a un
	@# inode huérfano (a veces incluso aparece en .Trash) y el `make
	@# migrate-sqlite` posterior recrea un archivo nuevo que NADIE está
	@# usando. Resultado: el dueño cree que reseteó y el sidecar sigue
	@# viendo el estado viejo. Esto es exactamente el bug que perdimos
	@# horas debuggeando — automatizo el cleanup para que no recurra.
	@# Si `air` watchea la BD, va a respawnear; si no, hay que rearrancar
	@# `make dev-sidecar` manualmente.
	@pids=$$(pgrep -f "(cuadra|tinta)-sidecar" 2>/dev/null || true); \
	if [ -n "$$pids" ]; then \
		echo "→ killing sidecar process(es) holding DB open: $$pids"; \
		kill $$pids 2>/dev/null || true; \
		sleep 1; \
	fi
	@echo "→ removing sidecar db $(SIDECAR_DB)"
	rm -f $(SIDECAR_DB) $(SIDECAR_DB)-shm $(SIDECAR_DB)-wal
	@for extra in $(EXTRA_SIDECAR_DBS); do \
		if [ -e "$$extra" ] || [ -e "$$extra-wal" ] || [ -e "$$extra-shm" ]; then \
			echo "→ also removing $$extra (alt sidecar location)"; \
			rm -f "$$extra" "$$extra-wal" "$$extra-shm"; \
		fi; \
	done
	@$(MAKE) --no-print-directory migrate-sqlite
	@echo "→ sqlite reseteado. Si tenías 'make dev-sidecar' corriendo, rearráncalo."

migrate-reset: migrate-reset-postgres migrate-reset-sqlite

# `make seed` — populate a clean Postgres with one demo gym + members in
# every dashboard category. Reuses the existing migrations runner.
seed:
	DATABASE_URL="$(DB_URL)" go run -tags server ./cmd/seed

# `make import-gym` — migrate a phpMyAdmin MariaDB dump (`gym.sql`) from a
# legacy gym system into Cuadra. Two modes:
#
#   1. Reuse an existing gym/owner you already created via the dashboard:
#        make import-gym GYM_ID=<uuid> OWNER_ID=<uuid>
#
#   2. Have the importer create everything from scratch (gym + owner) using
#      the legacy `configuracion` row + the email/password you provide:
#        make import-gym CREATE_MISSING=1 EMAIL=tu@gym.com PASSWORD=Secret123!
#
#   Optional flags: DUMP=path/to/gym.sql, RESET=1 (wipes prior data first),
#   DRY_RUN=1 (parse + print stats only).
DUMP ?= ../gym.sql
DRY_RUN_FLAG := $(if $(DRY_RUN),--dry-run,)
RESET_FLAG := $(if $(RESET),--reset,)
CREATE_FLAG := $(if $(CREATE_MISSING),--create-missing,)
GYM_ID_FLAG := $(if $(GYM_ID),--gym-id $(GYM_ID),)
OWNER_ID_FLAG := $(if $(OWNER_ID),--owner-user-id $(OWNER_ID),)
EMAIL_FLAG := $(if $(EMAIL),--owner-email $(EMAIL),)
PASSWORD_FLAG := $(if $(PASSWORD),--owner-password $(PASSWORD),)
GYM_NAME_FLAG := $(if $(GYM_NAME),--gym-name "$(GYM_NAME)",)
BACKFILL_FLAG := $(if $(BACKFILL),--backfill-projector,)

import-gym:
	DATABASE_URL="$(DB_URL)" go run -tags server ./cmd/import-gym \
		--dump $(DUMP) \
		$(GYM_ID_FLAG) $(OWNER_ID_FLAG) \
		$(CREATE_FLAG) $(EMAIL_FLAG) $(PASSWORD_FLAG) $(GYM_NAME_FLAG) \
		$(RESET_FLAG) $(DRY_RUN_FLAG) $(BACKFILL_FLAG)

clean:
	rm -rf tmp bin
