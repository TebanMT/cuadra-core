-- 033 — stock_movements.is_purchase
--
-- Distingue una COMPRA real (salió dinero) de la captura de inventario que
-- ya existía físicamente (alta de catálogo / hallazgo en conteo). El alta de
-- producto sembraba el stock inicial como movement_type='restock' con costo,
-- indistinguible de una compra → la carga de catálogo inflaba "egresos por
-- mercancía" y el mes arrancaba con utilidad negativa.
--
-- Default TRUE: todo lo histórico conserva su semántica (compra). Los
-- egresos filtran is_purchase; el costo promedio / margen NO filtra — las
-- capturas iniciales son la base del COGS.

BEGIN;

ALTER TABLE stock_movements
    ADD COLUMN IF NOT EXISTS is_purchase BOOLEAN NOT NULL DEFAULT TRUE;

INSERT INTO _migrations (version, name) VALUES (33, '033_stock_movements_is_purchase')
ON CONFLICT (version) DO NOTHING;

COMMIT;
