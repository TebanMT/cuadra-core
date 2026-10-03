BEGIN;
ALTER TABLE products DROP CONSTRAINT IF EXISTS chk_products_stock;
ALTER TABLE inventory_purchases ADD COLUMN origin TEXT NOT NULL DEFAULT 'desktop' CHECK(origin IN ('desktop','cloud'));
UPDATE inventory_purchases SET origin='cloud' WHERE stock_movement_id IS NULL;
UPDATE sync_entities s SET payload=jsonb_set(s.payload,'{origin}',to_jsonb(p.origin))
FROM inventory_purchases p WHERE s.entity_type='inventory_purchases' AND s.entity_id=p.id AND s.gym_id=p.gym_id;
INSERT INTO _migrations(version,name,applied_at) VALUES(50,'050_purchase_registration',NOW());
COMMIT;
