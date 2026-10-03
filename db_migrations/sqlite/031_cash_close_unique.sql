-- Espejo SQLite de PG 034: un cierre vivo por gimnasio y día.
BEGIN;

DROP INDEX IF EXISTS idx_cash_close_events_gym_date;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_close_events_gym_date
    ON cash_close_events(gym_id, close_date)
    WHERE deleted_at IS NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 31, '031_cash_close_unique', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 31);

COMMIT;
