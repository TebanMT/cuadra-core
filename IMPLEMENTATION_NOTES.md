# cuadra-core — Notas de Implementación, Sesión 1 (Foundations)

Fecha: 2026-04-25

## Qué se implementó

**Sesión 1 (UC-001 a UC-010) end-to-end:**

| UC | Endpoint | Use case |
|---|---|---|
| UC-001 (1) | `POST /api/v1/auth/signup` | `users/app.SignupOwner` |
| UC-001 (2) | `PATCH /api/v1/gyms/me/setup` | `gyms/app.UpdateBasicInfo` |
| UC-001 (3) | `POST /api/v1/membership-types` | `members/app.CreateMembershipType` |
| UC-001 (4) | `PATCH /api/v1/gyms/me/payment-methods` | `gyms/app.UpdatePaymentMethods` |
| UC-001 (5) | `POST /api/v1/gyms/me/setup/complete` | `gyms/app.CompleteSetup` |
| UC-002 | `POST /api/v1/auth/login` | `users/app.Login` |
| UC-003 | `POST /api/v1/auth/logout` | `users/app.Logout` |
| UC-004 (1) | `POST /api/v1/auth/forgot-password` | `users/app.RequestPasswordReset` |
| UC-004 (2) | `POST /api/v1/auth/reset-password` | `users/app.ConfirmPasswordReset` |
| UC-005 | `PATCH /api/v1/gyms/me` | `gyms/app.UpdateProfile` |
| UC-006 | `POST /api/v1/users` | `users/app.CreateOperator` |
| UC-007 | `PATCH /api/v1/users/{id}` | `users/app.UpdateOperator` |
| UC-008 | `PATCH /api/v1/users/{id}/active` | `users/app.ToggleOperatorActive` |
| UC-009 | `POST /api/v1/users/{id}/reset-password` | `users/app.ResetOperatorPassword` |
| UC-010 | `POST /api/v1/gyms/me/transfer-ownership` | `users/app.RequestTransferOwnership` + `ConfirmTransferOwnership` |

**Plumbing compartido:**
- `shared/domain` — `Transaction` + `UnitOfWork` con dos impls (`uow_postgres.go` GORM, `uow_sqlite.go` sqlx). `Command()` auto-rollback; `Query()` no-tx.
- `shared/audit` — `Recorder` con dos impls. Toda mutación de Sesión 1 inserta una fila `audit_log` dentro del mismo `UoW.Command()`.
- `shared/auth` — JWT (HS256, 15m access / 30d refresh), bcrypt cost 12, generador de temp password (UC-006 DA-6.1) y OTP numérico (UC-010).
- `shared/middleware` — `AuthMiddleware`, `RequireOwner`, propagación de IP/UA al ctx.
- `shared/sync` — `SqliteQueue` write-through (sidecar). El sync agent push/pull queda para Sesión 8.
- `shared/email` + `shared/whatsapp` — interfaces + impls "stdout" (loggers) para MVP. Twilio/Resend cuando llegue V1.0.
- `shared/biometric` — interface `Reader`. Mock para `!sidecar`, stub `DigitalPersonaReader` para sidecar (TODO real SDK).

**Migraciones SQL (ADR-002 §3):**
- `db_migrations/postgres/001_init_schema.sql` — TODAS las tablas del modelo de datos (gyms, users, membership_types, members, memberships, membership_adjustments, member_fingerprints, products, stock_movements, payments, sales, sale_items, checkins, contact_attempts, cash_close_events, gym_ownership_transfers, audit_log, conflict_log, notification_queue, password_reset_tokens, ownership_transfer_otps, refresh_token_blacklist, gym_keys).
- `db_migrations/sqlite/001_init_schema.sql` — paridad con Postgres aplicando el mapeo de tipos del ADR-002 §4 (TIMESTAMPTZ → INTEGER ms, NUMERIC → INTEGER cents, JSONB → TEXT con `json_valid`, BOOLEAN → INTEGER, BYTEA → BLOB), más `sync_queue` y `sync_state`.

