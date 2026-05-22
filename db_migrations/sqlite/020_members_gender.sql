-- Migration 020 — añade columna `gender` a members. Espeja postgres/025.
--
-- Mismo rationale: reportes operativos del dueño (composición + asistencias
-- por género × hora). NULL para socios pre-feature; ≡ no_especificado en
-- agregados. El CHECK se declara inline porque SQLite no permite ALTER TABLE
-- ADD CONSTRAINT post-hoc — viable porque las filas existentes son todas
-- NULL al momento de aplicar la migración.

BEGIN;

ALTER TABLE members ADD COLUMN gender TEXT
    CHECK (gender IS NULL OR gender IN ('hombre','mujer','no_especificado'));

INSERT INTO _migrations (version, name, applied_at)
SELECT 20, '020_members_gender', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 20);

COMMIT;
