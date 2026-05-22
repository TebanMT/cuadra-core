-- Sidecar mirror of postgres/023_gyms_stripe_customer.sql.
-- El sidecar nunca escribe customer_id directamente; recibe el valor por
-- sync pull cuando el cloud actualiza el field después de un webhook
-- Stripe. La columna existe para que el projector local pueda materializar
-- el field y la página Suscripción del desktop sepa si hay portal disponible
-- (aunque el botón siempre abre el cloud — el portal vive en browser).

BEGIN;

ALTER TABLE gyms ADD COLUMN stripe_customer_id TEXT;

INSERT INTO _migrations (version, name, applied_at)
SELECT 18, '018_gyms_stripe_customer', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 18);

COMMIT;
