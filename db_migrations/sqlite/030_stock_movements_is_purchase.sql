-- 030 — stock_movements.is_purchase (espejo de postgres/033).
--
-- Distingue una COMPRA real (salió dinero) de la captura de inventario que
-- ya existía físicamente (alta de catálogo / hallazgo en conteo). El alta
-- de producto sembraba el stock inicial como movement_type='restock' con
-- costo, indistinguible de una compra → la carga de catálogo inflaba
-- "egresos por mercancía" y el mes arrancaba con utilidad negativa.
--
-- Default 1: todo lo histórico conserva su semántica (compra). Los egresos
-- filtran is_purchase; el costo promedio / margen NO filtra — las capturas
-- iniciales son la base del COGS.

BEGIN;

ALTER TABLE stock_movements ADD COLUMN is_purchase INTEGER NOT NULL DEFAULT 1;

INSERT INTO _migrations (version, name, applied_at)
SELECT 30, '030_stock_movements_is_purchase', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 30);

COMMIT;
