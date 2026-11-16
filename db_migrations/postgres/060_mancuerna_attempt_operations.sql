-- Durable reservation of one provider operation per attempt. This records an
-- ambiguous call before leaving the transaction; no OTP or plaintext secret.
BEGIN;
ALTER TABLE mancuerna.auth_challenges
 ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 ADD COLUMN provider_operation_id uuid,
 ADD COLUMN provider_operation_hash bytea,
 ADD COLUMN provider_operation_kind text,
 ADD COLUMN provider_operation_started_at timestamptz,
 ADD CONSTRAINT auth_provider_operation_binding CHECK (
  (provider_operation_id IS NULL AND provider_operation_hash IS NULL AND provider_operation_kind IS NULL AND provider_operation_started_at IS NULL)
  OR
  (provider_operation_id IS NOT NULL AND provider_operation_hash IS NOT NULL AND octet_length(provider_operation_hash)=32 AND provider_operation_kind IS NOT NULL AND provider_operation_kind IN ('send','check') AND provider_operation_started_at IS NOT NULL)
 );
-- Evaluate expiry at the authorization check, including after waiting on the
-- account mutex. Soft-deleted identity rows must never validate an access JWT.
CREATE OR REPLACE FUNCTION mancuerna_private.session_active(sid uuid,did uuid) RETURNS boolean LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM mancuerna.sessions s JOIN mancuerna.devices d ON (d.account_id,d.id)=(s.account_id,s.device_id)
 JOIN mancuerna.accounts a ON a.id=s.account_id
 WHERE s.account_id=mancuerna_private.account_id() AND s.id=sid AND s.device_id=did
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND d.revoked_at IS NULL AND d.deleted_at IS NULL AND a.status='active' AND a.deleted_at IS NULL)
$$;
INSERT INTO mancuerna.schema_versions(version) VALUES(2);
INSERT INTO public._migrations(version,name) VALUES(60,'060_mancuerna_attempt_operations.sql');
COMMIT;
