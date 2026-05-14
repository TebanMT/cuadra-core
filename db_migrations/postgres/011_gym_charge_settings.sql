-- Migration 011 — configuración de cobros a nivel gym (inscripción +
-- mantenimiento). Hoy estos flags vivían en localStorage del desktop, lo
-- que era frágil (clear cache pierde la config) e inconsistente entre
-- múltiples equipos del mismo gym.
--
-- Columna nueva en `gyms` en lugar de meter las claves dentro de
-- `kiosk_settings` para que el naming siga reflejando el dominio
-- (kiosk_settings es para el modo kiosko; estos son cobros). Sync de
-- JSONB ya existe a través del projector — sólo hay que registrarla.

BEGIN;

ALTER TABLE gyms
    ADD COLUMN IF NOT EXISTS charge_settings JSONB NOT NULL DEFAULT '{}'::jsonb;

INSERT INTO _migrations (version, name) VALUES (11, '011_gym_charge_settings')
ON CONFLICT (version) DO NOTHING;

COMMIT;
