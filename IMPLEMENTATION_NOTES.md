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

---

# cuadra-core — Notas de Implementación, Sesión 5 (Checkins + Biometric)

Fecha: 2026-04-25

## Qué se implementó

**Sesión 5 (UC-028 a UC-032 + DA-29.2) end-to-end:**

| UC | Endpoint | Use case |
|---|---|---|
| UC-028 | `POST /api/v1/members/:id/fingerprint` | `members/app.RegisterFingerprint` |
| UC-029 | `POST /api/v1/checkins/fingerprint` (sidecar-only) | `checkins/app.CheckinByFingerprint` |
| UC-029 (auto-detect) | `GET  /api/v1/biometric/status` | `KioskController.handleStatus` |
| UC-030 | `POST /api/v1/checkins/manual` | `checkins/app.CheckinManual` |
| UC-031 | `POST /api/v1/kiosk/start`, `POST /api/v1/kiosk/stop`, `GET /api/v1/kiosk/events` (long-poll) | `checkins/app.KioskLoop` + `KioskBroadcaster` |
| UC-032 | `POST /api/v1/checkins/pin` | `checkins/app.CheckinByPin` |
| DA-29.2 | `POST /api/v1/checkins/override` | `checkins/app.OverrideCheckin` |

**`shared/biometric/` expandido:**
- `Reader` interface re-escrita conforme a ADR-004 §3.3:
  `Info()`, `OnConnect/OnDisconnect`, `Capture`, `Enroll(samples)`,
  `Identify(input, []EncryptedTemplate, threshold)`, `Available`.
- Tipos value: `CaptureResult{Bytes, Format, QualityScore}`, `EncryptedTemplate{MemberID, Bytes, Format}`, `MatchResult{MemberID, Score}`, `ReaderInfo{DeviceID, Vendor, Model, Connected}`.
- Mock (`mock.go`, `!sidecar || bio_mock`) ahora usa el `GMKProvider` para descifrar y compara plaintext byte-a-byte (test-friendly determinismo).
- Stub real DP (`digitalpersona.go`, `sidecar && bio_dp`) — header con declaraciones CGO comentadas + métodos vacíos. TODO ADR-004 Phase 1.
- Disabled stub (`digitalpersona_disabled.go`, `sidecar && !bio_dp`) — exporta `NewDigitalPersonaReader` para builds sidecar dev (kiosko opera en PIN/manual hasta que el SDK aterrice).

**`shared/biometric/crypto/`:**
- AES-256-GCM con formato `[version=0x01][nonce 12B][ciphertext+tag]` (ADR-006 §2.1).
- `EncryptTemplate` / `DecryptTemplate` + `Zero` defensivo para plaintexts.
- `GMKProvider` interface + `InMemoryGMKProvider` (mock determinístico para tests, fallback dev en cloud + sidecar). El backend real OS-keychain (Tauri command) está marcado como TODO Sesión 8.
- Tests: round-trip, tamper detection (ErrDecryptionFailed), wrong key, version unsupported, blob too short, nonce uniqueness, GMK provider lifecycle.

**`members` BC adiciones:**
- `domain/fingerprint/MemberFingerprint` aggregate (UC-028) — guarda **solo** bytes ya cifrados; el dominio no maneja plaintext nunca. Validators: identifiers no-Nil, blob no-empty, quality ≥ `QualityScoreFloor` (60). `SoftDelete` bumpea version.
- `domain/fingerprint/errors.go` — sentinels: `ErrEmptyTemplate`, `ErrFingerprintAlreadySet`, `ErrConsentRequired`, etc.
- `domain/repository/repository.go` — `FingerprintRepository` (Create/Update/GetByMember/ListByGym) + `MemberPinCandidateLister` interface (capability opcional para UC-032 sin engordar `MemberRepository`).
- `infraestructure/db/models/member_fingerprint_model.go` (Postgres GORM) + `db/repositories/fingerprint_postgres.go` (mapper bidireccional).
- `infraestructure/db/repositories/fingerprint_sqlite.go` (sqlx) — payload sync queue codifica `template_encrypted` como base64 (la columna del queue es TEXT).
- `app/register_fingerprint.go` (UC-028) — encripta fuera del UoW.Command, valida consentimiento, rechaza duplicados (DA-28.2), audita con tamaño-de-blob (jamás bytes).
- `MemberService.WithFingerprints(repo)` + `LoadFingerprintsForGym(ctx, tx, in)` — seam cross-BC para que checkins lea las huellas.
- `MemberRepository.ListPinCandidates` (Postgres + SQLite) — devuelve `[]PinCandidate{MemberID, PinHash}` para que checkins pueda hacer la iteración bcrypt sin conocer el repo concreto.

