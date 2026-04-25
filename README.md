# cuadra-core

Backend monorepo for Cuadra — operating system for small/medium gyms in Mexico and LatAm.

Two binaries share the same domain and app layers:

- `cmd/server` — cloud API (Go + Gin + GORM + PostgreSQL). Runs on Hetzner.
- `cmd/sidecar` — local sidecar embedded in the Tauri desktop app (Go + sqlx + SQLite).

See [`../CUADRA-SPEC.md`](../CUADRA-SPEC.md) for vision and architecture, [`../CUADRA-USE-CASES.md`](../CUADRA-USE-CASES.md) for use cases, and [`../adr/`](../adr/) for ADRs.

## Quick start

```bash
# 1. Boot Postgres
make docker-up

# 2. Apply migrations
cp .env.example .env
make migrate-postgres
make migrate-sqlite

# 3. Run cloud server (port 8080)
make run-server

# 4. Run local sidecar (port 9090)
make run-sidecar

# 5. Tests
make test
```

## Layout

```
cmd/
  server/     # cloud binary (build tag: server)
  sidecar/    # local binary (build tag: sidecar)
src/
  modules/<bc>/{app,domain,infraestructure,interfaces}
  shared/{domain,sync,middleware,biometric,audit,utils,auth,email,whatsapp}
infraestructure/db/    # connection helpers
db_migrations/{postgres,sqlite}/
```

Bounded contexts: `gyms`, `users`, `members`, `billing`, `products`, `checkins`, `notifications`.

## Build tags

- `server` — Postgres + GORM repos. Cloud-only.
- `sidecar` — SQLite + sqlx repos, sync queue write-through, biometric SDK shim.
- `!sidecar` — biometric mock.

`domain` and `app` layers carry no tags — shared.

## Status

- Sesión 1 (UC-001 to UC-010) implemented end-to-end for `gyms` + `users`.
- Sesiones 2-8: domain skeleton only. See [IMPLEMENTATION_NOTES.md](IMPLEMENTATION_NOTES.md).
