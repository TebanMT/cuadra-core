-- Migration 013 — desglose por concepto en cada Payment. Espeja
-- sqlite/010.
--
-- JSONB para que el dispatcher y otros consumidores futuros puedan
-- iterar las líneas sin re-parsear. Nullable: pagos pre-migración
-- caen al render legacy de una sola línea.

BEGIN;

ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS breakdown JSONB;

INSERT INTO _migrations (version, name) VALUES (13, '013_payment_breakdown')
ON CONFLICT (version) DO NOTHING;

COMMIT;
