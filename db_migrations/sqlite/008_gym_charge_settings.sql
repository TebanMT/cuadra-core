-- Migration 008 — configuración de cobros a nivel gym (inscripción +
-- mantenimiento). Espeja postgres/011. SQLite acepta ALTER TABLE ADD
-- COLUMN sin recrear la tabla mientras no haya constraints complejos.

BEGIN;

ALTER TABLE gyms
    ADD COLUMN charge_settings TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(charge_settings));

INSERT INTO _migrations (version, name, applied_at)
SELECT 8, '008_gym_charge_settings', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 8);

COMMIT;
