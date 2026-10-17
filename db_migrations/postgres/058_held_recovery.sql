-- Recovery is cloud authoritative. These fields never travel from a sidecar.
BEGIN;

ALTER TABLE notification_queue
 ADD COLUMN recovery_version integer NOT NULL DEFAULT 0,
 ADD COLUMN recovery_action text NOT NULL DEFAULT '',
 ADD COLUMN recovery_at timestamptz,
 ADD COLUMN recovery_attempted_at timestamptz;
CREATE INDEX notification_held_cleanup ON notification_queue(updated_at)
 WHERE status='held' AND deleted_at IS NULL;

INSERT INTO _migrations(version,name) VALUES(58,'058_held_recovery')
ON CONFLICT(version) DO NOTHING;
COMMIT;
