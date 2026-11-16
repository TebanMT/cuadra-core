-- A proof belongs to one exact account/session/device and is consumed by the
-- protected effect in the same transaction. Separate foreign keys alone do
-- not establish that the device is the one that owns the session.
BEGIN;
ALTER TABLE mancuerna.sessions
 ADD CONSTRAINT sessions_account_session_device UNIQUE(account_id,id,device_id);
ALTER TABLE mancuerna.device_proofs
 ADD CONSTRAINT device_proofs_exact_session_device
 FOREIGN KEY(account_id,session_id,device_id) REFERENCES mancuerna.sessions(account_id,id,device_id),
 ADD CONSTRAINT device_proofs_verified_token
 CHECK((token_hash IS NULL)=(verified_at IS NULL)),
 ADD CONSTRAINT device_proofs_token_length
 CHECK(token_hash IS NULL OR octet_length(token_hash)=32),
 ADD CONSTRAINT device_proofs_consumed_operation
 CHECK((consumed_at IS NULL)=(operation_id IS NULL)),
 ADD CONSTRAINT device_proofs_verified_window
 CHECK(verified_at IS NULL OR (verified_at>=expires_at-interval '5 minutes' AND verified_at<expires_at)),
 ADD CONSTRAINT device_proofs_consumed_window
 CHECK(consumed_at IS NULL OR (verified_at IS NOT NULL AND consumed_at>=verified_at AND consumed_at<expires_at));
INSERT INTO mancuerna.schema_versions(version) VALUES(4);
INSERT INTO public._migrations(version,name) VALUES(62,'062_mancuerna_device_proof_binding.sql');
COMMIT;
