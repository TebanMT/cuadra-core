-- Cuadra — Sesión 7 / notifications BC (ADR-007), SQLite mirror of postgres/002.
-- Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS whatsapp_events (
    id                  TEXT PRIMARY KEY,
    gym_id              TEXT REFERENCES gyms(id) ON DELETE RESTRICT,
    notification_id     TEXT REFERENCES notification_queue(id) ON DELETE SET NULL,
    provider_message_id TEXT NOT NULL,
    event_type          TEXT NOT NULL CHECK (event_type IN ('status','incoming')),
    status              TEXT,
    error_code          TEXT,
    error_message       TEXT,
    raw_payload         TEXT NOT NULL CHECK (json_valid(raw_payload)),
    received_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_whatsapp_events_provider_msg ON whatsapp_events(provider_message_id);
CREATE INDEX IF NOT EXISTS idx_whatsapp_events_notification ON whatsapp_events(notification_id) WHERE notification_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_whatsapp_events_gym ON whatsapp_events(gym_id, received_at DESC);

CREATE TABLE IF NOT EXISTS notification_templates (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    template_key    TEXT NOT NULL,
    body            TEXT NOT NULL,
    enabled         INTEGER NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_notification_templates_gym_key ON notification_templates(gym_id, template_key) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notification_templates_sync ON notification_templates(gym_id, updated_at);

-- SQLite supports ALTER TABLE ADD COLUMN. Wrap each in a guard via PRAGMA-style
-- check would require a function — instead we rely on the migration only
-- applying once (recorded in _migrations).
ALTER TABLE notification_queue ADD COLUMN idempotency_key TEXT;
ALTER TABLE notification_queue ADD COLUMN provider_message_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uq_notification_queue_idempotency ON notification_queue(gym_id, idempotency_key) WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 2, '002_notifications', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 2);

COMMIT;
