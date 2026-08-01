-- ============================================================================
-- purge_demo_gyms.sql — BORRADO DEFINITIVO de gyms de demo (cloud Postgres).
-- ============================================================================
-- Borra TODAS las filas de TODAS las tablas asociadas a los gyms listados en
-- la sección "EDITA AQUÍ". Es un hard delete real (no soft delete): no hay
-- vuelta atrás después del COMMIT.
--
-- USO (dos pasos, siempre):
--
--   1) DRY RUN — imprime identidad de cada gym, conteos por tabla y ejecuta
--      los DELETEs DENTRO de una transacción que termina en ROLLBACK.
--      No cambia nada; sirve para revisar el reporte completo:
--
--        psql "$DATABASE_URL" -f scripts/purge_demo_gyms.sql
--
--   2) EJECUCIÓN REAL — mismo comando + la variable commit:
--
--        psql "$DATABASE_URL" -v commit=1 -f scripts/purge_demo_gyms.sql
--
-- GUARDAS (abortan la transacción entera):
--   - El placeholder de UUID sigue sin editar.
--   - Algún UUID no existe en `gyms`.
--   - Algún gym tiene stripe_customer_id o plan distinto de 'trial' —
--     protege contra borrar un gym REAL de paga por typo. No hay flag para
--     saltarla a propósito: si de verdad necesitas borrar un gym pagado,
--     edita esa guarda a mano y sabrás lo que estás haciendo.
--   - Verificación final dinámica: recorre information_schema y exige CERO
--     filas restantes de los targets en CUALQUIER tabla pública con columna
--     gym_id — si una migración futura agrega una tabla que este script no
--     cubre, el script FALLA en vez de dejar huérfanos en silencio.
--
-- RECOMENDACIÓN: corre esto con los desktops de los gyms demo cerrados. Si un
-- sidecar sincroniza a media transacción, el peor caso es que la transacción
-- aborte por FK (ruidoso y sin efecto) — re-ejecuta y ya.
--
-- LO QUE ESTE SCRIPT *NO* BORRA (limpieza manual, por gym_id):
--   - R2 bucket privado:  prefijo  gyms/<gym_id>/        (fotos socios/productos)
--   - R2 bucket público:  prefijos welcome/<gym_id>/  y  receipts/<gym_id>/
--   - Disco del server:   /uploads/<gym_id>/             (logo del gym)
--       (rclone/aws s3 rm --recursive con el prefijo, o desde el dashboard CF)
--   - Stripe/MP: la guarda impide gyms con customer; si un demo llegó a tener
--     algo en Stripe (no debería), cancélalo/bórralo en el dashboard de Stripe.
--   - whatsapp_opt_outs: se conserva A PROPÓSITO — es global por teléfono y es
--     un registro de consentimiento; borrar el gym no borra el opt-out.
--   - El SQLite local de los desktops demo (desinstalar la app y ya). Si uno
--     intenta sincronizar después del purge, recibirá 401 — inofensivo.
-- ============================================================================

\set ON_ERROR_STOP on

BEGIN;

-- ────────────────────────────────────────────────────────────────────────────
-- EDITA AQUÍ: UUIDs de los gyms demo a borrar (uno por línea).
-- ────────────────────────────────────────────────────────────────────────────
CREATE TEMP TABLE _purge_targets (gym_id uuid PRIMARY KEY) ON COMMIT DROP;
INSERT INTO _purge_targets (gym_id) VALUES
    ('00000000-0000-0000-0000-000000000000');  -- ← REEMPLAZAR (placeholder aborta)

-- ────────────────────────────────────────────────────────────────────────────
-- GUARDAS + identidad de cada target (revisa este bloque en el dry run).
-- ────────────────────────────────────────────────────────────────────────────
DO $$
DECLARE
    g record;
    missing int;
