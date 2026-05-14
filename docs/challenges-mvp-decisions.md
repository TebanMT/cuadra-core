# Retos MVP — Decisiones de Sesión 2

Decisiones ambiguas tomadas durante la sesión y el rationale. Para
contexto de qué se entregó, leer `challenges-mvp.md`. Para arquitectura,
`CLAUDE.md` y `CUADRA-SPEC.md`.

## 1. AttendanceCounter como puerto, adapter en `challenges/infraestructure`

La interfaz `AttendanceCounter` vive en `challenges/domain/repository` y
expone un único método `CountInRange(tx, gymID, memberID, fromMs, toMs)`.
El adapter concreto vive en `challenges/infraestructure/checkins_attendance_{server,sidecar}.go`
y le pega al `checkins` table directamente (no a través del
`CheckinRepository` del módulo `checkins`).

**Por qué un adapter directo a SQL en lugar de extender `CheckinRepository`:**
- El módulo `checkins` no tenía un método `CountInRange` y agregarlo lo
  contamina con un caso de uso que sólo retos necesita.
- DDD: la dependencia es del módulo "alto" (retos) hacia el módulo
  "bajo" (checkins). El adapter expresa esa dirección y vive en retos.
- El query es trivial (un COUNT con filtros) y el costo es bajo.

**Filtro de check-ins válidos:** sólo se cuentan los `result LIKE 'allowed%'`.
Un socio que se presentó pero rebotó por membresía vencida no debe sumar a
su asistencia del reto — la asistencia mide compromiso, no intentos.

## 2. T₀ window cubre `open_registration` Y `running`

`AllowsT0Capture` regresa true cuando status ∈ {open_registration, running}
y `now ≤ MeasurementT0Deadline`. Permitir T₀ durante `running` es
intencional: en gimnasios reales el dueño abre inscripciones, empieza el
reto, y un socio que se inscribió tarde alcanza a llegar al primer pesaje.
Forzar que todos midan antes de `running` es un constraint sintético.

Trade-off: durante `running` también pueden capturarse mediciones
"intermediate" — esas ya están permitidas. T₀ con la misma ventana es
consistente.

## 3. Intermediate moments comparten la ventana de T₀

`MomentIntermediate` (la medición de semana 10 observacional, no entra al
scoring) usa la misma ventana que `MomentT0`. Razón: la nutricionista
quiere meter datos de seguimiento mientras el reto está activo
(open_registration → running). No hay valor en bloquearlas más estricto.

## 4. Orden de operaciones en `CaptureMeasurement`: Insert antes de Supersede

Cuando hay una medición previa para el mismo `(participant, moment)`:

1. Validate moment + ventana.
2. `GetActiveByMoment` — busca la activa previa (sin tocarla aún).
3. `Create(newMeasurement)` — genera ID, persiste fila nueva.
4. Si había previa, `Supersede(prior.ID, new.ID, now)`.

**Por qué Insert primero:** el `superseded_by_id` apunta a la fila nueva
— necesitamos su UUID. Si hiciéramos Update primero y Create después,
necesitaríamos dos roundtrips o un UUID generado client-side. El UUID en
realidad se genera en el use case con `uuid.New()` así que podríamos
ordenar al revés, pero hacer Insert primero también nos da una invariante
útil: en cualquier momento intermedio entre las dos statements del tx,
nunca existe un `superseded_by_id` apuntando a una fila inexistente. Es
más fácil razonar sobre.

Todo el flujo corre en una única `UoW.Command` → rollback automático si
cualquier paso falla.

## 5. Cache de ranking: `sync.Map` con TTL de 30s, sin Redis

El prompt sugería evitar Redis. Decisión:
- `sync.Map` keyed por `challenge_id`.
- TTL hardcoded de 30s.
- Sin invalidación explícita en el camino de escritura (capturas) — basta
  con que el ranking siguiente, después de 30s, sea fresco.

**Por qué no invalidar explícitamente al capturar:**
- Captura no es frecuente (decenas por reto, no miles).
- Implementar invalidación duplica lógica (cada uso del repo de mediciones
  tendría que conocer el cache).
- El TTL corto absorbe el costo: 30s es imperceptible para el dueño viendo
  el ranking durante el cierre.

Si en producción el cache TTL se nota mucho, expongo `InvalidateCache` en
el use case (ya implementado) y lo llamo desde `CaptureMeasurement`.

## 6. `update_participant.go` — partial exercise updates

Si el cliente manda `exercise_legs` pero no `exercise_push`/`exercise_pull`,
el use case construye una `ExerciseSelection` con sólo el campo provisto y
strings vacíos en los otros. Eso pasa al método `UpdateExercises` de la
entidad que **reemplaza** los tres.

