-- Migration 022 — verificación de correo (UC-013).
--
-- Agrega el estado "correo verificado" al dueño:
--
-- 1. users.email_verified_at: NULL = no verificado. El dashboard muestra
--    una banda "verifica tu correo" hasta que se rellene. NO bloqueante:
--    el trial sigue funcionando, pero el dueño que pierde su contraseña
--    no puede recuperar la cuenta si el correo era un typo, así que la
--    banda empuja a arreglarlo.
-- 2. email_verification_tokens: misma forma que password_reset_tokens
--    (token_hash SHA-256, used_at single-use, expires_at TTL). El use
--    case lo dispara al signup y vía POST /auth/resend-verification.

BEGIN;

ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS email_verification_tokens (
    id          UUID PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    token_hash  BYTEA NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_email_verification_tokens_user
    ON email_verification_tokens(user_id, expires_at);

INSERT INTO _migrations (version, name) VALUES (22, '022_email_verification')
ON CONFLICT (version) DO NOTHING;

COMMIT;
