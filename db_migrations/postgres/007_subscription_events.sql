-- Subscription events ledger.
--
-- Cloud-only. Source of truth for billing state is Stripe / Mercado Pago; this
-- table is what we use to reconcile, debug, and replay processor events. The
-- gym aggregate's subscription_status / subscription_plan / subscription_ends_at
-- are mutated by the use case that inserts here.
--
-- The (provider, external_id) pair is unique so duplicate webhook deliveries
-- (Stripe retries on non-2xx) become idempotent at the persistence layer.

CREATE TABLE IF NOT EXISTS subscription_events (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL,
    type            TEXT NOT NULL,
    external_id     TEXT NOT NULL,
    plan            TEXT NOT NULL,
    amount          NUMERIC(12, 2),
    currency        TEXT,
    period_ends_at  TIMESTAMPTZ,
    raw_payload     JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at     TIMESTAMPTZ NOT NULL,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_sub_provider CHECK (provider IN ('stripe', 'mercadopago', 'manual')),
    CONSTRAINT chk_sub_type     CHECK (type IN ('activated', 'renewed', 'past_due', 'cancelled', 'trial_extended'))
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_sub_event_external
    ON subscription_events (provider, external_id);

CREATE INDEX IF NOT EXISTS ix_sub_event_gym_occurred
    ON subscription_events (gym_id, occurred_at DESC);

INSERT INTO _migrations (version, name) VALUES (7, '007_subscription_events')
ON CONFLICT (version) DO NOTHING;