**`checkins` BC (nuevo):**
- `domain/checkin/Checkin` aggregate con cuatro factories — `NewFingerprintCheckin`, `NewPinCheckin`, `NewManualCheckin`, `NewOverrideCheckin`. Mapea `access.AccessStatus` → `chk_checkins_result` enum.
- `domain/errors/errors.go` — sentinels para UC-029/030/032/DA-29.2.
- `domain/repository/CheckinRepository` — `Create`, `GetByID`, `ListByMember(memberID, since, limit)`.
- `infraestructure/db/models/checkin_model.go` + `repositories/checkin_postgres.go` y `checkin_sqlite.go` (paridad cents/dates, sync_queue write-through).
- `app/services.go` — helper compartido `recordCheckin()` que evalúa AccessStatus + persiste + audita. Los UCs (manual / pin / fingerprint) lo consumen.
- `app/checkin_manual.go` (UC-030).
- `app/checkin_by_pin.go` (UC-032) + `PinAttemptLimiter` in-memory (5 intentos/60s ⇒ cooldown 60s, por gym, reset al éxito).
- `app/checkin_by_fingerprint.go` (UC-029, **build tag `sidecar`**) — fase 1: Query tx para cargar candidates; fase 2: `Reader.Identify` (sin tx); fase 3: Command tx para insertar checkin.
- `app/override_checkin.go` (DA-29.2) — inserta una segunda fila con `result=allowed_override`, conserva el método original, no muta el row negado anterior.
- `app/kiosk_events.go` — `KioskBroadcaster` (subscribe/publish, drop-on-full) + tipos `KioskEvent`/`KioskEventType`. Sin build tag — accesible al cloud para tests.
- `app/checkin_kiosk_loop.go` (**build tag `sidecar`**, UC-031) — goroutine que loopea `Reader.Capture`, dispara fingerprint check-in async, registra callbacks `OnConnect/OnDisconnect`, publica eventos. `Start` idempotente, `Stop` seguro multi-call.
- `interfaces/controllers/checkin_controller.go` — manual/pin/override (cloud + sidecar).
- `interfaces/controllers/kiosk_controller.go` (**build tag `sidecar`**) — fingerprint, biometric status, kiosk start/stop, kiosk events long-poll (timeout 25s).
- `members/interfaces/controllers/fingerprint_controller.go` — `POST /api/v1/members/:id/fingerprint`. Carga útil base64 (la captura del SDK).

**DI:**
- `cmd/server/main.go`: añadidos `fingerprintRepo`, `checkinRepo`, `gmkProvider` (in-memory), `registerFingerprint`, los tres UCs de checkin (manual/pin/override) y los controllers (`fingerprintCtrl`, `checkinCtrl`).
- `cmd/sidecar/main.go`: lo anterior + `bioReader := biometric.NewDigitalPersonaReader()` (variant según build tag), `checkinFingerprint`, `kioskEvents`, `kioskLoop` (con `uuid.Nil` provisional — TODO Sesión 6: bind GymID al login), `kioskCtrl`.

**Tests (todos verdes en sus respectivos build tags):**
- Unit:
  - `shared/biometric/crypto/crypto_test.go` — round-trip + tamper + wrong key + version + blob length + nonce uniqueness + GMK provider lifecycle.
  - `members/domain/fingerprint/fingerprint_test.go` — happy path, validators, soft-delete versioning.
  - `checkins/domain/checkin/checkin_test.go` — todos los factories + mapeo enum + override valida razón ≥5 + `IsAllowed`.
