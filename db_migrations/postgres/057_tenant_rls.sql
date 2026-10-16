-- Runtime uses a separate NOSUPERUSER NOBYPASSRLS role which owns no tables.
-- Only these narrow identity/discovery functions run as the migration owner.
-- They return IDs or aggregate counts, never customer records or credentials.
BEGIN;
CREATE SCHEMA IF NOT EXISTS tinta_private;
REVOKE ALL ON SCHEMA tinta_private FROM PUBLIC;
CREATE OR REPLACE FUNCTION tinta_private.resolve_tenant(kind text, lookup text)
RETURNS uuid LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE result uuid;
BEGIN
 CASE kind
 WHEN 'email' THEN SELECT gym_id INTO result FROM public.users WHERE lower(email)=lower(trim(lookup)) AND deleted_at IS NULL LIMIT 1;
 WHEN 'user' THEN SELECT gym_id INTO result FROM public.users WHERE id=lookup::uuid AND deleted_at IS NULL;
 WHEN 'reset' THEN SELECT u.gym_id INTO result FROM public.password_reset_tokens t JOIN public.users u ON u.id=t.user_id WHERE t.token_hash=decode(lookup,'hex') AND t.used_at IS NULL LIMIT 1;
 WHEN 'verify' THEN SELECT u.gym_id INTO result FROM public.email_verification_tokens t JOIN public.users u ON u.id=t.user_id WHERE t.token_hash=decode(lookup,'hex') AND t.used_at IS NULL LIMIT 1;
 WHEN 'sidecar' THEN SELECT gym_id INTO result FROM public.sidecar_credentials WHERE key_hash=decode(lookup,'hex') AND revoked_at IS NULL LIMIT 1;
 WHEN 'bootstrap' THEN SELECT gym_id INTO result FROM public.installer_bootstraps WHERE token_hash=decode(lookup,'hex') LIMIT 1;
 WHEN 'whatsapp' THEN SELECT id INTO result FROM public.gyms WHERE whatsapp_business_phone=lookup AND deleted_at IS NULL LIMIT 1;
 WHEN 'notification' THEN SELECT gym_id INTO result FROM public.notification_queue WHERE provider_message_id=lookup LIMIT 1;
 ELSE RAISE EXCEPTION 'unknown tenant identity';
 END CASE;
 RETURN result;
END $$;
CREATE OR REPLACE FUNCTION tinta_private.worker_tenants(after_id uuid)
RETURNS SETOF uuid LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT id FROM public.gyms WHERE id>after_id AND deleted_at IS NULL ORDER BY id LIMIT 100
$$;
-- Signup needs a global email uniqueness check, but no unscoped user SELECT.
CREATE OR REPLACE FUNCTION tinta_private.email_exists(address text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT EXISTS(SELECT 1 FROM public.users WHERE lower(email)=lower(trim(address)) AND deleted_at IS NULL)
$$;
CREATE OR REPLACE FUNCTION tinta_private.launch_used()
RETURNS bigint LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT COUNT(*) FROM public.launch_benefits
$$;
CREATE OR REPLACE FUNCTION tinta_private.launch_next_slot()
RETURNS bigint LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 SELECT COALESCE(MAX(slot),0)+1 FROM public.launch_benefits
$$;
CREATE OR REPLACE FUNCTION tinta_private.revoke_idle(threshold timestamptz)
RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
DECLARE affected bigint;
BEGIN
 UPDATE public.sidecar_credentials SET revoked_at=clock_timestamp() WHERE revoked_at IS NULL AND last_seen_at<threshold;
 GET DIAGNOSTICS affected=ROW_COUNT;RETURN affected;
END $$;
CREATE OR REPLACE FUNCTION tinta_private.log_unscoped_whatsapp(event jsonb)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,public AS $$
 INSERT INTO public.whatsapp_events(id,gym_id,notification_id,provider_message_id,event_type,status,error_code,error_message,raw_payload,received_at)
 SELECT (event->>'ID')::uuid,NULL,NULL,event->>'ProviderMessageID',event->>'EventType',event->>'Status',event->>'ErrorCode',event->>'ErrorMessage',event->'RawPayload',(event->>'ReceivedAt')::timestamptz
$$;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA tinta_private FROM PUBLIC;

DO $$ DECLARE tab record; BEGIN
 FOR tab IN SELECT c.table_name FROM information_schema.columns c JOIN information_schema.tables t USING(table_schema,table_name)
 WHERE c.table_schema='public' AND c.column_name='gym_id' AND t.table_type='BASE TABLE'
 LOOP
  EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY',tab.table_name);
  EXECUTE format('CREATE POLICY tinta_tenant ON public.%I USING (gym_id = NULLIF(current_setting(''tinta.gym_id'',true),'''')::uuid) WITH CHECK (gym_id = NULLIF(current_setting(''tinta.gym_id'',true),'''')::uuid)',tab.table_name);
 END LOOP;
 -- Auth records without a duplicated gym_id inherit scope from their user.
 FOR tab IN SELECT unnest(ARRAY['password_reset_tokens','email_verification_tokens','refresh_token_blacklist']) AS table_name LOOP
  EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY',tab.table_name);
  EXECUTE format('CREATE POLICY tinta_tenant ON public.%I USING (EXISTS(SELECT 1 FROM public.users u WHERE u.id=user_id)) WITH CHECK (EXISTS(SELECT 1 FROM public.users u WHERE u.id=user_id))',tab.table_name);
 END LOOP;
END $$;
INSERT INTO _migrations(version,name) VALUES(57,'057_tenant_rls') ON CONFLICT(version) DO NOTHING;
COMMIT;