**Build tags:**
- `//go:build server` → repos Postgres+GORM, recorder Postgres, conexión Postgres, migrate runner Postgres.
- `//go:build sidecar` → repos SQLite+sqlx, recorder SQLite, conexión SQLite, migrate runner SQLite, sync queue, biometric stub.
- `//go:build !sidecar` → biometric mock.
- `domain/` y `app/` no tienen build tags — compartidos.

**Tests (todos verdes):**
- Unit:
  - `users/domain/user/user_test.go` — validación email/password/name + reglas de SetActive (no self-deactivate, no deactivate owner) + role transitions.
  - `gyms/domain/gym/gym_test.go` — wizard transitions, validación RFC, pago, NextSetupStep.
  - `members/domain/membership_type/membership_type_test.go` — invariantes precio/duración/maintenance frequency.
  - `shared/auth/tokens_test.go` — JWT round-trip, bcrypt, generadores de password/OTP.
- Integration (con SQLite real):
  - `users/app/signup_test.go` (build tag `sidecar`) — UC-001 step 1 end-to-end: crea gym + user + audit + sync_queue, valida que un segundo signup con mismo email falla con BusinessError.

## Qué quedó como skeleton

| BC | Estado | UCs futuros |
|---|---|---|
| `members` | parcial — solo `CreateMembershipType` para UC-001 step 3 | UC-011..UC-017 |
| `billing` | doc.go solo | UC-018..UC-022, UC-025..UC-026 |
| `products` | doc.go solo | UC-023..UC-024 |
| `checkins` | doc.go solo | UC-029..UC-032 |
| `notifications` | doc.go solo | UC-037..UC-041 |

Las tablas SQL ya existen (migrations 001) — los UCs solo necesitan domain + app + repos.

## TODO pendientes

| Marcador | Lugar | Resumen |
|---|---|---|
| `TODO(humano)` | `src/shared/biometric/digitalpersona.go` | Integrar SDK real DigitalPersona U.are.U 4500 (ADR-004); por ahora retorna `ErrNotAvailable` para que el binario sidecar compile sin la lib. Bloqueado en UC-028 (Sesión 5). |
| `TODO(humano)` | `src/modules/members/domain/doc.go` | Implementar UC-011 a UC-017 (Sesión 2). |
| `TODO(humano)` | `src/modules/billing/domain/doc.go` | Implementar UC-018..UC-022 + UC-025..UC-026. |
| `TODO(humano)` | `src/modules/products/domain/doc.go` | Implementar UC-023, UC-024. |
| `TODO(humano)` | `src/modules/checkins/domain/doc.go` | Implementar UC-029..UC-032. |
| `TODO(humano)` | `src/modules/notifications/domain/doc.go` | Implementar UC-037..UC-041. |
| Diferido | sync agent (push/pull/full) | El protocolo está documentado en ADR-001 y la infra `sync_queue` write-through ya funciona. Falta el agent goroutine + endpoints `/sync/push`, `/sync/pull`, `/sync/full` (UC-042..UC-045). |
| Diferido | Persistencia GMK (ADR-006 §2.2) | Tabla `gym_keys` ya existe. Falta el flujo de generación al signup + entrega al login + cache local. UC dependiente: UC-028. |
| Diferido | Rate limit del login (UC-002 DA-2.2) | Decidido fuera del use case — irá en Caddy / fail2ban a nivel infraestructura. |
| Diferido | Modo solo-lectura por trial vencido (SPEC §9.3) | Login responde con `trial_ends_at` y `subscription_plan`; el bloqueo de mutaciones se hará con un middleware adicional cuando aterricen los UCs P0 de operación (Sesión 2-4). |

## Decisiones tomadas (cuando había ambigüedad)

