-- Migration 017 — extender subscription_events con las 4 columnas estándar
-- de sync (version, created_at, updated_at, deleted_at). Espeja
-- sqlite/014_subscription_events.sql.
--
-- Por qué: el dueño consulta el historial de pagos desde el desktop
-- offline-first. Sin esto el sidecar tenía que devolver historial vacío
-- (un stub) o proxy-ear al cloud por cada apertura. Sumando las 4 columnas
-- el projector genérico puede mirror-earlas al sidecar como cualquier otra
-- entidad. Volumen es bajísimo (~5 rows/gym/mes) — costo de storage
-- despreciable.
--
-- Las filas existentes usan recorded_at como base para created_at /
-- updated_at (el ledger es append-only; un evento nunca se "actualiza"
-- después de insertarse, así que ambos timestamps coinciden con su
-- nacimiento). version=1 para todas. deleted_at queda null — webhooks
-- nunca borran eventos.

BEGIN;

ALTER TABLE subscription_events
    ADD COLUMN IF NOT EXISTS version    INTEGER     NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- Backfill: para filas pre-existentes el created/updated debería ser su
-- recorded_at, no NOW() (que sería incorrecto para un dato histórico).
UPDATE subscription_events
   SET created_at = recorded_at,
       updated_at = recorded_at
 WHERE created_at <> recorded_at;

CREATE INDEX IF NOT EXISTS ix_sub_event_sync ON subscription_events(updated_at);

INSERT INTO _migrations (version, name) VALUES (17, '017_subscription_events_sync')
ON CONFLICT (version) DO NOTHING;

COMMIT;
