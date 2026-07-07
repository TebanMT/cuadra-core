-- Migration 029 — sync_quarantine (self-heal del pull).
--
-- Problema: el pull aplica cada página en UNA transacción y avanza el
-- cursor (last_pulled_at) DENTRO de esa misma tx. Si una sola fila no se
-- puede aplicar (bug de esquema tipo "no such column", un CHECK que
-- rebota, un dato imposible del cloud), toda la tx hace rollback → el
-- cursor no avanza → la MISMA página se re-baja cada tick → sync
-- permanentemente atascado en AMBAS direcciones, aunque el otro 99% de
-- los cambios estén sanos. Le pasó al piloto con owner_alert_configs.
--
-- sync_quarantine registra las filas que fallan al aplicarse. Tras
-- `quarantineThreshold` intentos (no al primer blip transitorio), el pull
-- las SALTA: aplica el resto de la página y avanza el cursor, dejando
-- constancia de qué se saltó (nunca silencioso — el estado de sync lo
-- refleja y soporte puede leer el error). La fila se re-intenta sola
-- cuando el cloud sube su versión (el projector bumpea server_updated_at
-- → vuelve a bajar → se resetea el conteo) o en un full-sync.

BEGIN;

CREATE TABLE IF NOT EXISTS sync_quarantine (
    entity_type   TEXT NOT NULL,
    entity_id     TEXT NOT NULL,
    -- version del change saltado. Si baja una version MAYOR, reseteamos
    -- attempts (el cloud cambió la fila → merece un intento nuevo).
    version       INTEGER NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 1,
    last_error    TEXT,
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,

    PRIMARY KEY (entity_type, entity_id)
);

INSERT INTO _migrations (version, name, applied_at)
SELECT 29, '029_sync_quarantine', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 29);

COMMIT;
