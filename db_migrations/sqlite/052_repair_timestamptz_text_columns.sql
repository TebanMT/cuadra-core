-- Migration 052 — repara columnas de fecha que NO terminan en `_at` y que
-- el apply del sync guardó como TEXT RFC3339 en vez de epoch-ms.
--
-- Origen del veneno: el apply (extractColumnValue) sólo convertía a
-- epoch-ms los strings RFC3339 de columnas con sufijo `_at`. Las columnas
-- TIMESTAMPTZ del cloud sin ese sufijo (catálogo timestamptzColumns del
-- projector: notification_queue.scheduled_for y las dos de medición de
-- challenges) se escribían TAL CUAL cuando el payload del pull las traía
-- como string — p.ej. "2026-08-31T17:42:37.960967-06:00" en recordatorios
-- generados por el cloud. SQLite (tipado dinámico) lo acepta sin ruido y
-- todo Scan posterior a int64 de esa fila muere con "converting string to
-- int64": el historial de WhatsApp del socio daba 500.
--
-- El apply ya usa el mismo catálogo que el projector (isTimestampColumn);
-- esta migración sana el DATO ya escrito. Idempotente: typeof() sólo
-- matchea filas envenenadas; en bases sanas es no-op. Mismo cálculo de
-- precisión que la 027 (segundos de %s + milisegundos de %f).

BEGIN;

UPDATE notification_queue
SET scheduled_for =
      CAST(strftime('%s', scheduled_for) AS INTEGER) * 1000
    + CAST(substr(strftime('%f', scheduled_for), 4) AS INTEGER)
WHERE typeof(scheduled_for) = 'text'
  AND strftime('%s', scheduled_for) IS NOT NULL;

-- Defensa: scheduled_for es NOT NULL. Si un string no parsea, created_at es
-- la mejor aproximación (los avisos se programan al crearse o después) y
-- evita que la fila reviente todos los Scan. El sync re-baja el canónico.
UPDATE notification_queue
SET scheduled_for = created_at
WHERE typeof(scheduled_for) = 'text';

UPDATE challenges
SET measurement_t0_deadline =
      CAST(strftime('%s', measurement_t0_deadline) AS INTEGER) * 1000
    + CAST(substr(strftime('%f', measurement_t0_deadline), 4) AS INTEGER)
WHERE typeof(measurement_t0_deadline) = 'text'
  AND strftime('%s', measurement_t0_deadline) IS NOT NULL;

UPDATE challenges
SET measurement_t1_start =
      CAST(strftime('%s', measurement_t1_start) AS INTEGER) * 1000
    + CAST(substr(strftime('%f', measurement_t1_start), 4) AS INTEGER)
WHERE typeof(measurement_t1_start) = 'text'
  AND strftime('%s', measurement_t1_start) IS NOT NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 52, '052_repair_timestamptz_text_columns', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 52);

COMMIT;
