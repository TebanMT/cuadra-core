-- Cuadra — Sesión 7 / notifications BC (ADR-007).
-- Adds whatsapp_events (Twilio webhook log) + notification_templates (per-gym
-- editable copy of the system templates, UC-038 DA-38.2).
-- Idempotent.

BEGIN;

-- ---------------------------------------------------------------------------
-- whatsapp_events — every StatusCallback / IncomingMessage Twilio sends us.
-- Used for debug, compliance, and to update notification_queue.status when
-- the actual delivery happens after our `Send` call returned.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS whatsapp_events (
    id                  UUID PRIMARY KEY,
    gym_id              UUID REFERENCES gyms(id) ON DELETE RESTRICT,
    notification_id     UUID REFERENCES notification_queue(id) ON DELETE SET NULL,
    provider_message_id TEXT NOT NULL,
    event_type          TEXT NOT NULL,                 -- 'status' | 'incoming'
    status              TEXT,                          -- 'queued'|'sent'|'delivered'|'read'|'failed'|'undelivered'
    error_code          TEXT,
    error_message       TEXT,
    raw_payload         JSONB NOT NULL,
    received_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_whatsapp_events_event_type CHECK (event_type IN ('status','incoming'))
);
CREATE INDEX IF NOT EXISTS idx_whatsapp_events_provider_msg ON whatsapp_events(provider_message_id);
CREATE INDEX IF NOT EXISTS idx_whatsapp_events_notification ON whatsapp_events(notification_id) WHERE notification_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_whatsapp_events_gym ON whatsapp_events(gym_id, received_at DESC);

-- ---------------------------------------------------------------------------
-- notification_templates — per-gym overrides of the default template body.
-- Defaults live in code (templates.DefaultLibrary). A row exists in this
-- table only when the owner has explicitly customised the message via
-- UC-038 DA-38.2. Variables are FIXED — only the surrounding copy is editable.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS notification_templates (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    template_key    TEXT NOT NULL,                  -- e.g. 'expiry_reminder_3d'
    body            TEXT NOT NULL,                  -- editable text with {var} placeholders
    enabled         BOOLEAN NOT NULL DEFAULT TRUE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_notification_templates_gym_key ON notification_templates(gym_id, template_key) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notification_templates_sync ON notification_templates(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- notification_queue idempotency_key — prevents UC-038 scheduler duplicates
-- for (member_id, template_key, reference_date).
-- ---------------------------------------------------------------------------
ALTER TABLE notification_queue
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uq_notification_queue_idempotency ON notification_queue(gym_id, idempotency_key) WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- notification_queue.provider_message_id — set after successful Twilio send.
-- ---------------------------------------------------------------------------
ALTER TABLE notification_queue
    ADD COLUMN IF NOT EXISTS provider_message_id TEXT;

INSERT INTO _migrations (version, name) VALUES (2, '002_notifications')
ON CONFLICT (version) DO NOTHING;

COMMIT;
