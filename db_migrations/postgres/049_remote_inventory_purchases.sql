BEGIN;
ALTER TABLE inventory_purchases ALTER COLUMN stock_movement_id DROP NOT NULL;
ALTER TABLE products ADD COLUMN stock_base INTEGER;
UPDATE products SET stock_base=stock;
CREATE TABLE inventory_purchase_receipts (
 id UUID PRIMARY KEY,
 gym_id UUID NOT NULL REFERENCES gyms(id),
 version INTEGER NOT NULL DEFAULT 1 CHECK(version=1),
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
 deleted_at TIMESTAMPTZ CHECK(deleted_at IS NULL),
 purchase_id UUID NOT NULL UNIQUE REFERENCES inventory_purchases(id),
 product_id UUID NOT NULL REFERENCES products(id),
 quantity INTEGER NOT NULL CHECK(quantity > 0),
 unit_cost NUMERIC(12,2) NOT NULL CHECK(unit_cost > 0),
 received_by UUID NOT NULL REFERENCES users(id),
 CHECK(id=purchase_id)
);
CREATE INDEX idx_purchase_receipts_product ON inventory_purchase_receipts(gym_id,product_id);

-- Publish journal timestamps in commit order. A timestamp assigned when an
-- INSERT runs can be skipped by a reader if that transaction commits late.
-- This deferred trigger runs after business writes, so the per-gym lock is
-- never held while waiting for another operation's product/purchase locks.
-- All journal writers (including older cloud paths) use the same boundary.
CREATE FUNCTION tinta_stamp_sync_commit() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  next_stamp TIMESTAMPTZ;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('tinta:sync-feed:' || NEW.gym_id::text, 0));
  SELECT GREATEST(clock_timestamp(), COALESCE(MAX(server_updated_at), 'epoch'::timestamptz) + interval '1 microsecond')
    INTO next_stamp FROM sync_entities WHERE gym_id=NEW.gym_id;
  -- WHEN is evaluated when the row changes. Suppress only the internal stamp,
  -- otherwise it would enqueue another deferred trigger recursively.
  PERFORM set_config('tinta.sync_stamping', 'on', true);
  UPDATE sync_entities SET server_updated_at=next_stamp
    WHERE gym_id=NEW.gym_id AND entity_type=NEW.entity_type AND entity_id=NEW.entity_id;
  PERFORM set_config('tinta.sync_stamping', 'off', true);
  RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER tinta_sync_commit_stamp
AFTER INSERT OR UPDATE ON sync_entities DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW WHEN (current_setting('tinta.sync_stamping', true) IS DISTINCT FROM 'on')
EXECUTE FUNCTION tinta_stamp_sync_commit();

INSERT INTO _migrations(version,name,applied_at) VALUES(49,'049_remote_inventory_purchases',NOW());
COMMIT;