- Integration (`checkins/app/checkins_integration_test.go`, build `sidecar bio_mock`, SQLite real + MockReader):
  - `TestUC028AndUC029_FingerprintEnrollmentAndCheckin` — registra huella, prueba que UC-029 identifica al socio correcto y persiste el checkin con `result=allowed_active`.
  - `TestUC029_NoMatch_ReturnsBusinessError` — captura distinta a la registrada → ErrNoFingerprintMatch.
  - `TestUC030_ManualCheckin_Allowed` — UC-030 con operador, valida que `operator_id` se persiste.
  - `TestUC032_PIN_MatchesAndRejects` — assign PIN → wrong PIN falla → right PIN éxito → 5 wrong intentos → lockout.
  - `TestDA29_2_OverrideAfterDenied` — socio inactive → manual checkin returns denied_inactive → override agrega segunda fila con `manual_override=true`. Ambas filas siguen presentes.
  - `TestKioskLoop_BroadcastsHotplugEvents` — start emite `kiosk_started`, simulación disconnect/connect emite los eventos correspondientes vía broadcaster.

## Build tag matrix

```
go build                                    → cloud (server tag implícito por main.go)
go build -tags=server                       → cloud explícito
go build -tags=sidecar                      → sidecar dev (DigitalPersona disabled stub)
go build -tags="sidecar bio_mock"           → sidecar tests (mock reader)
go build -tags="sidecar bio_dp"             → sidecar producción (DP SDK stub — TODO real link)

go test ./...                               → unit tests + crypto
go test -tags=sidecar ./...                 → + sqlite integration tests
go test -tags="sidecar bio_mock" ./...      → + UC-029 e2e con mock reader
```

## Decisiones tomadas (cuando había ambigüedad)

1. **`MemberPinCandidateLister` como capability opcional** en lugar de
   ampliar `MemberRepository` con un nuevo método obligatorio. Tipo
   `PinCandidate` definido en `members/domain/repository/` para que
   tanto los repos infra como el use case de checkins lo importen sin
   crear un ciclo infra→app.
2. **Encriptación FUERA del UoW.Command en RegisterFingerprint.**
   Failures de crypto (GMK no encontrada, IV exhausted) no deben
   rollback otras escrituras. Sólo entramos en transacción cuando ya
   tenemos el blob listo.
3. **El cloud server expone también las rutas de checkins manual/pin/override**
   aunque la operación en kiosko vive en el sidecar. El dashboard puede
   necesitar registrar checkins manualmente (revisión, corrección post-hoc)
   y los tests cloud se benefician. Fingerprint + kiosk endpoints sí son
   sidecar-only (`build sidecar`).
4. **`KioskBroadcaster` sin build tag** (ver `app/kiosk_events.go`)
   permite que controllers cloud serialicen eventos para dashboards
   futuros sin arrastrar el SDK biometrico.
5. **`KioskLoop` arranca con `uuid.Nil` GymID** — el sidecar es
   single-tenant per process, pero el login todavía no le pasa el
   `gym_id` al loop. Marcado como TODO Sesión 6.
6. **Mock match = byte-equality plaintext** (`MockReader.Identify`).
   El reader descifra los blobs con el GMK provider configurado y
   compara byte a byte. Esto valida la cadena completa (GMK → encrypt →
   sync → decrypt → match) sin simular un matcher fuzzy del SDK.
7. **`PinAttemptLimiter` in-memory** (no persistido). Reinicio del
   sidecar resetea el contador — aceptable porque el espacio es 10⁴
   y cada intento requiere acceso físico al kiosko.
8. **`gmk_keys` cloud aún no se persiste.** Sesión 1 dejó la tabla;
   Sesión 5 usa el `InMemoryGMKProvider` con seed determinístico para
   tests. La generación al signup + delivery al login + cache local
   queda como TODO Sesión 8 (cuando aterricen sync agent + Tauri command
   para keychain).
9. **No se modifican migrations.** Las tablas `member_fingerprints` y
   `checkins` ya las creó Sesión 1 (ADR-002 §3.8 + §3.12).

## TODOs nuevos

