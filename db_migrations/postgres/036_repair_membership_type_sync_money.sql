-- Repair historical membership_types sync journal payloads that stored money
-- in cents even though the canonical sync wire is pesos.
--
-- Bump BOTH the domain row and journal version so already-synced sidecars do
-- not discard the corrected payload under normal incremental LWW. The live
-- Postgres row is authoritative for every projected field below.
BEGIN;

WITH repaired AS (
    UPDATE membership_types mt
       SET version = mt.version + 1,
           updated_at = NOW()
     WHERE EXISTS (
         SELECT 1
           FROM sync_entities se
          WHERE se.gym_id = mt.gym_id
            AND se.entity_type = 'membership_types'
            AND se.entity_id = mt.id
     )
     RETURNING mt.*
)
UPDATE sync_entities se
   SET version = repaired.version,
       payload = se.payload || jsonb_build_object(
           'id',                    repaired.id,
           'gym_id',                repaired.gym_id,
           'version',               repaired.version,
           'name',                  repaired.name,
           'price',                 repaired.price,
           'duration_days',         repaired.duration_days,
           'duration_months',       repaired.duration_months,
           'enrollment_fee',        repaired.enrollment_fee,
           'maintenance_fee',       repaired.maintenance_fee,
           'maintenance_frequency', repaired.maintenance_frequency,
           'active',                repaired.active,
           'created_at', (EXTRACT(EPOCH FROM repaired.created_at) * 1000)::bigint,
           'updated_at', (EXTRACT(EPOCH FROM repaired.updated_at) * 1000)::bigint
       ),
       server_updated_at = NOW(),
       deleted_at = repaired.deleted_at
  FROM repaired
 WHERE se.gym_id = repaired.gym_id
   AND se.entity_type = 'membership_types'
   AND se.entity_id = repaired.id;

INSERT INTO _migrations(version, name)
VALUES (36, '036_repair_membership_type_sync_money')
ON CONFLICT(version) DO NOTHING;

COMMIT;
