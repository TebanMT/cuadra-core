-- A refresh may finish while its response is lost. The previous logout ticket
-- must keep its sole permission to revoke that same session offline/later.
-- Store hashes only, beside the consumed refresh generation; no reusable token.
BEGIN;
ALTER TABLE mancuerna.session_rotations
 ADD COLUMN old_revocation_ticket_hash bytea UNIQUE
 CHECK(old_revocation_ticket_hash IS NULL OR octet_length(old_revocation_ticket_hash)=32);
-- NULL supports rows from earlier revisions. Every new rotation writes the
-- previous ticket hash; retaining rotations retains this limited capability.
CREATE OR REPLACE FUNCTION mancuerna_private.resolve_identity(kind text, lookup text) RETURNS uuid LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE answer uuid;
BEGIN
 CASE kind
 WHEN 'phone' THEN SELECT id INTO answer FROM mancuerna.accounts WHERE phone_e164=lookup;
 WHEN 'recovery' THEN SELECT account_id INTO answer FROM mancuerna.recovery_keys WHERE id=lookup::uuid;
 WHEN 'refresh' THEN
  SELECT account_id INTO answer FROM mancuerna.sessions WHERE refresh_hash=decode(lookup,'hex');
  IF answer IS NULL THEN SELECT account_id INTO answer FROM mancuerna.session_rotations WHERE old_refresh_hash=decode(lookup,'hex'); END IF;
 WHEN 'logout' THEN
  SELECT account_id INTO answer FROM mancuerna.sessions WHERE revocation_ticket_hash=decode(lookup,'hex');
  IF answer IS NULL THEN SELECT account_id INTO answer FROM mancuerna.session_rotations WHERE old_revocation_ticket_hash=decode(lookup,'hex'); END IF;
 WHEN 'deletion_result' THEN SELECT account_id INTO answer FROM mancuerna.auth_receipts WHERE result_secret_hash=decode(lookup,'hex') AND purpose='delete_account';
 ELSE RAISE EXCEPTION 'unsupported identity lookup';
 END CASE;
 RETURN answer;
END $$;
-- Reassert the narrow function's ownership and grants after replacement.
ALTER FUNCTION mancuerna_private.resolve_identity(text,text) OWNER TO mancuerna_identity;
REVOKE ALL ON FUNCTION mancuerna_private.resolve_identity(text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mancuerna_private.resolve_identity(text,text) TO mancuerna_auth;
INSERT INTO mancuerna.schema_versions(version) VALUES(3);
INSERT INTO public._migrations(version,name) VALUES(61,'061_mancuerna_refresh_logout_tickets.sql');
COMMIT;
