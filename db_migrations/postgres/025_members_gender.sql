-- Migration 025 — añade columna `gender` a members para reportes operativos
-- del dueño (composición de la base activa, asistencias por género × hora —
-- caso clásico "horario de mujeres" en gyms MX). Espeja sqlite/020.
--
-- Enum cerrado de 3 valores: hombre / mujer / no_especificado. Nullable —
-- socios existentes no se backfillean; NULL ≡ no_especificado para reportes.
-- Si en el futuro hay que ampliar (no binario, etc.), va en migración propia.
-- No exponemos este campo en webhooks salientes ni en export default — es
-- dato personal (LFPDPPP), sin ser sensible.

BEGIN;

ALTER TABLE members
    ADD COLUMN IF NOT EXISTS gender VARCHAR(20);

ALTER TABLE members DROP CONSTRAINT IF EXISTS chk_members_gender;
ALTER TABLE members ADD CONSTRAINT chk_members_gender CHECK (
    gender IS NULL OR gender IN ('hombre','mujer','no_especificado')
);

INSERT INTO _migrations (version, name) VALUES (25, '025_members_gender')
ON CONFLICT (version) DO NOTHING;

COMMIT;
