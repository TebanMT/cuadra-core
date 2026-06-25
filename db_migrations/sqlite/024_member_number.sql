-- Migration 024 — ADR-010: número de socio unificado. Espeja postgres/029.
--
-- Añade members.member_number, backfilla desde el PIN plano (formato
-- histórico = exactamente 4 dígitos), reconcilia duplicados preexistentes y
-- crea el índice único (gym_id, member_number) entre no-borrados. pin_hash /
-- pin_plain quedan DEPRECADOS pero NO se borran todavía (rollback; el dominio
-- ya no los usa — ver member_sqlite.go).

BEGIN;

ALTER TABLE members ADD COLUMN member_number INTEGER;

-- Backfill desde el PIN plano. GLOB de 4 dígitos = el formato que generaba el
-- asignador viejo (ValidatePin exigía exactamente 4). PINs nulos quedan sin
-- número y se asignan luego en caliente.
UPDATE members
   SET member_number = CAST(pin_plain AS INTEGER)
 WHERE member_number IS NULL
   AND pin_plain IS NOT NULL
   AND pin_plain GLOB '[0-9][0-9][0-9][0-9]'
   AND deleted_at IS NULL;

-- Reconciliar duplicados preexistentes (misma regla que postgres/029: la fila
-- más antigua conserva su número; las más recientes reciben uno nuevo por
-- encima del máximo del gym).
WITH ranked AS (
    SELECT id, gym_id,
           ROW_NUMBER() OVER (PARTITION BY gym_id, member_number
                              ORDER BY created_at, id) AS rn
      FROM members
     WHERE deleted_at IS NULL AND member_number IS NOT NULL
),
maxnums AS (
    SELECT gym_id, COALESCE(MAX(member_number), 999) AS maxnum
      FROM members
     WHERE deleted_at IS NULL AND member_number IS NOT NULL
     GROUP BY gym_id
),
reassigned AS (
    SELECT rk.id,
           m.maxnum + ROW_NUMBER() OVER (PARTITION BY rk.gym_id ORDER BY rk.id) AS newnum
      FROM ranked rk
      JOIN maxnums m ON m.gym_id = rk.gym_id
     WHERE rk.rn > 1
)
UPDATE members
   SET member_number = (SELECT newnum FROM reassigned WHERE reassigned.id = members.id)
 WHERE id IN (SELECT id FROM reassigned);

CREATE UNIQUE INDEX IF NOT EXISTS uq_members_gym_number
    ON members (gym_id, member_number)
    WHERE deleted_at IS NULL AND member_number IS NOT NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 24, '024_member_number', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 24);

COMMIT;
