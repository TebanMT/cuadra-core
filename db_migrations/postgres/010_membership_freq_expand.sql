-- Migration 010 — expandir maintenance_frequency con bimensual, trimestral
-- y semestral (fase 1, opción B). El subset original ('monthly','annual')
-- dejaba afuera a ~5% de gimnasios mexicanos que cobran cada 2/3/6 meses.
-- Mantenemos enum estricto (no introducimos `interval_months` libre) hasta
-- que aparezca el primer cliente que pida intervalos exóticos —
-- entonces refactor a free-form.

BEGIN;

ALTER TABLE membership_types DROP CONSTRAINT IF EXISTS chk_membership_types_frequency;
ALTER TABLE membership_types ADD CONSTRAINT chk_membership_types_frequency CHECK (
    (maintenance_fee = 0 AND maintenance_frequency IS NULL) OR
    (maintenance_fee > 0 AND maintenance_frequency IN
        ('monthly','bimonthly','quarterly','semiannual','annual'))
);

INSERT INTO _migrations (version, name) VALUES (10, '010_membership_freq_expand')
ON CONFLICT (version) DO NOTHING;

COMMIT;
