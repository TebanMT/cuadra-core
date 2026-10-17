-- Preserve the existing count, including imported history without events.
-- Thereafter the immutable movements, not a device snapshot, determine stock.
BEGIN;

ALTER TABLE products ADD COLUMN stock_origin bigint;
UPDATE products p SET stock_origin=p.stock-COALESCE((
  SELECT SUM(delta) FROM stock_movements m
   WHERE m.gym_id=p.gym_id AND m.product_id=p.id AND m.deleted_at IS NULL
),0);

CREATE FUNCTION tinta_product_stock() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE physical bigint; receipts bigint;
BEGIN
  IF TG_OP='UPDATE' AND OLD.stock_origin IS NOT NULL THEN
    NEW.stock_origin := OLD.stock_origin;
  END IF;
  -- NULL is reserved for legacy import writers which have not supplied a
  -- complete event history. Normal product creation explicitly uses zero.
  IF NEW.stock_origin IS NULL THEN RETURN NEW; END IF;
  SELECT COALESCE(SUM(delta),0) INTO physical FROM stock_movements
    WHERE gym_id=NEW.gym_id AND product_id=NEW.id AND deleted_at IS NULL;
  SELECT COALESCE(SUM(quantity),0) INTO receipts FROM inventory_purchase_receipts
    WHERE gym_id=NEW.gym_id AND product_id=NEW.id;
  NEW.stock := NEW.stock_origin + physical;
  NEW.stock_base := NEW.stock - receipts;
  RETURN NEW;
END $$;
CREATE TRIGGER product_stock_from_ledger BEFORE INSERT OR UPDATE ON products
FOR EACH ROW EXECUTE FUNCTION tinta_product_stock();

CREATE FUNCTION tinta_stock_event_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.product_id,NEW.delta,NEW.movement_type,NEW.sale_item_id,NEW.deleted_at)
     IS DISTINCT FROM (OLD.product_id,OLD.delta,OLD.movement_type,OLD.sale_item_id,OLD.deleted_at) THEN
    RAISE EXCEPTION 'stock event must be reversed with a new movement'
      USING ERRCODE='23514', CONSTRAINT='stock_event_immutable';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER stock_event_immutable BEFORE UPDATE ON stock_movements
FOR EACH ROW EXECUTE FUNCTION tinta_stock_event_immutable();

CREATE FUNCTION tinta_stock_event_publish() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE p products%ROWTYPE;
BEGIN
  -- Same lock as product projection and receipt application. Identical
  -- replays do not increment inventory or manufacture a new movement.
  IF TG_OP='UPDATE' THEN RETURN NEW; END IF;
  UPDATE products SET version=GREATEST(version,COALESCE((
      SELECT version FROM sync_entities WHERE gym_id=NEW.gym_id
      AND entity_type='products' AND entity_id=NEW.product_id),0))+1,
      updated_at=clock_timestamp()
    WHERE gym_id=NEW.gym_id AND id=NEW.product_id AND stock_origin IS NOT NULL
    RETURNING * INTO p;
  IF FOUND THEN
    INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at,deleted_at)
    VALUES(p.gym_id,'products',p.id,p.version,to_jsonb(p)-'stock_origin',clock_timestamp(),p.deleted_at)
    ON CONFLICT(gym_id,entity_type,entity_id) DO UPDATE
      SET version=EXCLUDED.version,payload=EXCLUDED.payload,server_updated_at=EXCLUDED.server_updated_at;
  END IF;
  -- Receipt movements are derived locally by every supported desktop.
  -- Publishing them independently would collide with receipt application.
  IF NEW.idempotency_key LIKE 'purchase-receipt:%' THEN RETURN NEW; END IF;
  -- Native cloud stock writes used to omit the movement from the journal.
  INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at,deleted_at)
  VALUES(NEW.gym_id,'stock_movements',NEW.id,NEW.version,to_jsonb(NEW),clock_timestamp(),NEW.deleted_at)
  ON CONFLICT(gym_id,entity_type,entity_id) DO NOTHING;
  RETURN NEW;
END $$;
CREATE TRIGGER stock_event_publish AFTER INSERT ON stock_movements
FOR EACH ROW EXECUTE FUNCTION tinta_stock_event_publish();

INSERT INTO _migrations(version,name) VALUES(56,'056_stock_ledger')
ON CONFLICT(version) DO NOTHING;
COMMIT;
