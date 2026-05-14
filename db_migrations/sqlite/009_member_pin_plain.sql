-- Migration 009 — guarda el PIN del socio en texto plano además del
-- bcrypt hash. Espeja postgres/012.
--
-- ¿Por qué storage plano? El PIN del socio es un código de 4 dígitos
-- de conveniencia (no un secreto): el operador lo escribe en la
-- credencial física, se lo lee al socio cuando lo olvida, y se ve en el
-- perfil del socio. Sin texto plano el operador no puede recuperarlo
-- (bcrypt es one-way). El check-in sigue usando `pin_hash` por la misma
-- razón que `password_hash`: defensa en profundidad ante un DB leak.

BEGIN;

ALTER TABLE members ADD COLUMN pin_plain TEXT;

INSERT INTO _migrations (version, name, applied_at)
SELECT 9, '009_member_pin_plain', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 9);

COMMIT;
