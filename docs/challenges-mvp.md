# Retos — MVP

> "Reto 12" es una competencia de 12 semanas de recomposición corporal. Tinta
> lo automatiza: inscripciones, mediciones T₀/T₁, ranking en vivo, control
> de asistencia y descalificación automática.

Este doc cubre el módulo `challenges/` end-to-end. Para arquitectura general
de Tinta ver `CLAUDE.md` y `CUADRA-SPEC.md`.

## El Índice de Recomposición (IR)

Cada participante tiene dos mediciones — T₀ al inicio, T₁ al cierre. El IR
combina tres componentes:

```
IR = ΔG% + 2·ΔM% + min(ΔF%, strengthCap)

ΔG% = ((G₀ − G₁) / G₀) × 100      % de masa grasa perdida
ΔM% = ((M₁ − M₀) / M₀) × 100      % de masa magra ganada
ΔF% = ((F₁ − F₀) / F₀) × 100      % de fuerza relativa ganada

G = peso × %grasa / 100           kg de grasa
M = peso − G                      kg de magra
F = (Legs1RM + Push1RM + Pull1RM) / peso     fuerza por kg
1RM ≈ peso × (1 + reps/30)        (Epley, sobre el set submáximo)
```

Detalles importantes:

- ΔM% va con peso **×2** porque ganar músculo es ~2x más difícil que perder
  grasa en el mismo horizonte. Sin ese factor los que sólo bajan grasa
  dominan. Documentado en el landing público — cambiarlo rompe contrato.
- El strength cap se aplica **sólo hacia arriba**: perder fuerza (ΔF%
  negativo) duele al participante. El default es 25%.
- Fuerza se normaliza por peso corporal en cada momento. Subir 5 kg de
  bodyweight sin subir los lifts ≈ fuerza relativa plana, no más fuerte.
- El código vive en `domain/scoring/scoring.go` — función pura, sin
  dependencias, con tests exhaustivos. Es el componente más sensible del
  módulo (un bug aquí destruye un evento real).

## Estados del reto

```
draft ──open──> open_registration ──start──> running ──t1──> measuring_t1 ──close──> closed
  │                    │                        │                 │
  └────cancel──────────┴────────cancel──────────┴──────cancel─────┴─────> cancelled
```

| Estado            | Quién puede |
|-------------------|-------------|
| draft             | Crear categorías, editar config |
| open_registration | Inscribir socios, capturar T₀, editar config |
| running           | Capturar T₀ (sigue la ventana) |
| measuring_t1      | Capturar T₁, cerrar reto |
| closed            | Sólo lectura (ranking final) |
| cancelled         | Sólo lectura, sin ranking oficial |

Reglas que se enforzcan en el use-case (no en la entidad):

- `open_registration` requiere ≥1 categoría.
- Editar config (`PATCH /challenges/:id`) se rechaza con `ErrConfigLocked`
  apenas existe **una** medición — esa es la barrera que protege a los
  inscritos de un cambio de reglas a media competencia.

## Crear un reto end-to-end

Todos los curls asumen `BASE=https://api.entinta.app` y un token de owner.

### 1. Crear el reto

```bash
curl -X POST $BASE/api/v1/challenges \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Reto 12 — Verano 2026",
    "starts_at":              "2026-06-01T00:00:00Z",
    "measurement_t0_deadline":"2026-06-15T00:00:00Z",
    "measurement_t1_start":   "2026-08-17T00:00:00Z",
    "ends_at":                "2026-08-31T00:00:00Z",
    "inscription_fee_cents": 50000
  }'
```

### 2. Agregar categorías

```bash
curl -X POST $BASE/api/v1/challenges/$CID/categories \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{ "name": "Hombres", "sort_order": 1 }'

curl -X POST $BASE/api/v1/challenges/$CID/categories \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{ "name": "Mujeres", "sort_order": 2 }'
```

### 3. Abrir inscripciones

```bash
curl -X POST $BASE/api/v1/challenges/$CID/status \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{ "transition": "open_registration" }'
```

Transiciones válidas: `open_registration`, `start_running`,
`start_measuring_t1`, `close`, `cancel`.

### 4. Inscribir socios

```bash
curl -X POST $BASE/api/v1/challenges/$CID/participants \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "member_id":   "'$MEMBER_ID'",
    "category_id": "'$CAT_ID'"
  }'
```

