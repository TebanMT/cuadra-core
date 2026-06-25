-- Migration 026 — ADR-010: arregla los payloads 'pin' pendientes en sync_queue.
--
-- La migración 025 reescribió la TABLA checkins (method 'pin' → 'number') pero
-- NO el sync_queue: un check-in hecho por PIN, encolado y AÚN SIN PUSHEAR
-- (sidecar offline justo al momento del deploy), conserva "method":"pin" en su
-- payload snapshot. Tras el deploy el cloud (postgres/030) rechaza el CHECK con
-- 'pin' → la fila de cola queda ENVENENADA (error perpetuo en /sync/status) y
-- esa asistencia nunca llega al historial del dashboard.
--
-- Reescribimos esos payloads pendientes a 'number' (idéntico a como la 025
-- reescribió la tabla). Las migraciones corren al arranque del sidecar ANTES
-- de que el agent empiece a pushear, así que la cola sale corregida.
-- Idempotente: sólo toca filas pendientes (synced_at IS NULL) con method=pin.

BEGIN;

UPDATE sync_queue
SET payload = json_set(payload, '$.method', 'number')
WHERE entity_type = 'checkins'
  AND synced_at IS NULL
  AND json_extract(payload, '$.method') = 'pin';

INSERT INTO _migrations (version, name, applied_at)
SELECT 26, '026_checkin_pin_queue_fix', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 26);

COMMIT;
