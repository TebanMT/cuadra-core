-- Run explicitly as cluster administrator before migration 059. No LOGINs,
-- passwords, role grants to Tinta, or changes to existing roles are made here.
-- Re-running is safe only if existing group roles match the required boundary.
BEGIN;
DO $$
DECLARE name text;
BEGIN
 FOREACH name IN ARRAY ARRAY['mancuerna_data','mancuerna_auth','mancuerna_identity'] LOOP
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=name) THEN
   BEGIN
    EXECUTE format('CREATE ROLE %I NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION',name);
   EXCEPTION WHEN duplicate_object THEN
    -- Another isolated database test may provision the same cluster roles.
    NULL;
   END;
  END IF;
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=name AND (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolcanlogin OR rolreplication)) THEN
   RAISE EXCEPTION 'Unsafe pre-existing Mancuerna group role: %',name;
  END IF;
  IF EXISTS(SELECT 1 FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname=name)) THEN
   RAISE EXCEPTION 'Mancuerna group role must not inherit other roles: %',name;
  END IF;
 END LOOP;
END $$;
COMMIT;