Si no mandas los ejercicios se usan los defaults: Sentadilla / Press de
Banca / Peso Muerto. Para usar variantes, manda `exercise_legs`,
`exercise_push`, `exercise_pull` con los slugs de `participant.go`
(`prensa`, `press_pecho_maquina`, `jalon_polea`, etc.).

### 5. Capturar mediciones

```bash
curl -X POST $BASE/api/v1/challenges/$CID/participants/$PID/measurements \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "moment":          "t0",
    "measured_at":     "2026-06-05T14:30:00Z",
    "body_weight_kg":  82.4,
    "body_fat_pct":    24.1,
    "legs_weight_kg":  85, "legs_reps": 5,
    "push_weight_kg":  60, "push_reps": 5,
    "pull_weight_kg": 110, "pull_reps": 3,
    "notes": "Plicómetro 7-puntos"
  }'
```

La respuesta incluye:

- `measurement` — la fila recién creada
- `superseded_prior_id` — si esta captura reemplaza una anterior, el id de
  la fila previa (null si era la primera)
- `participant_status` — `active` después de la primera T₀

Mira la sección de **Supersession** abajo para cómo manejamos correcciones.

### 6. Ranking

```bash
curl $BASE/api/v1/challenges/$CID/ranking?category_id=$CAT_ID \
  -H "Authorization: Bearer $TOKEN"
```

Respuesta:

```json
{
  "items": [
    {
      "participant_id": "...",
      "category_id": "...",
      "member_id": "...",
      "ir": 12.34,
      "delta_fat_pct": 8.2,
      "delta_muscle_pct": 1.8,
      "delta_strength_pct": 0.45,
      "position": 1,
      "tied": false,
      "attendance_insufficient": false
    }
  ]
}
```

Notas:

- El ranking se calcula al vuelo desde las mediciones activas (no se
  almacena). Cache en memoria por challenge con TTL de 30s — basta para una
  pantalla que refresca cada pocos segundos sin pegarle a la DB.
- Participantes sin T₀ + T₁ activas no aparecen.
- Filtrar por `category_id` es opcional; sin filtro devuelve todas las
  categorías mezcladas.

## Ranking y empate técnico

Dentro de cada categoría se ordena por IR descendente. Después se marca
`tied: true` cuando dos posiciones consecutivas están dentro del
`tie_margin_ir` configurado en el reto (default 5.0 puntos IR).

Si hay empate técnico el rule-set permite usar asistencia como criterio
de desempate (no automatizado — el dueño lo resuelve viendo el reporte de
asistencia). El marcador `attendance_insufficient` ayuda a esa decisión:
es `true` cuando el participante quedó por debajo del mínimo semanal
acumulado a la fecha. **No se filtra del ranking** — sigue mostrándose en
su posición; el FE pinta una insignia y el dueño decide qué hacer.

## Capturar mediciones — supersession

Las mediciones son **inmutables** en semántica: cuando hay una corrección
NO se actualiza la fila — se inserta una nueva, y la anterior se marca
como superseded.

Flujo de captura cuando ya existe una medición activa del mismo
`(participant, moment)`:

```
                  ┌─────────────────┐
                  │  Capture T₀ #2  │  (operador re-captura por typo)
                  └────────┬────────┘
                           ▼
              ┌────────────────────────┐
              │  INSERT new measurement│ (gets new UUID)
              └────────┬───────────────┘
                       ▼
              ┌──────────────────────────────────────────┐
              │  UPDATE prior SET superseded_at=now(),   │
              │                  superseded_by_id=new.id │
              └──────────────────────────────────────────┘
```

Garantías:

- Ambas escrituras corren en la misma transacción (`UoW.Command`).
- El ranking consulta `WHERE superseded_at IS NULL` → siempre ve la última
  activa.
- `ListByParticipant` devuelve TODAS las filas — la modal de auditoría
  muestra la historia completa.
- En el sidecar, ambas filas se enquolan al `sync_queue` (la fila previa
  cambia su versión y necesita propagarse al cloud).

Por qué Insert antes de Supersede:

- Necesitamos el UUID de la fila nueva para apuntar el `superseded_by_id`
  → el flujo "a dónde fue esta fila" siempre apunta hacia adelante en el
  tiempo, nunca null.

## Descalificación por asistencia

El reto configura dos parámetros:

- `min_weekly_attendance` — check-ins requeridos por semana (default 3)
- `attendance_grace_weeks` — semanas que se pueden "fallar" sin DQ
  (default 2)

