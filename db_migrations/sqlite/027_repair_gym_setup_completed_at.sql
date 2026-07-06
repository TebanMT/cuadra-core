-- Migration 027 — repara filas de gyms envenenadas con setup_completed_at
-- guardado como TEXT RFC3339 ("2026-06-28T19:14:04.453Z") en la columna
-- INTEGER (epoch-ms).
--
-- Origen del veneno: enqueueGym (pre core v1.0.6) emitía el campo como
-- *time.Time crudo → json.Marshal lo serializaba RFC3339 → el payload
-- quedó así en sync_entities del cloud → el apply del sync de un sidecar
-- que pull-eara el gym (full-sync de instalación fresca) escribía el
-- string TAL CUAL, y SQLite (tipado dinámico) lo aceptaba sin ruido. A
-- partir de ahí, todo Scan del gym en esa máquina moría con "converting
-- string to int64": recibos y bienvenidas nunca se encolaban, /auth/me y
-- charge-settings daban 500.
--
-- El productor y el apply ya están corregidos (epoch-ms en el wire +
-- coerción RFC3339→ms en extractColumnValue); esta migración sana el DATO
-- ya escrito en instalaciones que absorbieron el payload viejo, sin borrar
-- AppData ni re-login. Idempotente: typeof() sólo matchea filas
-- envenenadas; en bases sanas es no-op.
--
-- Precisión: strftime('%s') da segundos y strftime('%f') da "SS.SSS" —
-- substr(...,4) extrae los milisegundos para recomponer el epoch-ms
-- exacto (mismo valor que habría producido UnixMilli()).

BEGIN;

UPDATE gyms
SET setup_completed_at =
      CAST(strftime('%s', setup_completed_at) AS INTEGER) * 1000
    + CAST(substr(strftime('%f', setup_completed_at), 4) AS INTEGER)
WHERE typeof(setup_completed_at) = 'text'
  AND strftime('%s', setup_completed_at) IS NOT NULL;

-- Defensa: si algún string no parsea (corrupción exótica), mejor NULL que
-- una fila que revienta todos los Scan del gym. setup_completed_at es
-- nullable por diseño (NULL = "wizard no completado"); el peor efecto es
-- re-mostrar el setup wizard — el sync re-baja el valor canónico después.
UPDATE gyms
SET setup_completed_at = NULL
WHERE typeof(setup_completed_at) = 'text';

INSERT INTO _migrations (version, name, applied_at)
SELECT 27, '027_repair_gym_setup_completed_at', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 27);

COMMIT;
