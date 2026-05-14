-- Migration 012 — guarda el PIN del socio en texto plano además del
-- bcrypt hash. Espeja sqlite/009.
--
-- El PIN del socio es un código de conveniencia (no un secreto). El
-- operador necesita poder leerlo desde el perfil para escribirlo en la
-- credencial física o cuando el socio lo olvida. bcrypt es one-way así
-- que no es recuperable sin esta columna. `pin_hash` sigue siendo la
-- fuente de verdad para el check-in (mismo razonamiento que
-- `password_hash` en `users`).

BEGIN;

ALTER TABLE members
    ADD COLUMN IF NOT EXISTS pin_plain TEXT;

INSERT INTO _migrations (version, name) VALUES (12, '012_member_pin_plain')
ON CONFLICT (version) DO NOTHING;

COMMIT;
