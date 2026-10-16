BEGIN;
ALTER TABLE gyms ADD COLUMN all_features_access BOOLEAN NOT NULL DEFAULT FALSE;
INSERT INTO _migrations(version,name,applied_at) VALUES(52,'052_launch_access',NOW());
COMMIT;