1. **`members.CreateMembershipType` se incluyó en Sesión 1** porque la wizard de UC-001 step 3 lo requiere end-to-end. El resto del BC `members` queda skeleton.
2. **Refresh token blacklist global por usuario.** UC-004 invalida "todas las sesiones" sin tener acceso a los refresh tokens vivos del cliente; usamos un row sintético `revoke-all:<user>` en `refresh_token_blacklist` cuya semántica es "todo refresh emitido antes de revoked_at queda inválido". El validador de refresh debe hacer cumplir esta semántica cuando se introduzca el endpoint `/auth/refresh` (Sesión 1+ futuro PR — fuera del scope de UC-001..UC-010 estrictos).
3. **Logout sidecar offline:** el sidecar no tiene `refresh_token_blacklist`. `Logout` recibe `nil` como blacklist y se vuelve no-op local; cuando vuelva la red, el sync agent debería empujar el evento al cloud — pero el endpoint es idempotente, así que también es válido que el cliente lo reintente desde cero la próxima vez que tenga internet.
4. **OTP de transferencia (UC-010):** vive cloud-side. El sidecar tiene un repo análogo (`ownership_transfer_otps_local`, sin migración formal — la tabla se crea on demand) para tests offline; producción usa el endpoint cloud.
5. **`gym_id = id`** en self-reference: enforced con un CHECK constraint en Postgres y SQLite. El UnitOfWork no necesita lógica especial.
6. **JSON columns** (payment_methods, kiosk_settings): codificadas como `json.Marshal` en Go al cruzar el mapper. No usamos tipos PG-específicos para no contaminar el dominio.
7. **Cents en SQLite, NUMERIC en Postgres:** Conversión en el mapper. El dominio expone `float64` por simplicidad (Sesión 1 no opera dinero significativo; Sesión 3 verá si conviene migrar a `shopspring/decimal`).
8. **Nombre del módulo Go:** `github.com/cuadra/cuadra-core` (placeholder editable). Cambiar requiere `go mod edit -module ...` + sed sobre los imports.
9. **Soft warning ≥5 operadores (UC-006 DA-6.2):** no implementado en backend; se considera responsabilidad del frontend (la respuesta de `GET /api/v1/users` muestra el conteo y el cliente decide si banner). Backend solo aplica el hard limit de 10.

## Comandos

```bash
# Setup
cp .env.example .env
make docker-up                # arranca Postgres
make migrate-postgres         # aplica db_migrations/postgres/*.sql
make migrate-sqlite           # crea ./tmp/cuadra.db con el schema

# Run
make run-server               # cmd/server, :8080
make run-sidecar              # cmd/sidecar, 127.0.0.1:9090

# Build (binarios)
make build                    # bin/cuadra-server + bin/cuadra-sidecar

# Quality gates
make test                     # ambos build tags
make vet
make fmt-check

# Smoke test manual
curl -X POST http://localhost:8080/api/v1/auth/signup \
  -H 'Content-Type: application/json' \
  -d '{"full_name":"Esteban Mares","email":"esteban@gym.com","password":"hunter2sufficient","password_confirm":"hunter2sufficient"}'
```

## Estructura de carpetas (verificar con `tree`)

```
cuadra-core/
├── cmd/{server,sidecar}/main.go        # binarios con build tags
├── infraestructure/db/                  # connection + migrate runners
├── db_migrations/{postgres,sqlite}/    # 001_init_schema.sql
├── src/
│   ├── modules/<bc>/{app,domain,infraestructure,interfaces}
│   └── shared/{domain,sync,middleware,biometric,audit,utils,auth,email,whatsapp}
├── docker-compose.yml
├── Makefile
├── README.md
├── IMPLEMENTATION_NOTES.md (este archivo)
├── go.mod / go.sum
└── .env.example
```

## Próxima sesión sugerida

Sesión 2 (UC-011..UC-017, members): Member + Membership + MembershipType (CRUD completo). Las tablas ya existen; falta domain + app + repos + handlers. El patrón de Sesión 1 (UoW.Command, audit, sync queue write-through) se replica directo.

---

# cuadra-core — Notas de Implementación, Sesión 2 (Members)

Fecha: 2026-04-25

## Qué se implementó

**Sesión 2 (UC-011 a UC-017 + UC-032 partial) end-to-end:**

