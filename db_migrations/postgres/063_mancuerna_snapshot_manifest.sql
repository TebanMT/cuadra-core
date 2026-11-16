-- Immutable bootstrap manifests preserve the exact original cursor and policy
-- on retries. Legacy rows remain intact but cannot be served without a manifest.
BEGIN;
ALTER TABLE mancuerna.sync_snapshots
 ADD COLUMN manifest jsonb,
 ADD CONSTRAINT snapshot_manifest_object CHECK(manifest IS NULL OR jsonb_typeof(manifest)='object'),
 ADD CONSTRAINT snapshot_page_count_limit CHECK(page_count<=100000);
ALTER TABLE mancuerna.sync_snapshot_pages
 DROP CONSTRAINT sync_snapshot_pages_account_id_snapshot_id_fkey,
 ADD CONSTRAINT sync_snapshot_pages_account_id_snapshot_id_fkey
 FOREIGN KEY(account_id,snapshot_id) REFERENCES mancuerna.sync_snapshots(account_id,id) DEFERRABLE INITIALLY DEFERRED,
 ADD CONSTRAINT snapshot_page_number_limit CHECK(page_number<100000),
 ADD CONSTRAINT snapshot_page_records_limit CHECK(jsonb_typeof(data)='array' AND jsonb_array_length(data)<=1000);
INSERT INTO mancuerna.schema_versions(version) VALUES(5);
INSERT INTO public._migrations(version,name) VALUES(63,'063_mancuerna_snapshot_manifest.sql');
COMMIT;