| Marcador | Lugar | Resumen |
|---|---|---|
| `TODO(humano)` | `shared/biometric/digitalpersona.go` | Linkear el SDK real DigitalPersona U.are.U 4500 (descomentar `cgo CFLAGS/LDFLAGS`, llamar `dpfpdd_capture`, `dpfpdd_create_ftrs`, `dpfpdd_identify`). Bloqueado en hardware en mano. |
| Sesión 6 | `cmd/sidecar/main.go` | El `KioskLoop` arranca con `uuid.Nil` — bind real `gym_id` después del login. |
| Sesión 8 | `cmd/sidecar/main.go` | Reemplazar `InMemoryGMKProvider` por OS-keychain provider (Tauri command `cuadra.gmk.<gym_id>`). Generación cloud, delivery al login, cache local. |
| Sesión 8 | `members/app/register_fingerprint.go` | Cuando el provider real exista, agregar fallback ARCO (member request to delete). El soft-delete está en el aggregate (`SoftDelete`) — sólo falta el use case + endpoint DELETE. |
| Sesión 9+ | `gyms.kiosk_settings` | Threshold de match (DA-29.4) actualmente hardcoded a 0.7 en `FingerprintMatchThresholdDefault`. Cuando aterrice settings UI, leer de la columna JSON. |

## Comandos de smoke

```bash
# Crypto round-trip
go test ./src/shared/biometric/crypto/...

# Checkin domain
go test ./src/modules/checkins/domain/...

# UC-028..UC-032 e2e con SQLite + MockReader
go test -tags="sidecar bio_mock" ./src/modules/checkins/...

# Build sidecar production-ish
go build -tags="sidecar bio_dp" -o bin/cuadra-sidecar ./cmd/sidecar

# Sidecar dev: arranca y verifica /biometric/status
SIDECAR_DB_PATH=/tmp/cuadra.db bin/cuadra-sidecar &
TOKEN=$(...login...)
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9090/api/v1/biometric/status
# → {"vendor":"HID/Crossmatch","model":"U.are.U 4500 (disabled at build)","connected":false,"available":false}
```

## Próxima sesión sugerida

Sesión 6 (UC-033..UC-036, dashboard read models + reports cross-context):
queries que usan `application/reports/` para retención, ingresos,
asistencia. Esta sesión expone `checkins.ListByMember` ya — reports
podrá agregarlo por gym/rango sin tocar el BC.

---

# cuadra-core — Notas de Implementación, Sesión 8 (Sync protocol)

Fecha: 2026-04-25

## Qué se implementó

**Sesión 8 (UC-042 a UC-045) end-to-end:**

| UC | Endpoint | Componente |
|---|---|---|
| UC-042 | `POST /api/v1/sync/push` (cloud) + Agent loop (sidecar) | `shared/sync.Handler` + `Agent` |
| UC-042 | `GET /api/v1/sync/pull?since=…&limit=…` (cloud) | `shared/sync.Handler.Pull` |
| UC-043 | `GET /api/v1/sync/full?cursor=…&limit=…` (cloud) | `shared/sync.Handler.FullSync` |
| UC-044 | `GET /api/v1/sync/status` (sidecar local) | `shared/sync.StatusController` |
| UC-044 | `POST /api/v1/sync/trigger` (sidecar local, manual nudge) | `StatusController.Trigger` |
| UC-044 | `POST /api/v1/sync/auth` (sidecar, JWT relay desde frontend) | `StatusController.SetAuth` |
| UC-045 | LWW silencioso + `conflict_log` (cloud) + `conflict_log_local` (sidecar) | `PostgresStore.UpsertOne`, `recordLocalConflict` |
| ADR §5 | `GET /_internal/metrics` (Prometheus text) | `shared/sync.Metrics` |

**Plumbing nuevo:**

- `shared/sync/types.go` — wire types (`PushRequest/Response`, `PullChange`, `StatusResponse`, etc.).
  Sin build tags — los comparten cloud y sidecar.