| UC | Endpoint | Use case |
|---|---|---|
| UC-011 (1) | `POST /api/v1/membership-types` | `members/app.CreateMembershipType` |
| UC-011 (2) | `PATCH /api/v1/membership-types/:id` | `members/app.UpdateMembershipType` |
| UC-011 (3) | `DELETE /api/v1/membership-types/:id` (soft) | `members/app.DeactivateMembershipType` |
| UC-011 (4) | `GET /api/v1/membership-types?include_inactive=true` | `members/app.ListMembershipTypes` |
| UC-012 | `POST /api/v1/members` | `members/app.CreateMember` |
| UC-013 | `PATCH /api/v1/members/:id` | `members/app.UpdateMember` |
| UC-014 | `GET /api/v1/members?q&status&plan_id&sort&page&page_size` | `members/app.ListMembers` |
| UC-015 | `GET /api/v1/members/:id` | `members/app.GetMemberDetail` |
| UC-016 | `PATCH /api/v1/members/:id/status` | `members/app.ToggleMemberStatus` |
| UC-017 | `POST /api/v1/memberships/:id/lock-expiry` | `members/app.LockMembershipExpiry` |
| UC-032 (partial) | `POST /api/v1/members/:id/pin` | `members/app.AssignPin` |

**Domain layer (`src/modules/members/domain/`):**
- `member/Member` aggregate con `NewMember`, `ApplyProfileUpdate`, `ChangeStatus`,
  `MarkEnrollmentPaid`, `UpdateLastMaintenance`, `SetPin`, `RegisterContactAttempt`,
  validators (chain) + helpers (`ValidatePhone`, `ValidateEmail`, `ValidatePin`,
  `ValidateStartDate`).
- `membership/Membership` aggregate con `New`, `Renew` (regla UC-018: acumula
  vs reinicia), `MarkReplaced`, `Cancel`, `AdjustExpiry`, `SetExpiry`, `IsActive`,
  `DaysUntilExpiry`. Snapshot fields (`TypeNameSnapshot`, `PriceSnapshot`,
  `DurationDaysSnapshot`) son inmutables — DA-11.1.
- `membership/MembershipAdjustment` entidad histórica con validación de razón ≥5.
- `membership_type/MembershipType` extendido con `Update`, `Deactivate`,
  `Reactivate` y validator chain.
- `access/AccessStatusEvaluator` — domain service puro, retorna
  `allowed_active | allowed_expiring_soon | denied_expired | denied_inactive | denied_no_membership`.
  Threshold "expiring_soon" = 7 días. **Consumido por checkins en Sesión 5**.
- `errors/` — sentinels nuevos para member/membership/adjustment/PIN.
- `repository/` — interfaces para los 4 aggregates + read model
  `MemberWithMembership` + `ListQuery` con filtros/sort/pagination.

**Cross-BC services (`members/app/services.go`):**
- `MemberService.RenewMembershipForPayment` — implementa la mitad de UC-018 que
  vive en `members`. Marca la membresía actual como `replaced`, crea una nueva
  con snapshot del MembershipType (posiblemente diferente) y devuelve ambas.
  **Listo para que billing en Sesión 3 lo invoque dentro de su UoW.Command**.
- `MemberService.GetAccessStatus` — delegación al evaluator. Consumido por
  checkins en Sesión 5.

**Infrastructure layer (`src/modules/members/infraestructure/`):**
- Modelos GORM (`member_model.go`, `membership_model.go`,
  `membership_type_model.go`) — sin tags en domain, solo aquí.
- Repos Postgres+GORM (`*_postgres.go`, `//go:build server`):
  `MemberPostgresRepository`, `MembershipPostgresRepository`,
  `MembershipAdjustmentPostgresRepository`, `MembershipTypePostgresRepository`.
- Repos SQLite+sqlx (`*_sqlite.go`, `//go:build sidecar`) con paridad —
  conversión cents/dates en el mapper (ADR-002 §4) + sync_queue write-through.
- `NextFolio` — Postgres usa `SELECT ... FOR UPDATE` (clause.Locking); SQLite
  confía en su tx serializada.
- `PinHashCollidesInGym` — itera bcrypt hashes del gym y corre `Verify` por PIN.
  O(N_pins) por intento; aceptable para gyms <10k members. Si alguna vez deja
  de serlo, migrar a HMAC peppered hash.

