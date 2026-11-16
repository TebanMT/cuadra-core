-- Keep the client's original capture text independently of server commit time.
-- Earlier receipts cannot recover this value from their request hash. Preserve
-- their NULL honestly; enforce presence on every new row without backfilling.
BEGIN;
ALTER TABLE mancuerna.operation_receipts
 ADD COLUMN captured_at text,
 ADD CONSTRAINT command_capture_format CHECK(captured_at IS NULL OR
   (captured_at ~ '^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{3})?Z$'
    AND isfinite(captured_at::timestamptz))),
 ADD CONSTRAINT command_capture_required CHECK(captured_at IS NOT NULL) NOT VALID;
INSERT INTO mancuerna.schema_versions(version) VALUES(6);
INSERT INTO public._migrations(version,name) VALUES(64,'064_mancuerna_command_capture.sql');
COMMIT;
