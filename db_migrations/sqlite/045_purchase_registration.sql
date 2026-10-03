-- Physical counts stay nonnegative when entered manually; sales may reveal a
-- missing receipt by leaving a negative balance. Rebuild without renaming the
-- old table so all existing foreign keys continue to reference products.
PRAGMA foreign_keys = OFF;
BEGIN;
CREATE TABLE products_next_045 (
 id TEXT PRIMARY KEY, gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
 version INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 deleted_at INTEGER, synced_at INTEGER, name TEXT NOT NULL,
 price INTEGER NOT NULL CHECK(price>0), stock INTEGER NOT NULL DEFAULT 0,
 stock_minimum INTEGER NOT NULL DEFAULT 0, category TEXT, image_url TEXT,
 active INTEGER NOT NULL DEFAULT 1, stock_base INTEGER
);
INSERT INTO products_next_045 SELECT id,gym_id,version,created_at,updated_at,deleted_at,synced_at,name,price,stock,stock_minimum,category,image_url,active,stock_base FROM products;
DROP TABLE products;
ALTER TABLE products_next_045 RENAME TO products;
CREATE UNIQUE INDEX uq_products_gym_name ON products(gym_id,name COLLATE NOCASE) WHERE deleted_at IS NULL;
CREATE INDEX idx_products_gym_active ON products(gym_id,active) WHERE deleted_at IS NULL;
CREATE INDEX idx_products_sync ON products(gym_id,updated_at);
ALTER TABLE inventory_purchases ADD COLUMN origin TEXT NOT NULL DEFAULT 'desktop' CHECK(origin IN ('desktop','cloud'));
UPDATE inventory_purchases SET origin='cloud' WHERE stock_movement_id IS NULL;
INSERT INTO _migrations(version,name,applied_at) VALUES(45,'045_purchase_registration',CAST(strftime('%s','now') AS INTEGER)*1000);
COMMIT;
PRAGMA foreign_keys = ON;
