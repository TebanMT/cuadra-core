-- Migration 027 — duración por meses naturales (UC-011 fix).
--
-- Bug: una membresía "mensual" se cobraba como 30 días naturales —
-- vendiendo el 25-may vencía el 24-jun en lugar de 25-jun. Injusto
-- para el socio porque cada mes "perdía" 0-3 días contra el calendario.
-- Sólo las personalizadas (1 día / 1 semana / 15 días / N días libres)
-- usan días; las mensuales/bimestrales/trimestrales/semestrales/anuales
-- son periodos de calendario.
--
-- Cambio: agregamos `duration_months` nullable a membership_types y
-- snapshot del mismo a memberships. NULL = sigue usando duration_days
-- (legacy + personalizadas + paso/semana/quincenal). NOT NULL = el
-- cálculo de expiry es start + N meses naturales (handling de
-- overflow en el dominio: 31-ene + 1 mes = 28/29-feb, no 3-mar).
--
-- Backfill: convertimos los presets ambiguos {30, 60, 90, 180, 365}
-- a sus equivalentes en meses ({1, 2, 3, 6, 12}). Las membresías ACTIVAS
-- mantienen su snapshot intacto (su expiry ya fue calculado y no se
-- toca por respeto al cobro). Las renovaciones futuras pasan por
-- Membership.Renew() que ya leerá el nuevo `duration_months` del
-- membership_type actual.

BEGIN;

ALTER TABLE membership_types
    ADD COLUMN IF NOT EXISTS duration_months INTEGER;

ALTER TABLE membership_types DROP CONSTRAINT IF EXISTS chk_membership_types_duration_months;
ALTER TABLE membership_types ADD CONSTRAINT chk_membership_types_duration_months
    CHECK (duration_months IS NULL OR duration_months BETWEEN 1 AND 60);

ALTER TABLE memberships
    ADD COLUMN IF NOT EXISTS duration_months_snapshot INTEGER;

ALTER TABLE memberships DROP CONSTRAINT IF EXISTS chk_memberships_duration_months;
ALTER TABLE memberships ADD CONSTRAINT chk_memberships_duration_months
    CHECK (duration_months_snapshot IS NULL OR duration_months_snapshot BETWEEN 1 AND 60);

-- Backfill: presets ambiguos sólo (NO tocamos 1, 7, 15, ni cualquier
-- otro valor — los días son explícitos para esos casos).
UPDATE membership_types SET duration_months = 1
    WHERE duration_months IS NULL AND duration_days = 30;
UPDATE membership_types SET duration_months = 2
    WHERE duration_months IS NULL AND duration_days = 60;
UPDATE membership_types SET duration_months = 3
    WHERE duration_months IS NULL AND duration_days = 90;
UPDATE membership_types SET duration_months = 6
    WHERE duration_months IS NULL AND duration_days = 180;
UPDATE membership_types SET duration_months = 12
    WHERE duration_months IS NULL AND duration_days = 365;

-- Memberships activas/expired/replaced: NO backfilleamos el snapshot.
-- Los expiry actuales ya fueron pagados con esa interpretación y modificar
-- snapshots violaría la inmutabilidad histórica del row (DA-11.1). Sólo
-- las renovaciones siguientes capturarán el nuevo snapshot correcto.

INSERT INTO _migrations (version, name) VALUES (27, '027_membership_duration_months')
ON CONFLICT (version) DO NOTHING;

COMMIT;
