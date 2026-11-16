-- Repair databases that recorded 040_cash_sessions but retained the legacy
-- one-close-per-gym/day index. The old index rejects valid sessions from a
-- second drawer or turn before the natural cash-session key can be evaluated.
-- This is schema-only: no cash close, transfer, or payment row is changed.
BEGIN;

DROP INDEX IF EXISTS public.uq_cash_close_events_gym_date;

CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_sessions_natural
  ON public.cash_close_events(gym_id, drawer_id, operational_date, sequence)
  WHERE deleted_at IS NULL;

INSERT INTO _migrations(version, name)
VALUES(66, '066_repair_cash_session_legacy_unique')
ON CONFLICT(version) DO NOTHING;

COMMIT;
