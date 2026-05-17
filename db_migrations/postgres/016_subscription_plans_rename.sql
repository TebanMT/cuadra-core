-- Migration 016 — rename subscription_plan SKUs.
-- Espeja sqlite/013_subscription_plans_rename.sql.
--
-- El nombre histórico `pro_monthly` significaba "tier Standard mensual" y
-- `pro_annual` significaba "tier Plus mensual" (no anual; el sufijo era
-- engañoso — el precio era $1,599 MXN/MES). Esta migración:
--
--   1) Reescribe el CHECK aceptando los cuatro SKUs nombre-correcto:
--      standard_monthly, standard_annual, plus_monthly, plus_annual.
--   2) Renombra las filas existentes:
--      pro_monthly  → standard_monthly
--      pro_annual   → plus_monthly       (no _annual: era $/mes)
--
-- Los SKUs *_annual quedan aceptados por el CHECK aunque todavía no haya
-- pricing/checkout cableado — los gateways los van a mappear conforme
-- entren los price_id de Stripe / amounts de MP.

BEGIN;

ALTER TABLE gyms DROP CONSTRAINT IF EXISTS chk_gyms_subscription_plan;

UPDATE gyms SET subscription_plan = 'standard_monthly' WHERE subscription_plan = 'pro_monthly';
UPDATE gyms SET subscription_plan = 'plus_monthly'     WHERE subscription_plan = 'pro_annual';

ALTER TABLE gyms ADD CONSTRAINT chk_gyms_subscription_plan
    CHECK (subscription_plan IN (
        'trial',
        'standard_monthly', 'standard_annual',
        'plus_monthly',     'plus_annual'
    ));

INSERT INTO _migrations (version, name) VALUES (16, '016_subscription_plans_rename')
ON CONFLICT (version) DO NOTHING;

COMMIT;
