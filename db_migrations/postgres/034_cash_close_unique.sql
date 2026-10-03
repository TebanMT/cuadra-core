-- UC-027: un cierre de caja vivo por gimnasio y día. Los cierres son
-- snapshots inmutables; si existen duplicados previos, la migración falla
-- deliberadamente para que se reconcilien en vez de borrar historia.
BEGIN;

DROP INDEX IF EXISTS idx_cash_close_events_gym_date;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_close_events_gym_date
    ON cash_close_events(gym_id, close_date)
    WHERE deleted_at IS NULL;

INSERT INTO _migrations (version, name) VALUES (34, '034_cash_close_unique')
ON CONFLICT (version) DO NOTHING;

COMMIT;
