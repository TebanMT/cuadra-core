-- Migration 015 — UNIQUE constraint para gyms.whatsapp en SQLite local.
-- Espeja postgres/018_gyms_whatsapp_unique.sql.
--
-- El sidecar también enforces la unicidad localmente. Razón: si el sync
-- baja dos filas conflictivas del cloud (poco probable porque cloud ya
-- enforce, pero defensa en profundidad), el INSERT al SQLite va a fallar
-- en lugar de quedar con datos inconsistentes silenciosamente.

BEGIN;

-- Soft-delete duplicados — mismo criterio que Postgres: preservamos el
-- más antiguo de cada grupo. SQLite no tiene ROW_NUMBER en versiones muy
-- viejas pero sí desde 3.25 (lo que el sidecar usa).
WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (PARTITION BY whatsapp ORDER BY created_at ASC, id ASC) AS rn
    FROM gyms
    WHERE whatsapp IS NOT NULL
      AND deleted_at IS NULL
)
UPDATE gyms
   SET deleted_at = CAST(strftime('%s','now') AS INTEGER) * 1000,
       updated_at = CAST(strftime('%s','now') AS INTEGER) * 1000,
       version    = version + 1
 WHERE id IN (SELECT id FROM ranked WHERE rn > 1);

CREATE UNIQUE INDEX IF NOT EXISTS uq_gyms_whatsapp
    ON gyms(whatsapp)
    WHERE whatsapp IS NOT NULL AND deleted_at IS NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 15, '015_gyms_whatsapp_unique', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 15);

COMMIT;
