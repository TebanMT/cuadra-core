-- Migration 023 — gyms.stripe_customer_id.
--
-- Captura el customer id que Stripe crea en el primer checkout (subscription
-- mode). Lo necesitamos para abrir el Billing Portal sin re-onboarding
-- (mostrar facturas, cambiar tarjeta, cancelar). Nullable: gyms en trial o
-- que sólo cobran por MP no tienen customer Stripe.
--
-- El webhook handler lo persiste cuando llega el primer evento que trae
-- `data.object.customer` (típicamente customer.subscription.created o
-- checkout.session.completed). RecordEvent es idempotente: re-asigna sólo
-- si el campo estaba vacío para no pisar manualmente cambios del founder.

BEGIN;

ALTER TABLE gyms ADD COLUMN IF NOT EXISTS stripe_customer_id TEXT;

INSERT INTO _migrations (version, name) VALUES (23, '023_gyms_stripe_customer')
ON CONFLICT (version) DO NOTHING;

COMMIT;
