-- Migration 029 — ADR-010: número de socio unificado.
--
-- El antiguo "PIN de acceso" del socio (pin_hash bcrypt + pin_plain de la
-- migración 012) se reposiciona como el NÚMERO DE SOCIO: público, entero,
-- único 1:1 por gym, y a la vez credencial de check-in. Esta migración:
--   1. añade members.member_number (INT, nullable durante la transición),
--   2. backfilla desde el PIN plano actual (ya único por gym por dispositivo),
--   3. reconcilia duplicados preexistentes (colisiones offline pasadas),
--   4. crea el índice único (gym_id, member_number) entre no-borrados.
-- pin_hash / pin_plain quedan DEPRECADOS pero NO se borran todavía
-- (ADR-010 §4.5: se conserva una versión por rollback; un migration futuro
-- hace el DROP una vez que el check-in por número esté estable en prod).

BEGIN;

-- 1) Columna nullable durante la transición.
ALTER TABLE members ADD COLUMN IF NOT EXISTS member_number INTEGER;

-- 2) Backfill: member_number = el PIN plano actual. Sólo PINs puramente
--    numéricos; nulos / no-numéricos quedan sin número (se les asigna luego
--    en caliente vía AssignMemberNumber).
UPDATE members
   SET member_number = CAST(pin_plain AS INTEGER)
 WHERE member_number IS NULL
   AND pin_plain ~ '^[0-9]+$'
   AND deleted_at IS NULL;

-- 3) Reconciliar duplicados preexistentes: en cada grupo (gym_id,
--    member_number) con >1 fila viva, se conserva la MÁS ANTIGUA (created_at)
--    con su número y se reasigna a las más recientes un número nuevo por
--    encima del máximo del gym — consistente con la regla del projector "el
--    que llegó después pierde" (ADR-010 §2.3 / §4.3).
WITH ranked AS (
    SELECT id, gym_id,
           ROW_NUMBER() OVER (PARTITION BY gym_id, member_number
                              ORDER BY created_at, id) AS rn
      FROM members
     WHERE deleted_at IS NULL AND member_number IS NOT NULL
),
losers AS (
    SELECT id, gym_id FROM ranked WHERE rn > 1
),
maxnums AS (
    SELECT gym_id, COALESCE(MAX(member_number), 999) AS maxnum
      FROM members
     WHERE deleted_at IS NULL AND member_number IS NOT NULL
     GROUP BY gym_id
),
reassigned AS (
    SELECT l.id,
           m.maxnum + ROW_NUMBER() OVER (PARTITION BY l.gym_id ORDER BY l.id) AS newnum
      FROM losers l
      JOIN maxnums m ON m.gym_id = l.gym_id
)
UPDATE members
   SET member_number = r.newnum
  FROM reassigned r
 WHERE members.id = r.id;

-- 4) Índice único entre no-borrados con número (enforce real, ADR-010 §2.3).
CREATE UNIQUE INDEX IF NOT EXISTS uq_members_gym_number
    ON members (gym_id, member_number)
    WHERE deleted_at IS NULL AND member_number IS NOT NULL;

INSERT INTO _migrations (version, name) VALUES (29, '029_member_number')
ON CONFLICT (version) DO NOTHING;

COMMIT;