**Tradeoff:** un cliente que sólo quiere editar legs tiene que mandar los
tres. Justifica:
- La modal del FE para editar ejercicios siempre muestra y manda los tres
  juntos (no hay edit-individual).
- Hacer parcial requeriría un constructor de `ExerciseSelection` que
  cargue desde la fila actual + sobrescriba — más complejidad por una
  ergonomía que el FE no usa.

Documentado en el controller con un comentario.

## 7. Validación de `category_id` en `AddParticipant`

Tres errores distintos, en orden:
1. `ErrChallengeNotRegistering` (status check primero — barato y aborta
   antes de cualquier otra query).
2. `ErrCategoryMismatch` (la categoría pertenece a otro reto).
3. `ErrAlreadyParticipating` (este miembro ya está inscrito).

**Por qué `ErrCategoryMismatch` antes que `ErrAlreadyParticipating`:**
si el cliente manda categoría incorrecta + miembro ya inscrito, el bug
más probable es la categoría (typo de UUID, copy-paste). Sacar ese error
primero ayuda al FE a corregir antes de reintentar.

## 8. Tests de integración: sin sync queue

El integration test usa `sharedDomain.NewSQLiteUnitOfWork(db, nil)` (sin
queue). Razón: los tests verifican el comportamiento del módulo de
retos, no del sync agent. El sync queue tiene sus propios tests
(`projector_test.go`, `agent_test.go`).

## 9. Validación del controller para "stop here" vs "continue"

Cuando `parseUUIDParam` falla devuelve `(uuid.Nil, false)` y ya escribió
el response. Los handlers verifican `ok` y hacen `return`. Por qué este
patrón en lugar de panic / un error builder:
- Match al patrón de `bind` que ya existía en el módulo (`return false`
  + cierre temprano del handler).
- No requiere imports extras ni tipos custom.

## 10. Defaults de ejercicios — sentadilla / press de banca / peso muerto

Constants en `participant.go`:
- `ExerciseLegsSquat = "sentadilla"`
- `ExercisePushBenchPress = "press_banca"`
- `ExercisePullDeadlift = "peso_muerto"`

Defaults aplican cuando el cliente manda strings vacíos en
`AddParticipantInput.Exercises`. Mantengo strings (no enum) para que
agregar variantes (e.g. `front_squat`) sea solo doc — sin migración de
DB. El conjunto canónico vive en código (constants) para que FE y BE
acuerden sin coordinación extra.

## 11. `ranking.go` — `attendance_insufficient` no excluye del ranking

El flag está pensado para mostrarse en UI (insignia roja), no para
filtrar. Razón:
- El participante con asistencia insuficiente pero con buenos números
  necesita ver su posición — es feedback honesto.
- La decisión de descalificar es una acción manual (`check-disqualifications`
  o desde el dashboard). Mezclar "no apareces porque no asististe" con
  "no entras en el podio porque no asististe" confunde al participante.
- El dueño ve la insignia y decide. Transparencia primero.

## Sesión 3 — UI

Decisiones de UX/FE de la sesión 3 (UI en cuadra-desktop +
cuadra-dashboard). El BE quedó intacto: los clientes son shells sobre los
endpoints que cerró sesión 2.

### S3.1 Wire types copiados por proyecto, no por paquete compartido

Cada cliente tiene su propio `src/types/challenges.ts`. Las dos copias
son byte-idénticas hoy. Razones:

- No hay un repo compartido entre cuadra-desktop y cuadra-dashboard
  (ambos importan de `cuadra-core` pero del lado Go, no del TS).
- Generar shapes desde un OpenAPI/proto introduce build steps que no
  pagan su costo para 7 interfaces.
- El BE es el source-of-truth; si el wire cambia, los dos clientes
  rompen al mismo tiempo y los reparamos en bloque (TS los flaggea en
  CI con `pnpm tsc --noEmit`).