BEGIN
    IF EXISTS (SELECT 1 FROM _purge_targets
               WHERE gym_id = '00000000-0000-0000-0000-000000000000') THEN
        RAISE EXCEPTION 'placeholder sin editar — pon los UUID reales de los gyms demo';
    END IF;

    SELECT count(*) INTO missing
    FROM _purge_targets t
    WHERE NOT EXISTS (SELECT 1 FROM gyms WHERE id = t.gym_id);
    IF missing > 0 THEN
        RAISE EXCEPTION '% target(s) no existen en gyms — revisa los UUID', missing;
    END IF;

    FOR g IN
        SELECT gy.id, gy.name, gy.subscription_plan, gy.subscription_status,
               gy.stripe_customer_id, gy.created_at,
               (SELECT count(*) FROM members  m WHERE m.gym_id = gy.id) AS members,
               (SELECT count(*) FROM payments p WHERE p.gym_id = gy.id) AS payments,
               (SELECT count(*) FROM checkins c WHERE c.gym_id = gy.id) AS checkins,
               (SELECT string_agg(u.email, ', ') FROM users u
                 WHERE u.gym_id = gy.id AND u.role = 'owner') AS owners
        FROM gyms gy
        JOIN _purge_targets t ON t.gym_id = gy.id
        ORDER BY gy.created_at
    LOOP
        RAISE NOTICE 'TARGET % | "%" | plan=% status=% | creado=% | owner(s)=% | socios=% pagos=% checkins=%',
            g.id, g.name, g.subscription_plan, g.subscription_status,
            g.created_at, coalesce(g.owners, '—'), g.members, g.payments, g.checkins;

        -- Guarda anti-"borré mi gym real": un demo jamás tiene Stripe customer
        -- ni plan de paga. Sin override por diseño.
        IF g.stripe_customer_id IS NOT NULL OR g.subscription_plan <> 'trial' THEN
            RAISE EXCEPTION 'el gym % ("%") tiene plan=% / stripe_customer=% — NO parece demo; abortando todo',
                g.id, g.name, g.subscription_plan, coalesce(g.stripe_customer_id, 'null');
        END IF;
    END LOOP;
END $$;

-- ────────────────────────────────────────────────────────────────────────────
-- Conteos por tabla ANTES de borrar (solo tablas con filas de los targets).
-- Dinámico sobre information_schema: cubre también tablas futuras con gym_id.
-- ────────────────────────────────────────────────────────────────────────────
DO $$
DECLARE
    t record;
    n bigint;
BEGIN
    FOR t IN
        SELECT c.table_name FROM information_schema.columns c
        JOIN information_schema.tables ti
          ON ti.table_schema = c.table_schema AND ti.table_name = c.table_name
        WHERE c.table_schema = 'public' AND c.column_name = 'gym_id'
          AND ti.table_type = 'BASE TABLE'
        ORDER BY c.table_name
    LOOP
        EXECUTE format(
            'SELECT count(*) FROM %I WHERE gym_id IN (SELECT gym_id FROM _purge_targets)',
            t.table_name) INTO n;
        IF n > 0 THEN
            RAISE NOTICE 'a borrar: % — % filas', t.table_name, n;
        END IF;
    END LOOP;
    -- Tablas sin gym_id, colgadas de users:
    FOR t IN SELECT unnest(ARRAY['email_verification_tokens',
                                 'password_reset_tokens',
                                 'refresh_token_blacklist']) AS table_name
    LOOP
        EXECUTE format(
            'SELECT count(*) FROM %I WHERE user_id IN
               (SELECT id FROM users WHERE gym_id IN (SELECT gym_id FROM _purge_targets))',
            t.table_name) INTO n;
        IF n > 0 THEN
            RAISE NOTICE 'a borrar: % — % filas (vía users)', t.table_name, n;
        END IF;
    END LOOP;
END $$;

-- ────────────────────────────────────────────────────────────────────────────
-- Neutralizar self-FKs (todas nullable). Sin esto, un DELETE masivo puede
-- tropezar con su propio ON DELETE RESTRICT según el orden interno de filas.
-- ────────────────────────────────────────────────────────────────────────────
UPDATE payments               SET parent_payment_id = NULL WHERE gym_id IN (SELECT gym_id FROM _purge_targets) AND parent_payment_id IS NOT NULL;
UPDATE memberships            SET replaced_by       = NULL WHERE gym_id IN (SELECT gym_id FROM _purge_targets) AND replaced_by       IS NOT NULL;
UPDATE challenge_measurements SET superseded_by_id  = NULL WHERE gym_id IN (SELECT gym_id FROM _purge_targets) AND superseded_by_id  IS NOT NULL;
UPDATE users                  SET created_by        = NULL WHERE gym_id IN (SELECT gym_id FROM _purge_targets) AND created_by        IS NOT NULL;

