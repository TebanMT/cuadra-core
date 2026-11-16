-- Mancuerna personal schema, adopted from approved S00 (15-Oct-2026).
-- Provision group roles first with scripts/db/mancuerna_roles.sql as cluster admin.
-- Additive only: no changes to public/Tinta, no personal tables in sidecar sync.
BEGIN;
DO $$ BEGIN
 IF (SELECT count(*) FROM pg_roles WHERE rolname IN ('mancuerna_data','mancuerna_auth','mancuerna_identity') AND NOT (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolcanlogin OR rolreplication)) <> 3 THEN
  RAISE EXCEPTION 'Provision restricted Mancuerna group roles before migration 059';
 END IF;
END $$;
CREATE SCHEMA mancuerna;
CREATE SCHEMA mancuerna_private;
REVOKE ALL ON SCHEMA mancuerna, mancuerna_private FROM PUBLIC;
CREATE TABLE mancuerna.schema_versions(version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO mancuerna.schema_versions(version) VALUES(1);
CREATE TABLE mancuerna.accounts(
 id uuid PRIMARY KEY, phone_e164 text UNIQUE CHECK(phone_e164 ~ '^\+[1-9][0-9]{7,14}$'),
 display_name text NOT NULL DEFAULT '' CHECK(length(display_name)<=80), weight_unit text NOT NULL DEFAULT 'kg' CHECK(weight_unit IN ('kg','lb')),
 status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','deletion_pending','deleted')),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0), security_version bigint NOT NULL DEFAULT 1 CHECK(security_version>0),
 phone_verified_at timestamptz NOT NULL, phone_changed_at timestamptz, deletion_requested_at timestamptz,
 deletion_policy_version text, purge_after timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
 CHECK(status='deleted' OR phone_e164 IS NOT NULL), CHECK(status='active' OR deletion_requested_at IS NOT NULL)
);
CREATE TABLE mancuerna.devices(
 id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES mancuerna.accounts(id),
 platform text NOT NULL CHECK(platform IN ('android','ios')), label text NOT NULL CHECK(length(label) BETWEEN 1 AND 80),
 public_key bytea NOT NULL CHECK(octet_length(public_key)=32),
 authorized_at timestamptz NOT NULL, authorized_by_method text NOT NULL CHECK(authorized_by_method IN ('signup','device','recovery')),
 revoked_at timestamptz, revocation_reason text, last_seen_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
 UNIQUE(account_id,id)
);
CREATE TABLE mancuerna.sessions(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, device_id uuid NOT NULL,
 refresh_hash bytea NOT NULL UNIQUE CHECK(octet_length(refresh_hash)=32), refresh_family_id uuid NOT NULL,
 generation bigint NOT NULL DEFAULT 1 CHECK(generation>0), expires_at timestamptz NOT NULL,
 revocation_ticket_hash bytea NOT NULL UNIQUE CHECK(octet_length(revocation_ticket_hash)=32),
 revoked_at timestamptz, last_used_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(account_id,id), FOREIGN KEY(account_id,device_id) REFERENCES mancuerna.devices(account_id,id)
);
CREATE TABLE mancuerna.session_rotations(
 account_id uuid NOT NULL, session_id uuid NOT NULL, generation bigint NOT NULL CHECK(generation>0),
 old_refresh_hash bytea NOT NULL UNIQUE CHECK(octet_length(old_refresh_hash)=32), operation_id uuid NOT NULL UNIQUE,
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32), response_ciphertext bytea, key_version text,
 response_expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(account_id,session_id,generation), FOREIGN KEY(account_id,session_id) REFERENCES mancuerna.sessions(account_id,id),
 CHECK((response_ciphertext IS NULL)=(key_version IS NULL))
);
CREATE TABLE mancuerna.auth_challenges(
 id uuid PRIMARY KEY, account_id uuid REFERENCES mancuerna.accounts(id), proposed_account_id uuid NOT NULL, candidate_device_id uuid NOT NULL,
 nonce bytea NOT NULL CHECK(octet_length(nonce)=32), candidate_public_key bytea NOT NULL CHECK(octet_length(candidate_public_key)=32), candidate_platform text NOT NULL CHECK(candidate_platform IN ('android','ios')), candidate_label text NOT NULL,
 purpose text NOT NULL CHECK(purpose IN ('access','recovery','phone_change')), destination_e164 text NOT NULL CHECK(destination_e164 ~ '^\+[1-9][0-9]{7,14}$'),
 channel text NOT NULL CHECK(channel IN ('whatsapp','sms')), provider_reference text,
 status text NOT NULL CHECK(status IN ('pending','verified','consumed','expired','cancelled','uncertain')),
 continuation_hash bytea NOT NULL CHECK(octet_length(continuation_hash)=32), request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0), expires_at timestamptz NOT NULL, verified_at timestamptz, consumed_at timestamptz,
 result_reference uuid, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(account_id,id)
);
CREATE TABLE mancuerna.device_authorizations(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, challenge_id uuid NOT NULL UNIQUE, approved_by_device_id uuid,
 comparison_code text NOT NULL CHECK(comparison_code ~ '^[0-9]{6}$'),
 status text NOT NULL CHECK(status IN ('pending','approved','denied','expired','cancelled')),
 expires_at timestamptz NOT NULL, decided_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(account_id,id), FOREIGN KEY(account_id,challenge_id) REFERENCES mancuerna.auth_challenges(account_id,id),
 FOREIGN KEY(account_id,approved_by_device_id) REFERENCES mancuerna.devices(account_id,id)
);
CREATE TABLE mancuerna.device_proofs(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, session_id uuid NOT NULL, device_id uuid NOT NULL,
 purpose text NOT NULL CHECK(purpose IN ('authorize_device','revoke_devices','recovery_key','phone_change','delete_account','vault_unlock')),
 nonce bytea NOT NULL CHECK(octet_length(nonce)=32), request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 token_hash bytea UNIQUE, expires_at timestamptz NOT NULL, verified_at timestamptz, consumed_at timestamptz, operation_id uuid,
 FOREIGN KEY(account_id,session_id) REFERENCES mancuerna.sessions(account_id,id),
 FOREIGN KEY(account_id,device_id) REFERENCES mancuerna.devices(account_id,id)
);
CREATE TABLE mancuerna.recovery_keys(
 id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES mancuerna.accounts(id),
 secret_verifier bytea NOT NULL CHECK(octet_length(secret_verifier)=32), verifier_key_version text NOT NULL, generation bigint NOT NULL CHECK(generation>0),
 created_at timestamptz NOT NULL DEFAULT now(), prepared_at timestamptz, consumed_at timestamptz, revoked_at timestamptz, UNIQUE(account_id,generation)
);
CREATE UNIQUE INDEX one_live_recovery_key ON mancuerna.recovery_keys(account_id) WHERE consumed_at IS NULL AND revoked_at IS NULL;
CREATE TABLE mancuerna.vault_keys(
 vault_id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES mancuerna.accounts(id), wrapped_data_key bytea NOT NULL,
 key_version text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), retired_at timestamptz, UNIQUE(account_id,vault_id)
);
CREATE TABLE mancuerna.security_events(
 id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES mancuerna.accounts(id), actor_device_id uuid,
 event_type text NOT NULL, request_id uuid NOT NULL, occurred_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(account_id,actor_device_id) REFERENCES mancuerna.devices(account_id,id)
);
CREATE TABLE mancuerna.auth_receipts(
 operation_id uuid PRIMARY KEY, account_id uuid REFERENCES mancuerna.accounts(id), attempt_id uuid REFERENCES mancuerna.auth_challenges(id),
 purpose text NOT NULL, request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32), result_reference uuid,
 response_ciphertext bytea, key_version text, response_expires_at timestamptz NOT NULL, result_secret_hash bytea,
 completed_at timestamptz NOT NULL DEFAULT now(), CHECK(account_id IS NOT NULL OR attempt_id IS NOT NULL),
 CHECK((response_ciphertext IS NULL)=(key_version IS NULL))
);
CREATE TABLE mancuerna.otp_budgets(
 scope text NOT NULL CHECK(scope IN ('destination','origin','service','recovery')), subject_hash bytea NOT NULL,
 window_start timestamptz NOT NULL, send_count integer NOT NULL DEFAULT 0 CHECK(send_count>=0), check_count integer NOT NULL DEFAULT 0 CHECK(check_count>=0),
 blocked_until timestamptz, PRIMARY KEY(scope,subject_hash,window_start)
);
CREATE TABLE mancuerna.catalog_exercises(
 id uuid PRIMARY KEY, name text NOT NULL CHECK(length(name) BETWEEN 1 AND 120), search_key text NOT NULL,
 load_mode text NOT NULL CHECK(load_mode IN ('external','none')), notes text NOT NULL DEFAULT '' CHECK(length(notes)<=1000),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0), archived_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz
);
CREATE TABLE mancuerna.personal_exercises(
 id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES mancuerna.accounts(id), name text NOT NULL CHECK(length(name) BETWEEN 1 AND 120), search_key text NOT NULL,
 load_mode text NOT NULL CHECK(load_mode IN ('external','none')), notes text NOT NULL DEFAULT '' CHECK(length(notes)<=1000),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0), archived_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz, UNIQUE(account_id,id)
);
CREATE INDEX personal_exercise_search ON mancuerna.personal_exercises(account_id, search_key);
CREATE TABLE mancuerna.routines(
 id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES mancuerna.accounts(id), title text NOT NULL CHECK(length(title)<=120),
 status text NOT NULL CHECK(status IN ('draft','ready','archived')), current_version_id uuid, duplicated_from_version_id uuid,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
 UNIQUE(account_id,id), CHECK(status<>'ready' OR current_version_id IS NOT NULL)
);
CREATE TABLE mancuerna.routine_versions(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, routine_id uuid NOT NULL, version_number integer NOT NULL CHECK(version_number>0),
 title_snapshot text NOT NULL CHECK(length(title_snapshot) BETWEEN 1 AND 120), content_hash bytea NOT NULL CHECK(octet_length(content_hash)=32),
 format_version integer NOT NULL DEFAULT 1 CHECK(format_version=1), source_draft_revision bigint NOT NULL CHECK(source_draft_revision>0),
 sealed boolean NOT NULL DEFAULT false, actor_device_id uuid NOT NULL, published_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(account_id,id), UNIQUE(account_id,routine_id,id), UNIQUE(account_id,routine_id,version_number),
 FOREIGN KEY(account_id,routine_id) REFERENCES mancuerna.routines(account_id,id),
 FOREIGN KEY(account_id,actor_device_id) REFERENCES mancuerna.devices(account_id,id)
);
ALTER TABLE mancuerna.routines ADD FOREIGN KEY(account_id,id,current_version_id) REFERENCES mancuerna.routine_versions(account_id,routine_id,id);
ALTER TABLE mancuerna.routines ADD FOREIGN KEY(account_id,duplicated_from_version_id) REFERENCES mancuerna.routine_versions(account_id,id);
CREATE TABLE mancuerna.routine_drafts(
 routine_id uuid PRIMARY KEY, account_id uuid NOT NULL, base_version_id uuid, revision bigint NOT NULL CHECK(revision>0),
 content jsonb, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
 FOREIGN KEY(account_id,routine_id) REFERENCES mancuerna.routines(account_id,id),
 FOREIGN KEY(account_id,routine_id,base_version_id) REFERENCES mancuerna.routine_versions(account_id,routine_id,id),
 CHECK(content IS NULL OR (jsonb_typeof(content)='object' AND octet_length(content::text)<=1048576)),
 CHECK((deleted_at IS NULL)=(content IS NOT NULL))
);
CREATE TABLE mancuerna.routine_days(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, version_id uuid NOT NULL, label text NOT NULL CHECK(length(label) BETWEEN 1 AND 80),
 position integer NOT NULL CHECK(position BETWEEN 0 AND 13), UNIQUE(account_id,version_id,id), UNIQUE(account_id,version_id,position),
 FOREIGN KEY(account_id,version_id) REFERENCES mancuerna.routine_versions(account_id,id)
);
CREATE TABLE mancuerna.routine_exercises(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, version_id uuid NOT NULL, day_id uuid NOT NULL,
 catalog_exercise_id uuid REFERENCES mancuerna.catalog_exercises(id), personal_exercise_id uuid,
 name_snapshot text NOT NULL CHECK(length(name_snapshot) BETWEEN 1 AND 120), load_mode_snapshot text NOT NULL CHECK(load_mode_snapshot IN ('external','none')),
 position integer NOT NULL CHECK(position BETWEEN 0 AND 49), prescribed_sets integer NOT NULL CHECK(prescribed_sets BETWEEN 1 AND 100),
 prescribed_reps text NOT NULL CHECK(length(prescribed_reps) BETWEEN 1 AND 80), rest_seconds integer CHECK(rest_seconds BETWEEN 0 AND 3600), notes text NOT NULL DEFAULT '' CHECK(length(notes)<=1000),
 CHECK((catalog_exercise_id IS NULL)<>(personal_exercise_id IS NULL)),
 UNIQUE(account_id,version_id,day_id,id), UNIQUE(account_id,day_id,position),
 FOREIGN KEY(account_id,version_id,day_id) REFERENCES mancuerna.routine_days(account_id,version_id,id),
 FOREIGN KEY(account_id,personal_exercise_id) REFERENCES mancuerna.personal_exercises(account_id,id)
);
CREATE TABLE mancuerna.workouts(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, routine_version_id uuid NOT NULL, routine_day_id uuid NOT NULL,
 routine_title_snapshot text NOT NULL, day_label_snapshot text NOT NULL, state text NOT NULL CHECK(state IN ('in_progress','completed')),
 started_at timestamptz NOT NULL, finished_at timestamptz, timezone text NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0), finalized_revision bigint, needs_review boolean NOT NULL DEFAULT false,
 snapshot_sealed boolean NOT NULL DEFAULT false, origin_device_id uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
 UNIQUE(account_id,id), UNIQUE(account_id,id,routine_version_id,routine_day_id),
 CHECK((state='completed')=(finished_at IS NOT NULL)), CHECK(finished_at IS NULL OR finished_at>=started_at),
 CHECK(finalized_revision IS NULL OR (finalized_revision>0 AND finalized_revision<=revision)),
 FOREIGN KEY(account_id,routine_version_id,routine_day_id) REFERENCES mancuerna.routine_days(account_id,version_id,id),
 FOREIGN KEY(account_id,origin_device_id) REFERENCES mancuerna.devices(account_id,id)
);
CREATE INDEX workout_history ON mancuerna.workouts(account_id,started_at,id) WHERE deleted_at IS NULL;
CREATE TABLE mancuerna.workout_exercises(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, workout_id uuid NOT NULL, routine_version_id uuid NOT NULL, routine_day_id uuid NOT NULL, source_exercise_id uuid NOT NULL,
 catalog_exercise_id uuid REFERENCES mancuerna.catalog_exercises(id), personal_exercise_id uuid,
 name_snapshot text NOT NULL, load_mode_snapshot text NOT NULL CHECK(load_mode_snapshot IN ('external','none')),
 position integer NOT NULL CHECK(position BETWEEN 0 AND 49), prescribed_sets integer NOT NULL CHECK(prescribed_sets BETWEEN 1 AND 100), prescribed_reps text NOT NULL,
 rest_seconds integer CHECK(rest_seconds BETWEEN 0 AND 3600), notes text NOT NULL,
 CHECK((catalog_exercise_id IS NULL)<>(personal_exercise_id IS NULL)), UNIQUE(account_id,workout_id,id), UNIQUE(account_id,workout_id,position), UNIQUE(account_id,workout_id,source_exercise_id),
 FOREIGN KEY(account_id,workout_id,routine_version_id,routine_day_id) REFERENCES mancuerna.workouts(account_id,id,routine_version_id,routine_day_id),
 FOREIGN KEY(account_id,routine_version_id,routine_day_id,source_exercise_id) REFERENCES mancuerna.routine_exercises(account_id,version_id,day_id,id),
 FOREIGN KEY(account_id,personal_exercise_id) REFERENCES mancuerna.personal_exercises(account_id,id)
);
CREATE TABLE mancuerna.workout_sets(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, workout_id uuid NOT NULL, workout_exercise_id uuid NOT NULL,
 origin_device_id uuid NOT NULL, position_hint integer NOT NULL CHECK(position_hint BETWEEN 0 AND 9999),
 load_mode text NOT NULL CHECK(load_mode IN ('external','none')), weight_value numeric(10,3), weight_unit text CHECK(weight_unit IN ('kg','lb')),
 repetitions integer NOT NULL CHECK(repetitions BETWEEN 0 AND 10000), revision bigint NOT NULL DEFAULT 1 CHECK(revision>0), captured_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz, UNIQUE(account_id,id),
 CHECK((load_mode='external' AND weight_value IS NOT NULL AND weight_value>=0 AND weight_unit IS NOT NULL) OR (load_mode='none' AND weight_value IS NULL AND weight_unit IS NULL)),
 FOREIGN KEY(account_id,workout_id,workout_exercise_id) REFERENCES mancuerna.workout_exercises(account_id,workout_id,id),
 FOREIGN KEY(account_id,origin_device_id) REFERENCES mancuerna.devices(account_id,id)
);
CREATE TABLE mancuerna.sync_streams(
 account_id uuid PRIMARY KEY REFERENCES mancuerna.accounts(id), epoch uuid NOT NULL, last_sequence bigint NOT NULL DEFAULT 0 CHECK(last_sequence>=0),
 compacted_through bigint NOT NULL DEFAULT 0 CHECK(compacted_through BETWEEN 0 AND last_sequence)
);
CREATE TABLE mancuerna.operation_receipts(
 operation_id uuid PRIMARY KEY, account_id uuid NOT NULL, actor_device_id uuid NOT NULL, origin_device_id uuid NOT NULL,
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32), command_type text NOT NULL,
 outcome text NOT NULL CHECK(outcome IN ('accepted','accepted_with_review','conflict','dependency_blocked','rejected')), result jsonb NOT NULL,
 committed_at timestamptz NOT NULL DEFAULT now(), UNIQUE(account_id,operation_id),
 FOREIGN KEY(account_id,actor_device_id) REFERENCES mancuerna.devices(account_id,id),
 FOREIGN KEY(account_id,origin_device_id) REFERENCES mancuerna.devices(account_id,id)
);
CREATE TABLE mancuerna.record_revisions(
 account_id uuid NOT NULL, entity_type text NOT NULL, entity_id uuid NOT NULL, revision bigint NOT NULL CHECK(revision>0),
 operation_id uuid NOT NULL, canonical jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(account_id,entity_type,entity_id,revision), FOREIGN KEY(account_id,operation_id) REFERENCES mancuerna.operation_receipts(account_id,operation_id) DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE mancuerna.sync_changes(
 account_id uuid NOT NULL REFERENCES mancuerna.sync_streams(account_id), sequence bigint NOT NULL CHECK(sequence>0), operation_id uuid NOT NULL,
 changes jsonb NOT NULL CHECK(jsonb_typeof(changes)='array' AND jsonb_array_length(changes)>0 AND octet_length(changes::text)<=1048576),
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(account_id,sequence),
 FOREIGN KEY(account_id,operation_id) REFERENCES mancuerna.operation_receipts(account_id,operation_id) DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE mancuerna.sync_conflicts(
 id uuid PRIMARY KEY, account_id uuid NOT NULL, operation_id uuid NOT NULL UNIQUE, entity_type text NOT NULL, entity_id uuid NOT NULL,
 base_revision bigint NOT NULL CHECK(base_revision>=0), current_revision bigint NOT NULL CHECK(current_revision>=0), proposal jsonb NOT NULL,
 reason text NOT NULL CHECK(reason IN ('revision','deleted','finalization')), state text NOT NULL CHECK(state IN ('open','resolved','superseded')),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0), resolution_operation_id uuid,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
 UNIQUE(account_id,id), FOREIGN KEY(account_id,operation_id) REFERENCES mancuerna.operation_receipts(account_id,operation_id) DEFERRABLE INITIALLY DEFERRED,
 FOREIGN KEY(account_id,resolution_operation_id) REFERENCES mancuerna.operation_receipts(account_id,operation_id) DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE mancuerna.sync_snapshots(
 id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES mancuerna.accounts(id), epoch uuid NOT NULL, through_sequence bigint NOT NULL CHECK(through_sequence>=0),
 page_count integer NOT NULL CHECK(page_count>0), expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(account_id,id)
);
CREATE TABLE mancuerna.sync_snapshot_pages(
 account_id uuid NOT NULL, snapshot_id uuid NOT NULL, page_number integer NOT NULL CHECK(page_number>=0), data jsonb NOT NULL, hash bytea NOT NULL CHECK(octet_length(hash)=32),
 PRIMARY KEY(account_id,snapshot_id,page_number), FOREIGN KEY(account_id,snapshot_id) REFERENCES mancuerna.sync_snapshots(account_id,id), CHECK(octet_length(data::text)<=1048576)
);
-- Private rows are inaccessible without an explicit account context; mixed tenant contexts fail closed.
CREATE FUNCTION mancuerna_private.account_id() RETURNS uuid LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
 SELECT CASE WHEN coalesce(current_setting('tinta.gym_id',true),'')='' THEN nullif(current_setting('mancuerna.account_id',true),'')::uuid ELSE NULL END
$$;
REVOKE ALL ON FUNCTION mancuerna_private.account_id() FROM PUBLIC;
GRANT USAGE ON SCHEMA mancuerna,mancuerna_private TO mancuerna_data,mancuerna_auth,mancuerna_identity;
GRANT EXECUTE ON FUNCTION mancuerna_private.account_id() TO mancuerna_data,mancuerna_auth,mancuerna_identity;
DO $$ DECLARE t text; BEGIN
 FOR t IN SELECT table_name FROM information_schema.columns WHERE table_schema='mancuerna' AND column_name='account_id' AND table_name NOT IN ('auth_challenges','auth_receipts') LOOP
  EXECUTE format('ALTER TABLE mancuerna.%I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE mancuerna.%I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY personal_owner ON mancuerna.%I USING(account_id=mancuerna_private.account_id()) WITH CHECK(account_id=mancuerna_private.account_id())',t);
 END LOOP;
END $$;
ALTER TABLE mancuerna.accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE mancuerna.accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY personal_owner ON mancuerna.accounts USING(id=mancuerna_private.account_id()) WITH CHECK(id=mancuerna_private.account_id());
ALTER TABLE mancuerna.auth_challenges ENABLE ROW LEVEL SECURITY;
ALTER TABLE mancuerna.auth_challenges FORCE ROW LEVEL SECURITY;
CREATE POLICY personal_attempt ON mancuerna.auth_challenges TO mancuerna_auth USING(
 coalesce(current_setting('tinta.gym_id',true),'')='' AND
 (account_id=mancuerna_private.account_id() OR (id=nullif(current_setting('mancuerna.attempt_id',true),'')::uuid AND continuation_hash=decode(current_setting('mancuerna.attempt_hash',true),'hex')))
) WITH CHECK(coalesce(current_setting('tinta.gym_id',true),'')='' AND
 (account_id=mancuerna_private.account_id() OR (id=nullif(current_setting('mancuerna.attempt_id',true),'')::uuid AND continuation_hash=decode(current_setting('mancuerna.attempt_hash',true),'hex'))));
ALTER TABLE mancuerna.auth_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE mancuerna.auth_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY personal_receipt ON mancuerna.auth_receipts TO mancuerna_auth USING(account_id=mancuerna_private.account_id() OR EXISTS(SELECT 1 FROM mancuerna.auth_challenges a WHERE a.id=attempt_id)) WITH CHECK(account_id=mancuerna_private.account_id() OR EXISTS(SELECT 1 FROM mancuerna.auth_challenges a WHERE a.id=attempt_id));
-- Auth lookup is a narrow database interface, not an HTTP lookup endpoint.
GRANT SELECT ON mancuerna.accounts,mancuerna.sessions,mancuerna.session_rotations,mancuerna.recovery_keys,mancuerna.auth_receipts TO mancuerna_identity;
CREATE POLICY identity_lookup ON mancuerna.accounts TO mancuerna_identity USING(true);
CREATE POLICY identity_lookup ON mancuerna.sessions TO mancuerna_identity USING(true);
CREATE POLICY identity_lookup ON mancuerna.session_rotations TO mancuerna_identity USING(true);
CREATE POLICY identity_lookup ON mancuerna.recovery_keys TO mancuerna_identity USING(true);
CREATE POLICY identity_lookup ON mancuerna.auth_receipts TO mancuerna_identity USING(true);
GRANT CREATE ON SCHEMA mancuerna_private TO mancuerna_identity;
CREATE FUNCTION mancuerna_private.resolve_identity(kind text, lookup text) RETURNS uuid LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE answer uuid;
BEGIN
 CASE kind
 WHEN 'phone' THEN SELECT id INTO answer FROM mancuerna.accounts WHERE phone_e164=lookup;
 WHEN 'recovery' THEN SELECT account_id INTO answer FROM mancuerna.recovery_keys WHERE id=lookup::uuid;
 WHEN 'refresh' THEN
  SELECT account_id INTO answer FROM mancuerna.sessions WHERE refresh_hash=decode(lookup,'hex');
  IF answer IS NULL THEN SELECT account_id INTO answer FROM mancuerna.session_rotations WHERE old_refresh_hash=decode(lookup,'hex'); END IF;
 WHEN 'logout' THEN SELECT account_id INTO answer FROM mancuerna.sessions WHERE revocation_ticket_hash=decode(lookup,'hex');
 WHEN 'deletion_result' THEN SELECT account_id INTO answer FROM mancuerna.auth_receipts WHERE result_secret_hash=decode(lookup,'hex') AND purpose='delete_account';
 ELSE RAISE EXCEPTION 'unsupported identity lookup';
 END CASE;
 RETURN answer;
END $$;
ALTER FUNCTION mancuerna_private.resolve_identity(text,text) OWNER TO mancuerna_identity;
REVOKE CREATE ON SCHEMA mancuerna_private FROM mancuerna_identity;
REVOKE ALL ON FUNCTION mancuerna_private.resolve_identity(text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mancuerna_private.resolve_identity(text,text) TO mancuerna_auth;
GRANT SELECT ON mancuerna.schema_versions,mancuerna.accounts,mancuerna.devices,mancuerna.catalog_exercises TO mancuerna_data;
GRANT UPDATE(display_name,weight_unit,revision,updated_at) ON mancuerna.accounts TO mancuerna_data;
GRANT SELECT,INSERT,UPDATE ON mancuerna.personal_exercises,mancuerna.routines,mancuerna.routine_drafts,mancuerna.workouts,mancuerna.workout_sets,mancuerna.sync_conflicts,mancuerna.sync_streams TO mancuerna_data;
GRANT SELECT,INSERT ON mancuerna.routine_versions,mancuerna.routine_days,mancuerna.routine_exercises,mancuerna.workout_exercises,mancuerna.operation_receipts,mancuerna.record_revisions,mancuerna.sync_changes,mancuerna.sync_snapshots,mancuerna.sync_snapshot_pages TO mancuerna_data;
GRANT SELECT,INSERT,UPDATE ON mancuerna.accounts,mancuerna.devices,mancuerna.sessions,mancuerna.session_rotations,mancuerna.auth_challenges,mancuerna.device_authorizations,mancuerna.device_proofs,mancuerna.recovery_keys,mancuerna.vault_keys,mancuerna.auth_receipts,mancuerna.otp_budgets,mancuerna.sync_streams TO mancuerna_auth;
GRANT SELECT,INSERT ON mancuerna.security_events TO mancuerna_auth;
GRANT SELECT ON mancuerna.schema_versions TO mancuerna_auth;
-- No DELETE/TRUNCATE, catalog writes, schema creation, role membership, PUBLIC grants, or access to Tinta tables.
-- Session validation is a scoped function, not permission to SELECT token verifiers in the data pool.
CREATE FUNCTION mancuerna_private.session_active(sid uuid,did uuid) RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM mancuerna.sessions s JOIN mancuerna.devices d ON (d.account_id,d.id)=(s.account_id,s.device_id)
 JOIN mancuerna.accounts a ON a.id=s.account_id
 WHERE s.account_id=mancuerna_private.account_id() AND s.id=sid AND s.device_id=did AND s.revoked_at IS NULL AND s.expires_at>now() AND d.revoked_at IS NULL AND a.status='active')
$$;
GRANT SELECT ON mancuerna.devices TO mancuerna_identity;
CREATE POLICY identity_lookup ON mancuerna.devices TO mancuerna_identity USING(true);
GRANT CREATE ON SCHEMA mancuerna_private TO mancuerna_identity;
ALTER FUNCTION mancuerna_private.session_active(uuid,uuid) OWNER TO mancuerna_identity;
REVOKE CREATE ON SCHEMA mancuerna_private FROM mancuerna_identity;
REVOKE ALL ON FUNCTION mancuerna_private.session_active(uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION mancuerna_private.session_active(uuid,uuid) TO mancuerna_data,mancuerna_auth;
CREATE INDEX routines_library ON mancuerna.routines(account_id,status,updated_at,id) WHERE deleted_at IS NULL;
CREATE INDEX sets_workout ON mancuerna.workout_sets(account_id,workout_id,workout_exercise_id,id);
CREATE INDEX conflicts_queue ON mancuerna.sync_conflicts(account_id,state,id);
CREATE INDEX sessions_device ON mancuerna.sessions(account_id,device_id,revoked_at);
CREATE INDEX challenges_expiry ON mancuerna.auth_challenges(expires_at) WHERE consumed_at IS NULL;
CREATE INDEX snapshot_expiry ON mancuerna.sync_snapshots(expires_at);
-- A version/snapshot is constructed in one transaction and sealed before commit.
GRANT UPDATE(sealed) ON mancuerna.routine_versions TO mancuerna_data;
CREATE FUNCTION mancuerna_private.immutable_version() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF OLD.sealed THEN RAISE EXCEPTION 'published version immutable'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER immutable_version BEFORE UPDATE OR DELETE ON mancuerna.routine_versions FOR EACH ROW EXECUTE FUNCTION mancuerna_private.immutable_version();
CREATE FUNCTION mancuerna_private.version_child() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF (SELECT sealed FROM mancuerna.routine_versions WHERE account_id=NEW.account_id AND id=NEW.version_id) THEN RAISE EXCEPTION 'published children immutable'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER version_child BEFORE INSERT ON mancuerna.routine_days FOR EACH ROW EXECUTE FUNCTION mancuerna_private.version_child();
CREATE TRIGGER version_child BEFORE INSERT ON mancuerna.routine_exercises FOR EACH ROW EXECUTE FUNCTION mancuerna_private.version_child();
CREATE FUNCTION mancuerna_private.complete_version() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT (SELECT sealed FROM mancuerna.routine_versions WHERE id=NEW.id AND account_id=NEW.account_id)
 OR NOT EXISTS(SELECT 1 FROM mancuerna.routine_days WHERE account_id=NEW.account_id AND version_id=NEW.id)
 OR EXISTS(SELECT 1 FROM mancuerna.routine_days d WHERE d.account_id=NEW.account_id AND d.version_id=NEW.id AND NOT EXISTS(SELECT 1 FROM mancuerna.routine_exercises e WHERE (e.account_id,e.version_id,e.day_id)=(d.account_id,d.version_id,d.id)))
 THEN RAISE EXCEPTION 'incomplete publication'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER complete_version AFTER INSERT OR UPDATE ON mancuerna.routine_versions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION mancuerna_private.complete_version();
CREATE FUNCTION mancuerna_private.immutable_workout() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF OLD.snapshot_sealed AND (NOT NEW.snapshot_sealed OR ROW(OLD.routine_version_id,OLD.routine_day_id,OLD.routine_title_snapshot,OLD.day_label_snapshot,OLD.started_at,OLD.timezone,OLD.origin_device_id) IS DISTINCT FROM ROW(NEW.routine_version_id,NEW.routine_day_id,NEW.routine_title_snapshot,NEW.day_label_snapshot,NEW.started_at,NEW.timezone,NEW.origin_device_id)) THEN RAISE EXCEPTION 'workout snapshot immutable'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER immutable_workout BEFORE UPDATE ON mancuerna.workouts FOR EACH ROW EXECUTE FUNCTION mancuerna_private.immutable_workout();
CREATE FUNCTION mancuerna_private.workout_child() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF (SELECT snapshot_sealed FROM mancuerna.workouts WHERE account_id=NEW.account_id AND id=NEW.workout_id) THEN RAISE EXCEPTION 'workout children immutable'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER workout_child BEFORE INSERT ON mancuerna.workout_exercises FOR EACH ROW EXECUTE FUNCTION mancuerna_private.workout_child();
CREATE FUNCTION mancuerna_private.complete_workout() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT (SELECT snapshot_sealed FROM mancuerna.workouts WHERE id=NEW.id AND account_id=NEW.account_id)
 OR (SELECT count(*) FROM mancuerna.workout_exercises WHERE account_id=NEW.account_id AND workout_id=NEW.id) <> (SELECT count(*) FROM mancuerna.routine_exercises WHERE (account_id,version_id,day_id)=(NEW.account_id,NEW.routine_version_id,NEW.routine_day_id))
 THEN RAISE EXCEPTION 'incomplete workout snapshot'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER complete_workout AFTER INSERT OR UPDATE ON mancuerna.workouts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION mancuerna_private.complete_workout();
REVOKE ALL ON FUNCTION mancuerna_private.immutable_version(),mancuerna_private.version_child(),mancuerna_private.complete_version(),mancuerna_private.immutable_workout(),mancuerna_private.workout_child(),mancuerna_private.complete_workout() FROM PUBLIC;

INSERT INTO _migrations(version,name) VALUES(59,'059_mancuerna_personal') ON CONFLICT(version) DO NOTHING;
COMMIT;
