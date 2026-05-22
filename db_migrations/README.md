# Migrations

Dos sets paralelos, uno por destino:

- `postgres/` — schema del cloud. Lo aplica `ApplyPostgresMigrations` al arrancar `cmd/server`.
- `sqlite/` — schema del sidecar. Lo aplica `ApplySQLiteMigrations` al arrancar `cmd/sidecar`.

Ambos comparten numeración semántica (001 = init, 002 = notifications, etc.) pero **no son archivos espejo**. Hay tablas que viven sólo en cloud y otras que viven sólo en el sidecar; la lista cuelga abajo.

## Tablas cloud-only

Estas tablas existen únicamente en Postgres. El sidecar las consume vía `sync pull` (cuando el cloud las proyecta a `sync_entities`) y las lee localmente desde la copia sincronizada, pero **nunca escribe en ellas desde el dispositivo**.

| Tabla | Migration | Razón |
|---|---|---|
| `subscription_events` | 007 | Webhooks de Stripe/MP. El sidecar no procesa cobros. |
| `_migrations` (postgres) | runner | Tracking de versión propio del cloud. SQLite tiene el suyo. |
| `sidecar_credentials` | 004 | sk_live_ tokens; el cloud es source of truth (ADR-008). |
| `installer_bootstraps` | 006 | `cb_install_*` codes; sólo el cloud los mintea. |
| `email_verification_tokens` | 022 | Token de verify-email; flujo cloud-only. |
| `refresh_token_blacklist` | 001 | JWT revocation; el sidecar no firma refresh tokens. |
| `audit_log` (centralizado) | 001 | El sidecar emite eventos vía sync; el ledger vive en cloud. |

## Tablas sidecar-only

Estas tablas existen únicamente en SQLite. Sirven para state local del dispositivo y nunca se sincronizan.

| Tabla | Migration sqlite | Razón |
|---|---|---|
| `sync_queue` | 003 | Cola local de cambios pendientes de push. |
| `sync_state` | 003 | Cursores `LastPulledAt`, cached login bcrypt, sk_live_ activo. |

## Drift conocido — sidecar 5 migraciones atrás

A día de hoy (mayo 2026) el sidecar va por la 017 y el cloud por la 022. Las cinco migraciones cloud-only que no tienen contraparte SQLite son:

- 018 `gyms_whatsapp_unique`
- 019 `operators_pin_first`
- 020 `fingerprints_multi`
- 021 `fix_uq_users_email`
- 022 `email_verification`

Algunas de ellas (operators_pin_first, fingerprints_multi) **sí afectan tablas que el sidecar sincroniza** (`users`, `member_fingerprints`). El sidecar las tiene reemplazadas con migrations equivalentes propias (016 y 017 en sqlite/) — verificar que ambas evolucionen en paralelo cuando se añadan nuevas. Si una migración cloud altera el shape de una tabla sincronizada y no hay réplica sqlite, el projector cloud rechazará pushes del sidecar (columna desconocida) o el pull aplicará a una tabla SQLite con un schema viejo.

## Reglas

1. Cualquier migración que altere una tabla en `SyncedTables` (ver `src/shared/sync/tables.go`) debe tener réplica en `sqlite/`.
2. Las migraciones cloud-only viven sólo en `postgres/` (subscription_events, installer_bootstraps, etc.).
3. La numeración puede divergir: no es problema que sidecar/017 corresponda funcionalmente a cloud/020. Lo importante es que para cada tabla SyncedTable el shape esté alineado.
4. Si dudas si una tabla es SyncedTable: `grep <table_name> src/shared/sync/tables.go`. Si aparece, es bi-direccional y necesita ambas migraciones.