Trade-off explícito: drift potencial. Mitigación: el doc lo dice ("son
copias 1-a-1") y la primera sección de la PR review chequea ambos.

### S3.2 IR / deltas SIEMPRE del BE — el FE no recalcula

El cliente nunca aplica la fórmula `IR = ΔG% + 2·ΔM% + min(ΔF%, cap)` ni
mira `tie_margin_ir` para etiquetar empates. Pinta lo que devuelve
`/ranking`. Razones:

- La fórmula es el contrato público del producto (landing). Una
  divergencia FE/BE en un evento real es un desastre.
- El BE cachea el ranking 30s en `sync.Map` — el cliente lo refetchea
  con `staleTime: 30s` + `refetchOnWindowFocus`, alineado con esa TTL.
- 1RM Epley **sí** se muestra en el FE (sólo durante captura, como
  feedback al operador). El BE lo recomputa y el ranking final usa el
  suyo — el del FE es UX, no truth.

### S3.3 Owner-only vs todos-autenticados — el FE muta UI, el BE muta state

Roles: el BE tiene `RequireOwner()` en las rutas que importan (POST/PATCH
de challenge, categorías, participantes, status, DQ). El FE lee
`useAuthStore.user.role` y oculta CTAs cuando `role !== "owner"`. Razones:

- Si un operador intenta llamar un endpoint owner-only, el BE devuelve
  403 y todo queda consistente. La UI sólo le ahorra el viaje.
- No duplicamos lógica de permisos (escalable: un nuevo rol cambia BE
  primero, FE lo respeta al renderear).
- Capturar mediciones queda abierto a operador (la nutri toma datos sin
  pedirle al dueño que se loguee).

### S3.4 La modal de captura muestra el aviso de supersession con la fecha previa

Cuando ya hay una medición activa del mismo `(participant, moment)`, la
modal levanta un banner amarillo con la fecha de la medición previa
**antes** del submit. El BE igual hace el insert + supersede en una
única transacción si el operador guarda. El banner sirve sólo de
prevención psicológica: "estás reescribiendo el dato, ¿en serio?".

Razón: la pelea es UX-de-confianza, no integridad. La integridad la
garantiza el BE (la fila vieja queda con `superseded_at` no nulo,
auditable). El banner protege contra typos accidentales.

### S3.5 "Mediciones" es un tab del desktop pero NO del dashboard

El dashboard tiene 4 tabs (Resumen / Participantes / Ranking / Config),
no 5. Razón:

- El dashboard es para consulta desde el celular del dueño. Capturar una
  medición pide hardware específico (báscula, plicómetro, espacio para
  un set submáximo). Hacerlo desde el celular es una UX falsa.
- La modal de captura pide pantalla amplia + teclado físico. Encajarla
  en un viewport móvil resulta en errores.
- Quien captura es la nutri/operador en recepción, que ya está frente a
  la Tinta Desktop. La división de surfaces refleja la división de
  tareas reales.

### S3.6 Config en el dashboard es read-only — siempre

El dashboard expone el tab Config sólo si rol = owner, pero **siempre
deshabilita los inputs**, incluso en `status=draft`. Razones:

- Un cambio de cuota o fechas es una decisión cara — el dueño la toma
  con tiempo, no desde el celular en el carro.
- Mantenemos un único lugar de verdad para "config edit" (desktop). El
  dashboard linka a la pantalla del desktop por copy ("La configuración
  se edita desde la Tinta Desktop").
- Reduce surface de bugs: la edición desde dashboard tendría que validar
  contra `ErrConfigLocked` y manejar fechas en UTC vs local — work que
  no paga su costo para una superficie de bajo uso.

### S3.7 Bottom nav: bumpeo a `grid-cols-7` en lugar de filtrar

Agregar `/retos` al sidebar deja al bottom nav del dashboard con 7 items
después del filtro de `/download`. Opciones:

- (a) bumpear a `grid-cols-7`,
- (b) filtrar otro item del bottom nav,
- (c) meter el icono "más" con un sheet de overflow.

Elegimos (a). Razón: dejar `Retos` accesible a una sola tap desde el
celular es el flujo principal del dueño durante el reto (mirar el
ranking). El target táctil baja ~14% en width pero sigue siendo
~52px en pantallas iPhone 13+, dentro del mínimo de Apple HIG (44pt).

### S3.8 Avatar del participante: hash del nombre, NO del estado

Igual que el patrón ya establecido para Socios (sesión previa): el color
del avatar es identidad, el estado vive en el Badge. Razones documentadas
en `MembersPage.tsx` y se respetan acá para coherencia visual.

---

## 12. `GetChallengeDetail` retorna conteos T₀/T₁ con N+1 queries

Para cada participante hace dos `GetActiveByMoment`. Es N+1 — un único
COUNT por challenge sería más eficiente. Razón para no optimizar:
- Un reto típico tiene 20-50 participantes. Dos queries por participante
  con índice partial `(participant_id, moment) WHERE deleted_at IS NULL
  AND superseded_at IS NULL` corre sub-milisegundo.
- El endpoint se llama UNA vez al abrir la pantalla de detalle.
- Una query agregada introduce un método nuevo en el repo y duplica la
  lógica de "qué es activa".
- Si en producción el detail es lento, hay un único lugar donde
  reemplazarlo.

YAGNI explícito.
