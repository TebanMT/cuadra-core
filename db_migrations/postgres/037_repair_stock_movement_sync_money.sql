-- Repair historical stock_movements sync journal payloads that stored cost
-- in cents even though the canonical sync wire is pesos. A sidecar multiplied
-- that stale value by 100 again when materialising SQLite, making desktop
-- COGS differ from the dashboard.
--
-- Bump the domain and journal versions so already-synced sidecars receive the
-- canonical cost through the normal incremental pull. Rows without cost do
-- not need repair.
BEGIN;

WITH repaired AS (
    UPDATE stock_movements sm
       SET version = sm.version + 1,
           updated_at = NOW()
     WHERE sm.cost IS NOT NULL
       AND EXISTS (
           SELECT 1
             FROM sync_entities se
            WHERE se.gym_id = sm.gym_id
              AND se.entity_type = 'stock_movements'
              AND se.entity_id = sm.id
       )
     RETURNING sm.*
)
UPDATE sync_entities se
   SET version = repaired.version,
       payload = se.payload || jsonb_build_object(
           'id',            repaired.id,
           'gym_id',        repaired.gym_id,
           'version',       repaired.version,
           'product_id',    repaired.product_id,
           'movement_type', repaired.movement_type,
           'delta',         repaired.delta,
           'reason',        repaired.reason,
           'cost',          repaired.cost,
           'is_purchase',   repaired.is_purchase,
           'sale_item_id',  repaired.sale_item_id,
           'operator_id',   repaired.operator_id,
           'created_at', (EXTRACT(EPOCH FROM repaired.created_at) * 1000)::bigint,
           'updated_at', (EXTRACT(EPOCH FROM repaired.updated_at) * 1000)::bigint
       ),
       server_updated_at = NOW(),
       deleted_at = repaired.deleted_at
  FROM repaired
 WHERE se.gym_id = repaired.gym_id
   AND se.entity_type = 'stock_movements'
   AND se.entity_id = repaired.id;

INSERT INTO _migrations(version, name)
VALUES (37, '037_repair_stock_movement_sync_money')
ON CONFLICT(version) DO NOTHING;

COMMIT;