-- ────────────────────────────────────────────────────────────────────────────
-- DELETEs en orden FK-safe (hijos → padres). Todas las FKs del esquema son
-- ON DELETE RESTRICT salvo excepciones puntuales, así que cualquier tabla
-- olvidada o mal ordenada ABORTA la transacción — nunca deja estado parcial.
-- ────────────────────────────────────────────────────────────────────────────
DELETE FROM whatsapp_events        WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM notification_queue     WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM applied_promotions     WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM sale_items             WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM sales                  WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM challenge_measurements WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM challenge_participants WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM challenge_categories   WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM challenges             WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM membership_adjustments WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM memberships            WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM payments               WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM checkins               WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM member_fingerprints    WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM contact_attempts       WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM stock_movements        WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM promotions             WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM products               WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM membership_types       WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM members                WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM owner_alert_configs    WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM notification_templates WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM cash_close_events      WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM expenses               WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM subscription_events    WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM audit_log              WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM conflict_log           WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM installer_bootstraps   WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM sidecar_credentials    WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM gym_keys               WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM ownership_transfer_otps WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM gym_ownership_transfers WHERE gym_id IN (SELECT gym_id FROM _purge_targets);

-- Tablas sin gym_id: cuelgan de users.
DELETE FROM email_verification_tokens WHERE user_id IN (SELECT id FROM users WHERE gym_id IN (SELECT gym_id FROM _purge_targets));
DELETE FROM password_reset_tokens     WHERE user_id IN (SELECT id FROM users WHERE gym_id IN (SELECT gym_id FROM _purge_targets));
DELETE FROM refresh_token_blacklist   WHERE user_id IN (SELECT id FROM users WHERE gym_id IN (SELECT gym_id FROM _purge_targets));

DELETE FROM users         WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM sync_entities WHERE gym_id IN (SELECT gym_id FROM _purge_targets);
DELETE FROM gyms          WHERE id     IN (SELECT gym_id FROM _purge_targets);

-- ────────────────────────────────────────────────────────────────────────────
-- Verificación final: cero filas restantes en TODA tabla pública con gym_id.
-- Cubre tablas que existan hoy Y las que se agreguen en el futuro.
-- ────────────────────────────────────────────────────────────────────────────
DO $$
DECLARE
    t record;
    n bigint;
    leftovers text := '';
BEGIN
    FOR t IN
        SELECT c.table_name FROM information_schema.columns c
        JOIN information_schema.tables ti
          ON ti.table_schema = c.table_schema AND ti.table_name = c.table_name
        WHERE c.table_schema = 'public' AND c.column_name = 'gym_id'
          AND ti.table_type = 'BASE TABLE'
    LOOP
        EXECUTE format(
            'SELECT count(*) FROM %I WHERE gym_id IN (SELECT gym_id FROM _purge_targets)',
            t.table_name) INTO n;
        IF n > 0 THEN
            leftovers := leftovers || format(' %s(%s)', t.table_name, n);
        END IF;
    END LOOP;
    IF leftovers <> '' THEN
        RAISE EXCEPTION 'quedaron filas huérfanas:% — tabla nueva no cubierta por el script? Nada se borró (rollback).', leftovers;
    END IF;
    RAISE NOTICE 'verificación OK: cero filas restantes de los targets en todas las tablas con gym_id';
END $$;

-- ────────────────────────────────────────────────────────────────────────────
-- DRY RUN por default. Solo con  -v commit=1  se vuelve permanente.
-- ────────────────────────────────────────────────────────────────────────────
\if :{?commit}
COMMIT;
\echo '*** COMMIT — los gyms demo fueron borrados PERMANENTEMENTE. Recuerda la limpieza de R2 y /uploads (ver header). ***'
\else
ROLLBACK;
\echo '*** DRY RUN — ROLLBACK ejecutado, NADA fue borrado. Revisa el reporte y re-corre con -v commit=1 ***'
\endif
