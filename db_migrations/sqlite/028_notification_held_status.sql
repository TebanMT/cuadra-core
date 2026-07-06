-- Migration 028 — agrega el estado `held` al CHECK de notification_queue.
-- Gemela de postgres/032 (lock-step): el dispatcher del cloud deja en
-- `held` las notificaciones más viejas que el TTL de su template en vez
-- de enviarlas tarde, y el mirror de notifications (cloud→sync_entities)
-- baja ese status a los sidecars. Sin este CHECK expandido, el primer
-- pull de una notificación retenida viola el CHECK local — exactamente
-- la clase de píldora venenosa de sync que el lock-step existe para
-- atrapar (el CI de check_lockstep.sh cachó este hueco).
--
-- SQLite no permite ALTER de CHECK constraints in-place: rebuild completo
-- con el patrón de 021 — PRAGMA foreign_keys=OFF para que (a) el DROP no
-- tropiece con la FK entrante de whatsapp_events.notification_id y (b) el
-- RENAME no reescriba esa FK (debe seguir apuntando a notification_queue,
-- el nombre final). Esquema final = 001 (base) + los dos ADD COLUMN de
-- 002 (idempotency_key, provider_message_id) + 'held' en el CHECK.
--
-- Idempotente: el runner skipea versiones registradas en _migrations; el
-- rebuild produce el mismo esquema final si corriera dos veces.

PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE notification_queue_canon (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    channel             TEXT NOT NULL CHECK (channel IN ('whatsapp','email','in_app')),
    template_key        TEXT NOT NULL,
    recipient_type      TEXT NOT NULL CHECK (recipient_type IN ('member','user')),
    recipient_id        TEXT NOT NULL,
    recipient_address   TEXT NOT NULL,
    payload             TEXT NOT NULL CHECK (json_valid(payload)),
    status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed','cancelled','held')),
    sent_at             INTEGER,
    failed_at           INTEGER,
    error_message       TEXT,
    retry_count         INTEGER NOT NULL DEFAULT 0,
    scheduled_for       INTEGER NOT NULL,
    idempotency_key     TEXT,
    provider_message_id TEXT
);

INSERT INTO notification_queue_canon
SELECT id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
       channel, template_key, recipient_type, recipient_id, recipient_address,
       payload, status, sent_at, failed_at, error_message, retry_count,
       scheduled_for, idempotency_key, provider_message_id
FROM notification_queue;

DROP TABLE notification_queue;
ALTER TABLE notification_queue_canon RENAME TO notification_queue;

-- Misma lista de índices que 001 + 002 — idempotentes via IF NOT EXISTS
-- aunque el DROP TABLE ya los tiró con la tabla vieja.
CREATE INDEX IF NOT EXISTS idx_notification_queue_pending ON notification_queue(scheduled_for, status) WHERE status = 'pending' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notification_queue_gym ON notification_queue(gym_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notification_queue_sync ON notification_queue(gym_id, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_notification_queue_idempotency ON notification_queue(gym_id, idempotency_key) WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 28, '028_notification_held_status', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 28);

COMMIT;

PRAGMA foreign_keys = ON;
