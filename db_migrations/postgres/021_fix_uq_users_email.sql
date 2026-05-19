-- Migration 021 — corrige uq_users_email.
--
-- La migración 019 reconstruyó uq_users_email para que ignore correos
-- vacíos (los operadores PIN-first se guardan con email = ''). En la base
-- viva el índice quedó con la definición vieja de 001 — sin el filtro
-- `email <> ''` — pese a que 019 figura aplicada en _migrations: drift de
-- esquema (restore parcial / cambio manual sobre la base ya migrada). El
-- runner no puede auto-corregir esto porque 019 ya está registrada y se
-- salta; por eso la corrección va en una migración nueva.
--
-- Sin el filtro, dos operadores sin correo colisionan en LOWER('') y el
-- segundo no se puede crear (ver src/modules/users/domain/user/user.go
-- NewOperator). Recrear el índice es idempotente: la versión nueva es más
-- permisiva que la vieja, así que nunca falla por datos existentes.

BEGIN;

DROP INDEX IF EXISTS uq_users_email;
CREATE UNIQUE INDEX uq_users_email
    ON users(LOWER(email))
    WHERE email <> '' AND deleted_at IS NULL;

INSERT INTO _migrations (version, name) VALUES (21, '021_fix_uq_users_email')
ON CONFLICT (version) DO NOTHING;

COMMIT;