- `shared/sync/tables.go` — registry de las 18 entidades sync'd con sus columnas, en orden topológico (gyms → users → membership_types → … → audit_log). Usado por SQLite apply (sidecar) y full-sync (server).
- `shared/sync/server_store.go` (`//go:build server`) — `Store` interface, `PostgresStore` (UPSERTs en `sync_entities` con `SELECT FOR UPDATE`), cursor opaco resumible, `extractUpdatedAt` (acepta epoch ms o ISO).
- `shared/sync/server_handler.go` — handler Gin, valida `gym_id` payload vs JWT, `schema_version` (responde 426 si excede), procesa cada item en su propia transacción serializable.
- `shared/sync/server_metrics.go` — Prometheus text format hand-rolled (sin dep nueva), counters + histogram con buckets sub-100ms..10s.
- `shared/sync/agent.go` (`//go:build sidecar`) — goroutine con `Run(ctx)`, ticker 30s + canal de `TriggerNow`, `Push`/`Pull`/`FullSync`, backoff exponencial 1s..5min (ADR §3.3).
- `shared/sync/agent_apply.go` — `ApplyPullChange`: UPSERT genérico desde el JSON snapshot a la tabla local, con LWW por `version`, `WHERE excluded.version > <table>.version`. Decoder base64 para BLOBs (`template_encrypted`, `whatsapp_business_token_enc`), coerciones JSON→SQLite (`bool→0/1`, JSONB→TEXT, números enteros).
- `shared/sync/agent_state.go` — wrappers para `sync_state` (client_id, last_pulled_at, last_synced_at, full_sync_cursor, retry_count, next_retry_at).
- `shared/sync/agent_status.go` — `StatusController` que mapea `AgentSnapshot` a los 5 estados de UC-044 (`online | offline_short | offline_medium | offline_long | offline_critical | initial_syncing`).

**Migraciones:**

- `db_migrations/postgres/003_sync_entities.sql` — tabla canónica donde aterrizan los snapshots pusheados desde sidecars. PK `(gym_id, entity_type, entity_id)`, índice `(gym_id, server_updated_at)`. Cloud-only.
- `db_migrations/sqlite/003_sync_local.sql` — `conflict_log_local` (mirror del cloud para soporte/debug; ADR-001 §3.7).

**Atomicidad sync_queue + mutación verificada (ADR §3.10):**

`shared/sync/agent_test.go::TestAtomicidad_SyncQueueAndMutationCommitTogether` y `TestAtomicidad_BothSidesCommit` — fuerzan rollback dentro del `UoW.Command` y verifican que ni members ni sync_queue persistieron. La rama positiva confirma que ambas filas commitean juntas. La existing infra de Sesión 1 (`UoW.Command + SqlxTransaction.EnqueueSync`) ya lo garantizaba; el test es la verificación viva.

**Coalescing en sync_queue (ADR §3.2):**

`SqliteQueue.Enqueue` ya hacía `UPDATE … WHERE entity_id=? AND synced_at IS NULL` antes de `INSERT`. Test `TestSyncQueueCoalescing` confirma que 5 mutaciones consecutivas a la misma entidad colapsan a 1 sola fila pendiente, con `client_version` del último.

## Tests

- **Unit** (no build tag): registry sanity (`tables_test.go`).
- **Server** (`-tags server`): cursor round-trip, `extractUpdatedAt`, push (accept/idempotent/conflict_server_wins/conflict_client_wins/rejected_unauthorized/schema 426), pull, full-sync paginación, `/_internal/metrics`. Mock `Store` + mock `ConflictLogger` evitan necesitar Postgres.
- **Sidecar** (`-tags sidecar`): agent end-to-end contra `httptest`, atomicidad (forced rollback), coalescing, `buildStatusResponse` thresholds (UC-044), `backoff` sequence.
- **Chaos** (`-tags "sidecar chaos"`, **excluido de CI por convención**): sleep 200ms ante cada request. Verificado localmente.

```bash
make test                                   # server + sidecar
go test -tags "sidecar chaos" ./src/shared/sync/  # chaos opcional
```

## Decisiones de diseño (cuando había ambigüedad)

