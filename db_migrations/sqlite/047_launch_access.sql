BEGIN;
ALTER TABLE gyms ADD COLUMN all_features_access INTEGER NOT NULL DEFAULT 0 CHECK (all_features_access IN (0, 1));
INSERT INTO _migrations(version,name,applied_at) VALUES(47,'047_launch_access',CAST(strftime('%s','now') AS INTEGER)*1000);
COMMIT;
