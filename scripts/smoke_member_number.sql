-- smoke_member_number.sql — verificación de ADR-010 (número de socio unificado).
--
-- Corre estas queries DESPUÉS de aplicar la migración 029 (Postgres) / 024
-- (SQLite) contra una base con datos reales (o el respaldo de prod) para
-- confirmar que el backfill + el índice único quedaron sanos.
--
--   Postgres:  psql "$DATABASE_URL" -f scripts/smoke_member_number.sql
--   SQLite:    sqlite3 ruta/al/tinta.db < scripts/smoke_member_number.sql
--
-- Resultados esperados:
--   (1) El índice único uq_members_gym_number existe.
--   (2) CERO grupos (gym_id, member_number) duplicados entre no-borrados
--       (si aparece >0, la reconciliación de la migración falló).
--   (3) Cobertura del backfill: members con número vs sin número. Los "sin
--       número" deberían ser sólo los que NO tenían un PIN plano numérico
--       (se les asigna en caliente al primer AssignMemberNumber/check-in).

-- (1) ¿Existe el índice único?  ── Postgres
--     (En SQLite usa la línea de abajo; comenta la que no aplique.)
SELECT 'pg_index_exists' AS check, indexname
  FROM pg_indexes
 WHERE tablename = 'members' AND indexname = 'uq_members_gym_number';
-- SQLite:
-- SELECT 'sqlite_index_exists' AS "check", name
--   FROM sqlite_master WHERE type = 'index' AND name = 'uq_members_gym_number';

-- (2) Duplicados preexistentes que la reconciliación debió eliminar.
--     DEBE devolver 0 filas.
SELECT gym_id, member_number, COUNT(*) AS dups
  FROM members
 WHERE deleted_at IS NULL AND member_number IS NOT NULL
 GROUP BY gym_id, member_number
HAVING COUNT(*) > 1
 ORDER BY dups DESC;

-- (3) Cobertura del backfill por gym: total vivos, con número, sin número,
--     y el rango asignado. "sin_numero" debería corresponder a socios sin
--     PIN plano numérico previo.
SELECT gym_id,
       COUNT(*)                                              AS socios_vivos,
       COUNT(member_number)                                  AS con_numero,
       COUNT(*) - COUNT(member_number)                       AS sin_numero,
       MIN(member_number)                                    AS min_num,
       MAX(member_number)                                    AS max_num
  FROM members
 WHERE deleted_at IS NULL
 GROUP BY gym_id
 ORDER BY socios_vivos DESC;
