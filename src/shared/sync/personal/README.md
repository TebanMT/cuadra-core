# Sincronización personal: bootstrap PostgreSQL

Este subárbol implementa el protocolo personal separado del sync de gimnasios.
La referencia funcional es `docs/mancuerna/etapa-1/s00/SYNC.md` en el workspace.
El bootstrap materializa un corte inmutable para iniciar `pull` después de su
secuencia H; no conserva una transacción abierta entre peticiones HTTP.

## Entrada y autorización

`CreateSnapshot.Execute(ctx, actor, snapshotID)` usa el UUID de cliente recibido
como `Idempotency-Key`. El body del endpoint está fijado a `protocol_version: 1`.
`GetSnapshotPage.Execute(ctx, actor, snapshotID, page)` sirve páginas desde cero.
Los casos de uso están en [app](app/) y el puerto es
[repository.Snapshots](domain/repository/snapshots.go).

La creación exige `PersonalSnapshotUnitOfWork.SnapshotCommand`: transacción de
lectura/escritura REPEATABLE READ, pool de rol `mancuerna_data`, cuenta explícita
y ausencia de ámbitos de gimnasio o intento. Antes de consultar autoridad, el
UoW actualiza la fila del stream con `last_sequence=last_sequence`. Esa escritura
adquiere el mutex sin avanzar H. Todas las adquisiciones iniciales de autoridad
de cuenta usan el mismo mecanismo. Si una transacción RR empezó a observar antes
de esperar una revocación, PostgreSQL rechaza su escritura con `40001`; el callback
no recibe una vista antigua autorizada. Un reintento necesita una transacción
nueva, nunca continuar la transacción abortada.

Cada creación, repetición y GET de página comprueba `session_active` bajo el
mutex. El JWT por sí solo y conocer el manifest no autorizan descargas. El GET
usa una transacción Command corta READ COMMITTED, con el mismo ámbito y mutex.
El repositorio vuelve a comprobar rol, aislamiento y ámbito, además de aplicar
filtros `account_id` y RLS. Sus errores no incluyen SQL ni payloads.

## Fuente, corte y atomicidad

Epoch, H, perfil y revisiones se leen desde una sola vista RR. El perfil se
proyecta directamente desde `accounts`, excluyendo datos de autenticación.
Los demás registros provienen de la revisión canónica más reciente por
`(entity_type, entity_id)`, incluidos tombstones. Los tipos son perfil, ejercicio
personal, rutina, borrador, versión, workout, serie y conflicto.

Antes de materializar, un FULL JOIN contrasta IDs y revisiones del journal con
las raíces de negocio: rechaza raíces sin publicación, publicaciones sin raíz
y revisiones distintas. El perfil inicial de revisión 1 puede existir sin
journal; después se exige que su publicación coincida también con su proyección.
El orden es estable por `(entity_type COLLATE "C", entity_id)`.

Este control depende de que cada productor guarde raíz, revisión, canónico,
recibo y grupo de cambios en la misma transacción. Para los tipos distintos de
perfil no vuelve a construir todo el payload desde sus columnas. Una alteración
directa de columnas que conserve la revisión puede eludir esa comparación;
no se debe usar el materializador como reparación de datos ni como validador de
un nuevo productor. Cada caso de negocio debe demostrar esa invariante al
integrarse.

El repositorio usa un cursor SQL y obtiene un registro por FETCH. Cada resultado
se cierra antes de escribir la página y el cursor se cierra al salir, también en
error. El buffer conserva como máximo una página y el siguiente registro; la
lista de hashes crece con el número de páginas, limitado a 100 000. PostgreSQL
administra el trabajo de consulta y ordenación.

Cada página contiene hasta 1000 registros y 1 MiB de JSON canónico compacto UTF-8,
sin dividir registros. También se limita la representación JSONB almacenada,
más conservadora por sus espacios. El hash SHA-256 corresponde exclusivamente a
`records` canónico, no al wrapper HTTP. Los límites y hashes se vuelven a comprobar
al leer. Las páginas se insertan primero y el manifest al final; la FK diferida
de la migración 063 permite ese orden dentro de una sola transacción. Cualquier
fallo revierte páginas y metadata juntos y devuelve un resultado vacío.

## Repeticiones y vigencia

El manifest conserva el cursor, capabilities, hashes y vencimiento originales.
Repetir el mismo ID no lo rematerializa ni amplía su vida de 24 horas, incluso
después de rotar la clave de cursores o cambiar capabilities. El constructor y
las respuestas nuevas copian los campos mutables de capabilities. Los manifests
anteriores a 063 sin contenido completo se consideran vencidos y no se recrean.

Se rechazan snapshots vencidos, de otro epoch o cuyo H quedó detrás de
`compacted_through`. H superior al stream es un error de persistencia. Los IDs de
otra cuenta no permiten recuperar páginas ni reemplazar snapshots existentes.
El cursor original permite continuar `pull` estrictamente después de H.

## Evidencia y límites

La corrida integrada del 19 de octubre de 2026 pasó 235 pruebas principales con
PostgreSQL 18.3, roles restringidos y migraciones 059–063, sin pruebas omitidas.
El resumen persistente está en
`docs/mancuerna/etapa-1/entregas/evidencia/2026-10-19-B02/backend-bootstrap-authorization-intermedio.json`;
el manifest de fuentes adyacente fija la copia probada. Artefacto local original:
`/tmp/mancuerna-cuenta-ot_as_p3/result.json`.

Los tests de bootstrap viven en
`src/modules/personalaccounts/infraestructure/db/repositories/`:

- `snapshot_integration_test.go`: perfil inicial, replay con rotación de clave,
  cuentas A/B, TTL, epoch, revocación, corte H frente a un escritor realmente
  bloqueado, rollback posterior a páginas, últimas revisiones, tombstones,
  drift y compactación. Incluye 1001 ejercicios, límites por número de registros
  y páginas con texto Unicode que obliga a medir bytes UTF-8.
- `snapshot_transaction_integration_test.go`: espera real del mutex y rechazo
  `40001` tras revocación, aislamiento, privilegios y rollback del UoW.
- `snapshot_http_integration_test.go`: manifest y páginas mediante HTTP, hash,
  continuidad con pull, repetición y JWT cuya sesión fue revocada.

Los ejercicios de las pruebas son fixtures completos de infraestructura que
escriben raíces y `Commands.Commit` juntos. No acreditan un caso de uso funcional
de ejercicios, rutinas o entrenamiento. Esta evidencia tampoco acredita descarga
en staging, promoción atómica ni un recorrido completo de cuenta en el móvil.
La copia adicional de capabilities en cada respuesta se revisó después de esta
corrida; no se atribuye a la foto anterior una ejecución de ese cambio.

Para reproducir, usar `python3 tests/mancuerna/run.py` desde `cuadra-core`. El
runner crea un cluster desechable con socket Unix privado, no acepta una base
externa ni lee `.env`. No ejecutar las pruebas contra producción. Este README
describe implementación y evidencia técnica; no cambia el estado de historias.
