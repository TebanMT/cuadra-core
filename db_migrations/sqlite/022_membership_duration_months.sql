-- Migration 022 — duración por meses naturales (UC-011 fix). Espeja
-- postgres/027.
--
-- Bug: una membresía "mensual" se cobraba como 30 días naturales —
-- 25-may + 30d = 24-jun en lugar de 25-jun. Sólo las personalizadas
-- (1 día / 7d / 15d / N) usan días; mensuales/bimestrales/anuales etc.
-- son meses naturales del calendario.
--
-- Agregamos duration_months nullable. NULL = sigue como duration_days
-- (legacy + presets exactos). NOT NULL = start + N meses naturales
-- (overflow en dominio).
--
-- CHECK inline porque SQLite no permite ALTER TABLE ADD CONSTRAINT.
-- Las filas pre-feature son todas NULL al aplicar.

BEGIN;

ALTER TABLE membership_types ADD COLUMN duration_months INTEGER
    CHECK (duration_months IS NULL OR (duration_months BETWEEN 1 AND 60));

ALTER TABLE memberships ADD COLUMN duration_months_snapshot INTEGER
    CHECK (duration_months_snapshot IS NULL OR (duration_months_snapshot BETWEEN 1 AND 60));

-- Backfill de presets ambiguos solamente.
UPDATE membership_types SET duration_months = 1
    WHERE duration_months IS NULL AND duration_days = 30;
UPDATE membership_types SET duration_months = 2
    WHERE duration_months IS NULL AND duration_days = 60;
UPDATE membership_types SET duration_months = 3
    WHERE duration_months IS NULL AND duration_days = 90;
UPDATE membership_types SET duration_months = 6
    WHERE duration_months IS NULL AND duration_days = 180;
UPDATE membership_types SET duration_months = 12
    WHERE duration_months IS NULL AND duration_days = 365;

-- memberships: no tocamos snapshots (inmutables — DA-11.1).

INSERT INTO _migrations (version, name, applied_at)
SELECT 22, '022_membership_duration_months', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 22);

COMMIT;
