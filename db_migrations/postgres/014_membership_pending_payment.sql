-- Migration 014 — soporte para "membresía inscrita sin pago todavía".
-- Espeja sqlite/011.
--
-- Cambios:
--   * status CHECK suma 'pending_payment'.
--   * expiry_date pasa a NULL-able.
--   * dates CHECK permite expiry_date IS NULL.
--   * uq_memberships_member_active extiende su WHERE a 'pending_payment'.

BEGIN;

-- Status: nueva opción 'pending_payment'.
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS chk_memberships_status;
ALTER TABLE memberships ADD CONSTRAINT chk_memberships_status
    CHECK (status IN ('active','expired','replaced','cancelled','pending_payment'));

-- expiry_date pasa a nullable.
ALTER TABLE memberships ALTER COLUMN expiry_date DROP NOT NULL;

-- Dates: permitir NULL.
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS chk_memberships_dates;
ALTER TABLE memberships ADD CONSTRAINT chk_memberships_dates
    CHECK (expiry_date IS NULL OR expiry_date >= start_date);

-- Unique partial index: ampliar para que "pending" cuente como la
-- misma slot que "active" — un socio no puede tener dos planes a la
-- vez, esté pagado o no.
DROP INDEX IF EXISTS uq_memberships_member_active;
CREATE UNIQUE INDEX uq_memberships_member_active
    ON memberships(member_id)
    WHERE status IN ('active','pending_payment') AND deleted_at IS NULL;

-- chk_checkins_result: agregar 'denied_unpaid_enrollment'.
ALTER TABLE checkins DROP CONSTRAINT IF EXISTS chk_checkins_result;
ALTER TABLE checkins ADD CONSTRAINT chk_checkins_result
    CHECK (result IN (
        'allowed_active','allowed_expiring_soon','allowed_override',
        'denied_expired','denied_inactive','denied_no_membership','denied_unpaid_enrollment'
    ));

INSERT INTO _migrations (version, name) VALUES (14, '014_membership_pending_payment')
ON CONFLICT (version) DO NOTHING;

COMMIT;
