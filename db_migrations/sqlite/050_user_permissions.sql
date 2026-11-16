-- Per-operator permissions (UC-007). `permissions` is a JSON array with what
-- the owner allowed a reception employee to do beyond the basic work. The
-- owner always has every permission. Operators that already exist keep what
-- they could do before (every permission); a new operator starts empty.
BEGIN;
ALTER TABLE users ADD COLUMN permissions TEXT NOT NULL DEFAULT '[]';
UPDATE users
   SET permissions = '["discounts","credit","cash_out","view_totals","checkin_override","products","inventory","member_status"]'
 WHERE role = 'operator';
INSERT INTO _migrations(version,name,applied_at) VALUES(50,'050_user_permissions',CAST(strftime('%s','now') AS INTEGER)*1000);
COMMIT;
