-- Migration 013 — rename subscription_plan SKUs en SQLite local.
-- Espeja postgres/016_subscription_plans_rename.sql.
--
-- Estrategia: en lugar de recrear la tabla `gyms` entera (que rompía
-- FK constraints desde users/members/payments/etc.), usamos el patrón
-- column-swap soportado desde SQLite 3.35+:
--
--   1) RENAME COLUMN subscription_plan → _old_subscription_plan
--      (SQLite ajusta automáticamente la CHECK del column-level para
--       referenciar el nombre nuevo, sigue aceptando pro_*).
--   2) ADD COLUMN subscription_plan con la CHECK nueva (los 5 SKUs).
--   3) UPDATE poblando subscription_plan con traducción
--      pro_monthly → standard_monthly, pro_annual → plus_monthly.
--   4) DROP COLUMN _old_subscription_plan — se va el CHECK viejo con él.
--
-- Cero recreate-table = cero FK issues. La columna se reemplaza in-place,
-- las referencias de otras tablas a gyms.id quedan intactas porque la tabla
-- nunca se renombra ni se cae.

BEGIN;

-- Step 1: el RENAME COLUMN auto-ajusta el CHECK column-level para que
-- referencie _old_subscription_plan. La columna sigue aceptando los
-- valores viejos durante la migración.
ALTER TABLE gyms RENAME COLUMN subscription_plan TO _old_subscription_plan;

-- Step 2: agregamos la columna nueva con el CHECK que admite los 4 SKUs
-- nuevos + trial. DEFAULT 'trial' es seguro porque vamos a sobreescribir
-- en el siguiente UPDATE.
ALTER TABLE gyms
    ADD COLUMN subscription_plan TEXT NOT NULL DEFAULT 'trial'
    CHECK (subscription_plan IN (
        'trial',
        'standard_monthly', 'standard_annual',
        'plus_monthly',     'plus_annual'
    ));

-- Step 3: traducción de los SKUs antiguos a los nuevos. `pro_monthly` era
-- el Standard mensual y `pro_annual` (mal nombrado) era el Plus mensual —
-- ambos quedan apuntando a los SKUs *_monthly correspondientes. Trial y
-- cualquier otro valor desconocido se preservan tal cual.
UPDATE gyms SET subscription_plan = CASE _old_subscription_plan
    WHEN 'pro_monthly' THEN 'standard_monthly'
    WHEN 'pro_annual'  THEN 'plus_monthly'
    ELSE _old_subscription_plan
END;

-- Step 4: la columna vieja se va, y con ella su CHECK constraint que
-- referenciaba pro_*. Quedamos sólo con el CHECK nuevo.
ALTER TABLE gyms DROP COLUMN _old_subscription_plan;

INSERT INTO _migrations (version, name, applied_at)
SELECT 13, '013_subscription_plans_rename', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 13);

COMMIT;