El módulo expone dos endpoints relacionados:

### Reporte semana-a-semana

```
GET /api/v1/challenges/:id/attendance-status
```

Devuelve, por cada participante: array de semanas con conteo y flag `met`,
totales `weeks_met` / `weeks_missed`, y `within_grace` true cuando aún no
excede las semanas de gracia.

### Aplicar descalificaciones

```
POST /api/v1/challenges/:id/check-disqualifications
```

Job idempotente: itera sobre participantes en estado `active`, calcula
semanas fallidas, y si `missed > attendance_grace_weeks` marca el
participante como `disqualified` con razón "asistencia insuficiente".

Diseño:

- El job es **idempotente** — re-correrlo no produce DQs duplicados (la
  entidad rechaza el cambio de estado si ya está `disqualified`).
- En producción se llama dos veces:
  1. Como cron semanal — captura los DQs naturales conforme avanza el reto.
  2. Manualmente desde el dashboard antes del cierre — el dueño puede
     verlo después del reporte de asistencia y decidir si jala el gatillo.
- El ranking no auto-filtra DQ — los marca con `attendance_insufficient`
  pero los deja visibles. La razón es transparencia: el participante ve
  que su IR fue alto pero la insignia roja explica por qué no compite.

## Endpoints completos

| Método | Path | Rol |
|--------|------|-----|
| GET    | `/api/v1/challenges` | any |
| POST   | `/api/v1/challenges` | owner |
| GET    | `/api/v1/challenges/:id` | any |
| PATCH  | `/api/v1/challenges/:id` | owner |
| POST   | `/api/v1/challenges/:id/status` | owner |
| GET    | `/api/v1/challenges/:id/categories` | any |
| POST   | `/api/v1/challenges/:id/categories` | owner |
| PATCH  | `/api/v1/challenges/:id/categories/:catId` | owner |
| DELETE | `/api/v1/challenges/:id/categories/:catId` | owner |
| GET    | `/api/v1/challenges/:id/participants` | any |
| POST   | `/api/v1/challenges/:id/participants` | owner |
| PATCH  | `/api/v1/challenges/:id/participants/:pid` | owner |
| DELETE | `/api/v1/challenges/:id/participants/:pid` | owner |
| GET    | `/api/v1/challenges/:id/participants/:pid/measurements` | any |
| POST   | `/api/v1/challenges/:id/participants/:pid/measurements` | any (operador) |
| GET    | `/api/v1/challenges/:id/ranking` | any |
| GET    | `/api/v1/challenges/:id/attendance-status` | any |
| POST   | `/api/v1/challenges/:id/check-disqualifications` | owner |

Capturar mediciones es de cualquier usuario autenticado porque la
nutricionista del gym (rol `operator`) es quien sostiene la libreta. El
owner mantiene control de las decisiones estructurales (config, status,
categorías, descalificaciones).

## Offline-first

Todo este módulo corre tanto en cloud (`-tags server`) como en el sidecar
local (`-tags sidecar`). El sidecar usa SQLite + `sync_queue` que el agent
empuja al cloud cuando hay internet. Para el flujo de captura — que pasa
junto a la cama de la pesa, con la nutri tomando notas en una laptop —
esto significa que la operación nunca depende de Hetzner estar arriba.

## UI — desktop (captura) y dashboard (consulta)

La UI vive en dos clientes con superficies distintas. El BE (este módulo)
es el único source-of-truth; los clientes son shells que renderizan lo que
el BE devuelve. Importante: el IR, los empates técnicos y los flags de
asistencia NO se recomputan en el cliente — el BE los emite en
`/ranking` y la UI los pinta tal cual.

### Wire types

Los TS interfaces que consumen los clientes viven en:

- `cuadra-desktop/src/types/challenges.ts`
- `cuadra-dashboard/src/types/challenges.ts`

Son copias 1-a-1 (mismas interfaces y constantes de ejercicios). Si
agregás un campo al `challengeResp` o `rankingEntryResp`, actualizá
**ambos** archivos. No hay capa de transformación entre wire y UI.

### Desktop — `/retos` (Tinta Desktop, recepción)

Cinco pantallas atadas a un único árbol de rutas:

- `/retos` — lista de retos con badge de estado, fechas T₀/T₁, conteo de
  participantes activos/totales, CTA primario "Nuevo reto". Empty state
  editorial: "Todavía no hay retos. Crea el primero para empezar."
