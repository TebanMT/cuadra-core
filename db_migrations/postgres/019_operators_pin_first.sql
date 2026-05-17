-- Migration 019 — operators PIN-first.
--
-- 1. password_hash pasa a NULLABLE: los operadores nuevos no llevan
--    password — solo PIN (ver app/operators.go::CreateOperator). Los
--    operadores legacy con password siguen funcionando hasta que el
--    dueño los migre.
-- 2. Email opcional para operadores: el índice único existente
--    `uq_users_email` se reemplaza por uno que ignora valores vacíos
--    (varios operadores pueden no tener correo a la vez). Mantenemos
--    NOT NULL en la columna para no romper código que asume string;
--    "" se trata como "sin correo" en el dominio.
-- 3. Unicidad por gym del teléfono: el mismo número no puede registrarse
--    dos veces dentro de un gym. Sí puede existir en dos gyms distintos
--    (la misma persona trabajando en dos lugares).

BEGIN;

ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

DROP INDEX IF EXISTS uq_users_email;
CREATE UNIQUE INDEX uq_users_email
    ON users(LOWER(email))
    WHERE email <> '' AND deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_users_gym_phone
    ON users(gym_id, phone)
    WHERE phone IS NOT NULL AND deleted_at IS NULL;

INSERT INTO _migrations (version, name) VALUES (19, '019_operators_pin_first')
ON CONFLICT (version) DO NOTHING;

COMMIT;