1. **`sync_entities` como tabla canónica cloud, NO populación directa de tablas de dominio en cloud-side desde sync.** El push del sidecar aterriza en `sync_entities` (JSONB blob). Las tablas de dominio cloud (members, payments, …) se populan por handlers cloud-side directos (signup, login, …). Pros: protocolo genérico, cero impedance mismatch entre Postgres y SQLite tipos. Cons: el dashboard cloud (Sesión 6) lee de tablas de dominio — la data pusheada por el sidecar todavía no se ve ahí. Bridging (proyección cloud-side de `sync_entities` → tablas de dominio) es un follow-up.
2. **Wire format = SQLite shape**: timestamps en epoch ms (no ISO), BLOBs base64, JSONB embebido como object/array. Mantiene compat con los `enqueue*` ya escritos en cada BC sin tocar 18 funciones.
3. **JWT relay vía `POST /sync/auth`** (no compartir el JWT secret cloud↔sidecar). El frontend desktop envía el token al sidecar después de UC-002 login. Limpio y desacoplado.
4. **Per-item transacción serializable** (no batch transaction): un item malo no rolea el batch. ADR §3.3 ambiguo, opto por el camino seguro.
5. **`ConflictLogger` como interface** (no struct concreto): desacopla del Postgres real para que los unit tests del handler corran sin DB.
6. **`Bootstrap` público en Agent**: tests precargan `sync_state` y luego llaman `Bootstrap` para que el agent re-lea estado. En producción `Run` lo invoca al startup.
7. **Métricas Prom hand-rolled** (sin `prometheus/client_golang`): set chico y estable, principio "boring", swap si crece.

## TODO pendientes

| Marcador | Lugar | Resumen |
|---|---|---|
| Diferido | `sync_entities` → tablas de dominio (proyección cloud-side) | Para que el dashboard cloud (Sesión 6) vea data pusheada por el sidecar. ETA: tras Sesión 8 está mergeada y el equipo decida si se trata server-side worker o trigger Postgres. |
| Diferido | Snapshots completos en `enqueue*` por BC | Las funciones `enqueueMember`, etc., emiten payloads parciales. ApplyPullChange merge-friendly hoy, pero un snapshot full es más correcto. Tickets per-BC. |
| Diferido | Auto-update de migraciones sidecar (ADR-005) | Cuando llegue `migration version mismatch`, sidecar caer en read-only. Hoy `Pull` con `426` lo deja en backoff infinito — UC-044 mostrará `offline_critical`. Aceptable hasta ADR-005. |
| Diferido | `sync_queue_depth` gauge real | El campo existe en `Metrics` pero el server no escribe (no tiene visibilidad del sidecar). Idea: cliente lo manda en cada push como header `X-Sync-Queue-Depth`. |
| `TODO(humano)` | Chaos suite ampliada (clock skew, packet loss) | Sólo latency está implementada. ADR-001 §6.3 sugiere también drop, clock skew. Implementar gradualmente; CI-skip por defecto. |

## Comandos

```bash
# Build (ambos binarios)
make build

# Run cloud server (con sync handlers)
DATABASE_URL=... make run-server   # serves /api/v1/sync/{push,pull,full} + /_internal/metrics

# Run sidecar con sync agent
CUADRA_CLOUD_URL=https://cloud.cuadra.app SYNC_INTERVAL_S=30 make run-sidecar

# El frontend desktop, post-login (UC-002), envía el token:
curl -X POST http://127.0.0.1:9090/api/v1/sync/auth \
  -H "Content-Type: application/json" \
  -d '{"token":"<access_jwt_del_login>"}'

# Indicador para el frontend (UC-044):
curl http://127.0.0.1:9090/api/v1/sync/status
# {"state":"online","last_synced_at":"…","queue_pending_count":0,…}

# Métricas Prom para scraping
curl http://localhost:8080/_internal/metrics
```

## Próxima sesión sugerida

Cierre / hardening:
- Bridging `sync_entities` → tablas de dominio cloud (para que dashboard vea sidecar pushes).
- Backfill snapshots completos en cada BC enqueue (~18 funciones, tedioso pero mecánico).
- E2E real cliente↔servidor sobre Postgres (hoy hay mock Store + httptest; falta corrida con `make migrate-postgres` en CI).
- ADR-005 (auto-update) — habilita que `426` actualice el sidecar en lugar de quedar en `offline_critical` indefinido.
