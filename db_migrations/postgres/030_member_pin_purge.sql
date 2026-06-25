-- Migration 030 — ADR-010: purga total del "PIN de socio".
--
-- El número de socio (migración 029) ya es el identificador canónico. Esta
-- migración elimina los últimos rastros del antiguo PIN del SOCIO:
--   1. Renombra el método de check-in "pin" → "number" (enum + datos).
--   2. Borra las columnas deprecadas members.pin_hash / pin_plain /
--      pin_assigned_at.
-- NOTA: el PIN de LOGIN del operador (users.pin_hash) es otro concepto y NO
-- se toca. (La feature no está en prod, así que no se conserva red de
-- rollback de los datos del PIN viejo.)

BEGIN;

-- 1) checkins.method "pin" → "number".
ALTER TABLE checkins DROP CONSTRAINT IF EXISTS chk_checkins_method;
UPDATE checkins SET method = 'number' WHERE method = 'pin';
ALTER TABLE checkins ADD CONSTRAINT chk_checkins_method
    CHECK (method IN ('fingerprint','manual','number'));

-- 2) Borrar columnas PIN deprecadas del socio.
ALTER TABLE members DROP COLUMN IF EXISTS pin_hash;
ALTER TABLE members DROP COLUMN IF EXISTS pin_plain;
ALTER TABLE members DROP COLUMN IF EXISTS pin_assigned_at;

INSERT INTO _migrations (version, name) VALUES (30, '030_member_pin_purge')
ON CONFLICT (version) DO NOTHING;

COMMIT;
