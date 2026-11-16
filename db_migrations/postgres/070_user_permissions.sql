-- Per-operator permissions (UC-007). `permissions` lists what the owner
-- allowed a reception employee to do beyond the basic work: discounts,
-- credit, cash out, money totals, check-in override, products, inventory,
-- member status.
-- The owner always has every permission, so the column is ignored for them.
--
-- Operators that already exist keep what they could do before this
-- migration (every permission); a new operator starts with an empty list.
-- The backfill only runs when the column is created, so replaying this file
-- never re-grants what an owner took away.
-- The column is pushed by the desktop (sidecar-owned, like kiosk_settings)
-- and pulls read it from the live row, so an older desktop that syncs
-- without the key never wipes it.
BEGIN;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns
     WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'permissions'
  ) THEN
    ALTER TABLE public.users
      ADD COLUMN permissions JSONB NOT NULL DEFAULT '[]'::jsonb;
    UPDATE public.users
       SET permissions = '["discounts","credit","cash_out","view_totals","checkin_override","products","inventory","member_status"]'::jsonb
     WHERE role = 'operator';
  END IF;
END $$;

ALTER TABLE public.users DROP CONSTRAINT IF EXISTS chk_users_permissions_array;
ALTER TABLE public.users
  ADD CONSTRAINT chk_users_permissions_array CHECK (jsonb_typeof(permissions) = 'array');

INSERT INTO _migrations(version, name)
VALUES (70, '070_user_permissions')
ON CONFLICT (version) DO NOTHING;

COMMIT;
