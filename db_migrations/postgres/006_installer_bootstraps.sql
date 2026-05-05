-- Migration 006 — installer_bootstraps (cloud-only, ADR-008 §4.5 extension)
--
-- Single-use codes the dashboard hands the operator after web signup so the
-- desktop's first launch can swap the code for a full session (operator
-- JWT + sk_live_* sidecar credential) without asking for email + password
-- again.
--
-- Lifecycle:
--   1. Dashboard POST /api/v1/auth/installer-token (auth required) →
--      mints plaintext token, stores SHA-256.
--   2. Desktop's first launch reads the token from the installer payload
--      (URL query, dropped file, or pasted by the user) and POSTs it to
--      /api/v1/auth/redeem-installer (no auth) along with X-Cuadra-Client-ID.
--   3. Cloud verifies hash + not redeemed + not expired, marks redeemed_at,
--      mints fresh JWTs + sidecar_token, returns the full login payload.
--
-- A token is good for 24 hours. After redemption it cannot be reused.

BEGIN;

CREATE TABLE IF NOT EXISTS installer_bootstraps (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash          BYTEA NOT NULL,
    gym_id              UUID NOT NULL REFERENCES gyms(id) ON DELETE CASCADE,
    created_by_user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at          TIMESTAMPTZ NOT NULL,
    redeemed_at         TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS installer_bootstraps_token_hash
    ON installer_bootstraps (token_hash);

-- Active-tokens lookup index for the redeem path: WHERE token_hash = ?
-- AND redeemed_at IS NULL AND expires_at > NOW().
CREATE INDEX IF NOT EXISTS installer_bootstraps_active
    ON installer_bootstraps (token_hash) WHERE redeemed_at IS NULL;

INSERT INTO _migrations (version, name) VALUES (6, '006_installer_bootstraps')
ON CONFLICT (version) DO NOTHING;

COMMIT;
