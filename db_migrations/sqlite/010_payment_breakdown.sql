-- Migration 010 — desglose por concepto en cada Payment. Espeja
-- postgres/013.
--
-- Hoy guardamos sólo el total (Payment.amount). El recibo termina con
-- una sola línea "Mensualidad / membresía: $XXX" aunque el cobro en
-- realidad incluyera membresía + inscripción + mantenimiento, o varios
-- productos. La columna `breakdown` guarda un JSON con las líneas
-- individuales (label, amount) que el renderizador del PDF imprime tal
-- cual. Nullable: los pagos viejos siguen renderizando con la línea
-- única tradicional.

BEGIN;

ALTER TABLE payments ADD COLUMN breakdown TEXT;

INSERT INTO _migrations (version, name, applied_at)
SELECT 10, '010_payment_breakdown', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 10);

COMMIT;