**Interfaces layer (`src/modules/members/interfaces/controllers/`):**
- `MembershipTypeController` — UC-011.
- `MemberController` — UC-012..UC-017 + UC-032 partial.
- Mismo patrón: parsear → use case → mapear response. Errors mapeados con
  `utils.DomainErrorToHttpCode`. Auth middleware en todas.
- UC-017 (lock-expiry) detrás de `RequireOwner()` — DA-17.1.

**DI:** Sólo se *agregaron* repos/use cases/controllers en `cmd/server/main.go`
y `cmd/sidecar/main.go`. Sin refactorizar Sesión 1.

**Tests:**
- Unit (`domain/`): 4 archivos
  (`member_test.go`, `membership_test.go`, `access/evaluator_test.go`,
  `membership_type_test.go` heredado).
  - `Renew()` cubre las dos ramas de UC-018 (acumula vs reinicia).
  - `AccessStatusEvaluator` cubre los 5 estados + threshold ±1.
  - Validators cubren happy path + errores específicos.
- Integration (`app/members_integration_test.go`, build `sidecar`, SQLite real):
  cubre UC-011..UC-017 + AssignPin. Atomicidad de UC-017 verificada
  explícitamente — un reason inválido NO debe mover `expiry_date`.

## Decisiones tomadas (cuando había ambigüedad)

1. **PIN hash collision detection**: bcrypt salts impiden lookup directo;
   iteramos hashes del gym y `Verify` por intento. Aceptable para volúmenes
   esperados (≤1k miembros con PIN por gym).
2. **Folio format**: `MEM-NNNNNN` (6 dígitos zero-padded por gym). Suficiente
   para 10⁶ socios; los gyms reales no llegan a 10⁴.
3. **List status filter**: derivado al vuelo en SQL (DA-14.2). El cálculo
   `julianday(expiry_date) - julianday(today)` en SQLite es exacto en horas
   de UTC; en Postgres usamos `expiry_date - today`.
4. **`enrollment_paid` y `last_maintenance_paid`** se mantienen en `members`
   como cache (ADR-002 §3.5). Se actualizarán desde billing/UC-018 vía
   `Member.MarkEnrollmentPaid` / `Member.UpdateLastMaintenance`.
5. **No emisión real de eventos `MemberCreatedWithInitialPayment`**: en
   Sesión 2 todavía no existe `billing`. La señal viaja como `PendingFirstPayment`
   en el Output del use case + en la respuesta HTTP. Cuando billing aterrice
   (Sesión 3), `CreateMember` puede invocar `billing.RegisterPayment` directamente
   dentro del mismo `UoW.Command`. Marcado como `TODO(billing — Sesión 3)`.
6. **`UpdateMembershipType` no muta memberships activas**: DA-11.1. El snapshot
   en `memberships` es la fuente de verdad para esa membresía vigente. El
   próximo `Renew()` ya verá los nuevos valores del MembershipType.
7. **PIN auto-generation**: 4 dígitos random rechazando weak PINs (0000, 1234,
   secuencias 1111-9999). 50 intentos máx antes de devolver `errPinExhausted`.
8. **`UC-014` ordenación de "expiry"**: Postgres usa `NULLS LAST`; SQLite los
   pone donde le toca según orden. Aceptable: socios sin membresía activa son
   raros y la UI ya los muestra al final por `status` filter.

## TODO pendientes nuevos

| Marcador | Lugar | Resumen |
|---|---|---|
| `TODO(billing — Sesión 3)` | `members/app/create_member.go` | Si `ChargeFirstPayment=true`, invocar `billing.RegisterPayment` dentro del mismo `UoW.Command`. La firma del cross-BC service `RenewMembershipForPayment` ya está lista. |
| Diferido | UC-018 completo | Vive en billing. Esta sesión deja `RenewMembershipForPayment` ready. |

## Comandos

```bash
# Smoke verificado en sidecar (`bin/cuadra-sidecar`):
SIDECAR_DB_PATH=/tmp/cuadra.db bin/cuadra-sidecar &
curl -s http://127.0.0.1:9090/health
# Signup → token → POST /api/v1/membership-types → POST /api/v1/members → ...
```
