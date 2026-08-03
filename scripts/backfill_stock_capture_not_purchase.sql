-- backfill_stock_capture_not_purchase.sql — reclasifica las capturas de
-- catálogo que quedaron contadas como COMPRA (bug ago-2026).
--
-- CONTEXTO: hasta ago-2026 el alta de producto sembraba el stock inicial
-- como movement_type='restock' con costo, indistinguible de una compra →
-- la carga de catálogo del gym inflaba "egresos por mercancía" y el mes
-- arrancaba con utilidad negativa. El fix agrega stock_movements.is_purchase
-- (migración postgres/033 + sqlite/030); este script corrige las filas que
-- nacieron ANTES del fix.
--
-- CRITERIO: reason = 'Stock inicial' — es el default que CreateProduct
-- escribe en el seed del alta (create_product.go). Los restocks de
-- proveedor van por /adjust-stock con reason libre ("Proveedor Coca", …),
-- así que el match es preciso. Si el dueño capturó altas con reason
-- manual, agregarlas por id en el PASO 3.
--
-- ⚠️ CÓMO USARLO (contra el Postgres del CLOUD, no contra el sidecar):
--   1. Deploy del core con la migración 033 aplicada (cloud) y release del
--      desktop con la 030 (sidecar) — la columna debe existir en ambos.
--   2. PASO 1 read-only para ver qué filas se van a tocar.
--   3. PASO 2 dentro de una transacción. Actualiza la tabla canónica Y el
--      journal de sync (sync_entities) con version+1 — los sidecars hacen
--      pull de ahí y aplican por LWW, así la corrección baja SOLA a todos
--      los dispositivos del gym. No hace falta tocar el SQLite a mano.
--
-- El costo NO se toca: sigue alimentando el promedio ponderado del margen.
-- Idempotente: la segunda corrida no encuentra filas (is_purchase ya FALSE).

-- ⚠️ OJO — ALTAS QUE SÍ FUERON COMPRA: si algún producto se dio de alta
-- porque ACABABA de llegar (el dinero sí salió), ese movimiento debe
-- QUEDARSE como compra. Identifícalos en el PASO 1 (por nombre de
-- producto) y pon sus ids en la lista de exclusión del PASO 2a. Caso real
-- del gym piloto (2-ago-2026): la mayoría era catálogo preexistente, pero
-- un par sí fue mercancía recién llegada.

-- ────────────────────────────────────────────────────────────────────────
-- PASO 1 — VERIFICACIÓN (read-only)
-- ────────────────────────────────────────────────────────────────────────
SELECT sm.id, sm.gym_id, g.name AS gym, p.name AS producto,
       sm.delta, sm.cost, sm.cost * sm.delta AS egreso_fantasma,
       sm.reason, sm.created_at
  FROM stock_movements sm
  JOIN products p ON p.id = sm.product_id
  LEFT JOIN gyms g ON g.id = sm.gym_id
 WHERE sm.deleted_at IS NULL
   AND sm.movement_type = 'restock'
   AND sm.cost IS NOT NULL
   AND sm.is_purchase = TRUE
   AND sm.reason = 'Stock inicial'
 ORDER BY sm.created_at DESC;

-- Total del egreso fantasma por gym (lo que va a desaparecer de "egresos"):
SELECT sm.gym_id, g.name AS gym, SUM(sm.cost * sm.delta) AS egreso_fantasma
  FROM stock_movements sm
  LEFT JOIN gyms g ON g.id = sm.gym_id
 WHERE sm.deleted_at IS NULL
   AND sm.movement_type = 'restock'
   AND sm.cost IS NOT NULL
   AND sm.is_purchase = TRUE
   AND sm.reason = 'Stock inicial'
 GROUP BY sm.gym_id, g.name;

-- ────────────────────────────────────────────────────────────────────────
-- PASO 2 — BACKFILL (idempotente; correr dentro de una transacción)
-- Descomentar para ejecutar.
-- ────────────────────────────────────────────────────────────────────────
-- BEGIN;
--
-- -- 2a. Tabla canónica. version+1 y updated_at=NOW() para ganar el LWW
-- --     contra las copias viejas de los sidecars. La lista NOT IN excluye
-- --     las altas que SÍ fueron compra (ids del PASO 1) — quítala si no
-- --     aplica, o rellénala con los ids reales.
-- UPDATE stock_movements
--    SET is_purchase = FALSE, version = version + 1, updated_at = NOW()
--  WHERE deleted_at IS NULL
--    AND movement_type = 'restock'
--    AND cost IS NOT NULL
--    AND is_purchase = TRUE
--    AND reason = 'Stock inicial'
--    AND id NOT IN (
--      -- ids de altas que SÍ fueron compra (producto recién llegado):
--      -- 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx',
--      -- 'yyyyyyyy-yyyy-yyyy-yyyy-yyyyyyyyyyyy'
--      '00000000-0000-0000-0000-000000000000'  -- placeholder para que el NOT IN nunca quede vacío
--    );
--
-- -- 2b. Journal de sync — los sidecars hacen pull de acá. Alineamos
-- --     payload/version, bumpeamos payload.updated_at (UnixMilli — lo que
-- --     compara el LWW del sidecar) y server_updated_at (cursor del pull).
-- --     Payload keys según enqueueStockMovement (stock_movement_sqlite.go).
-- UPDATE sync_entities se
--    SET payload = se.payload || jsonb_build_object(
--                    'is_purchase', FALSE,
--                    'version',     sm.version,
--                    'updated_at',  (EXTRACT(EPOCH FROM NOW()) * 1000)::bigint),
--        version = sm.version,
--        server_updated_at = NOW()
--   FROM stock_movements sm
--  WHERE se.entity_type = 'stock_movements'
--    AND se.entity_id   = sm.id
--    AND sm.is_purchase = FALSE
--    AND sm.reason = 'Stock inicial'
--    AND COALESCE((se.payload ->> 'is_purchase')::boolean, TRUE) = TRUE;
--
-- COMMIT;

-- ────────────────────────────────────────────────────────────────────────
-- PASO 3 (opcional) — altas capturadas con reason manual: mismo par de
-- UPDATEs de arriba cambiando el filtro `reason = 'Stock inicial'` por
-- `id IN ('…', '…')` con los ids confirmados en el PASO 1.
-- ────────────────────────────────────────────────────────────────────────

-- ────────────────────────────────────────────────────────────────────────
-- PASO 4 (correctivo) — si el backfill YA corrió y reclasificó de más:
-- regresar ids específicos a compra. Mismo mecanismo (tabla + journal).
-- Descomentar, rellenar ids y correr en una transacción.
-- ────────────────────────────────────────────────────────────────────────
-- BEGIN;
--
-- UPDATE stock_movements
--    SET is_purchase = TRUE, version = version + 1, updated_at = NOW()
--  WHERE id IN ('xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx');
--
-- UPDATE sync_entities se
--    SET payload = se.payload || jsonb_build_object(
--                    'is_purchase', TRUE,
--                    'version',     sm.version,
--                    'updated_at',  (EXTRACT(EPOCH FROM NOW()) * 1000)::bigint),
--        version = sm.version,
--        server_updated_at = NOW()
--   FROM stock_movements sm
--  WHERE se.entity_type = 'stock_movements'
--    AND se.entity_id   = sm.id
--    AND sm.id IN ('xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx');
--
-- COMMIT;