- `/retos/:id` — detalle con cinco tabs:
  - **Resumen** (default): 4 StatCards — participantes activos, T₀
    pendientes, días para T₁, T₁ capturadas. Botón primario contextual
    al status (`Abrir inscripciones` / `Iniciar reto` / `Iniciar
    mediciones T₁` / `Cerrar reto`).
  - **Participantes**: tabla con foto/avatar, nombre, categoría, ejercicios,
    fee, estado. Filtros por categoría y status. CTA "Capturar medición"
    inline y botón "Agregar participante" (owner-only).
  - **Mediciones**: vista enfocada en captura. Lista pendientes del momento
    activo según status (`t0` cuando draft/open_registration/running,
    `t1` cuando measuring_t1). Cada fila tiene CTA primario "Capturar".
  - **Ranking**: tabla por categoría ordenada por IR desc. Columnas
    Posición / Socio / ΔG% / ΔM% / ΔF% / IR / Estado. Empates técnicos
    con badge `warning`; asistencia insuficiente con badge `destructive`.
    Banner amarillo "Ranking provisional" cuando `status != closed`.
  - **Configuración** (owner-only): form con nombre, fechas, cuota,
    asistencia mínima y semanas de gracia. Inputs `disabled` cuando
    `status != draft` (el BE rechaza con `ErrConfigLocked` apenas hay
    una medición; el FE refleja eso bloqueando los inputs antes). Gestor
    inline de categorías. Botón "Revisar descalificaciones" que dispara
    el job idempotente.

### Modales

- **Capturar medición** (`CaptureMeasurementModal.tsx`): dos secciones —
  composición corporal (peso, %BF, derivados de masa magra/grasa
  mostrados como labels read-only) y fuerza (peso + reps por patrón,
  ejercicio del participante en read-only, 1RM Epley calculado en el FE
  sólo para feedback). Banner amarillo cuando ya existe una medición
  activa del mismo `moment`: "Si guardas, la anterior será reemplazada
  pero no eliminada (auditable)". Validaciones cliente: %BF entre 3 y
  60, reps entre 1 y 15, warning si BF queda por debajo del piso del
  reto (el BE valida también).
- **Agregar participante** (`AddParticipantModal.tsx`): autocomplete
  contra `/api/v1/members/search`, selector de categoría poblado del
  reto, 3 selectores de ejercicios con defaults
  (sentadilla / press_banca / peso_muerto), toggle "Inscripción pagada"
  que dispara un `PATCH /participants/:pid` con `mark_fee_paid: true`
  encadenado al `POST`.

### Dashboard — `/retos` (consulta desde celular)

Mismo árbol pero **read-only y mobile-first**:

- `/retos` — lista compacta. Cliqueable a `/retos/:id`.
- `/retos/:id` — cuatro tabs (sin Mediciones):
  - Resumen (idéntico).
  - Participantes (sin CTA "Capturar"; sin filtros owner-only).
  - Ranking (idéntico).
  - Configuración (visible sólo si rol = owner; **siempre read-only** —
    edición vive en el desktop). Banner: "La configuración se edita
    desde la Tinta Desktop."

### Hooks (compartidos por shape, copiados por proyecto)

`useChallenges.ts` expone, en cada cliente:

```
useChallenges({ status?, page?, pageSize? })
useChallenge(id)
useChallengeCategories(id)
useChallengeParticipants(id, { status?, categoryId? })
useParticipantMeasurements(challengeId, participantId)
useChallengeRanking(id, categoryId?)     // staleTime 30s + refetchOnFocus
useChallengeAttendance(id)

useCreateChallenge() / useUpdateChallenge(id) / useTransitionChallenge(id)
useAddCategory(id) / useUpdateCategory(id, catId) / useDeleteCategory(id)
useAddParticipant(id) / useUpdateParticipant(id, pid) / useRemoveParticipant(id)
useCaptureMeasurement(id, pid)
useCheckDisqualifications(id)
```

Todas las mutations invalidan `["challenges", challengeId]` — eso barre
detail, categories, participants y ranking en una sola línea. El cliente
no autoriza nada: roles se respetan en BE (`RequireOwner` middleware),
el FE sólo muestra/oculta CTAs por `useAuthStore.user.role`.

### Sidebar nav

Ambos clientes ganan un nav item `/retos` con icono `Trophy`. El bottom
nav del dashboard pasó de `grid-cols-6` a `grid-cols-7` para acomodarlo.
