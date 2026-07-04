-- backfill_duration_months.sql — corrige planes de calendario creados sin
-- duration_months (bug jul-2026).
--
-- CONTEXTO: hasta jul-2026 el wizard del dashboard (Step3FirstPlan) y el
-- form del desktop creaban planes "mensuales" mandando sólo duration_days=30
-- → el dominio caía al cálculo por días corridos y una mensual vendida el
-- 2-jul vencía el 1-ago en lugar del 2-ago. El FE ya manda duration_months
-- explícito; este script corrige los planes que nacieron ANTES del fix.
--
-- ⚠️ CÓMO USARLO:
--
--   PASO 1 (siempre): correr la verificación read-only para ver qué planes
--   están afectados y en qué gyms.
--
--   PASO 2 (RECOMENDADO, sin SQL): re-guardar cada plan afectado desde el
--   form arreglado (Desktop → Ajustes → Membresías → Editar → elegir
--   "1 mes (mensual)" → Guardar). Eso pasa por el use case UpdateMembershipType,
--   que escribe la tabla Y el journal de sync (sync_entities) atómicamente
--   → propaga a todos los sidecars. Cero riesgo.
--
--   PASO 3 (sólo si hay MUCHOS planes / no hay acceso al gym): el UPDATE
--   masivo de abajo. OJO: hay que tocar TAMBIÉN sync_entities (el pull de
--   los sidecars lee de ahí, no de membership_types) — por eso el script
--   actualiza ambos. Mismo criterio que el backfill de la migración 027:
--   sólo los presets no ambiguos {30,60,90,180,365} → {1,2,3,6,12}.
--   Cualquier otro valor (45, 20, …) se queda en días — esos los confirma
--   el dueño del gym y se corrigen por el form.
--
-- Las MEMBERSHIPS ya vendidas NO se tocan (snapshot inmutable, DA-11.1);
-- las renovaciones futuras toman el plan corregido. Si hace falta corregir
-- la vigencia de un socio ya cobrado, usar UC-017 (ajustar vigencia) con
-- razón explícita.

-- ────────────────────────────────────────────────────────────────────────
-- PASO 1 — VERIFICACIÓN (read-only)
-- ────────────────────────────────────────────────────────────────────────
SELECT mt.id, mt.gym_id, g.name AS gym, mt.name, mt.duration_days,
       mt.duration_months, mt.active, mt.created_at
  FROM membership_types mt
  LEFT JOIN gyms g ON g.id = mt.gym_id
 WHERE mt.deleted_at IS NULL
   AND mt.duration_months IS NULL
 ORDER BY mt.created_at DESC;

-- ────────────────────────────────────────────────────────────────────────
-- PASO 3 — BACKFILL MASIVO (idempotente; correr dentro de una transacción)
-- Descomentar para ejecutar.
-- ────────────────────────────────────────────────────────────────────────
-- BEGIN;
--
-- -- 3a. Tabla canónica. version+1 y updated_at=NOW() para que la fila
-- --     gane en LWW contra copias viejas de los sidecars.
-- UPDATE membership_types SET duration_months = d.m, version = version + 1, updated_at = NOW()
--   FROM (VALUES (30, 1), (60, 2), (90, 3), (180, 6), (365, 12)) AS d(days, m)
--  WHERE duration_months IS NULL AND deleted_at IS NULL AND duration_days = d.days;
--
-- -- 3b. Journal de sync — los sidecars hacen pull de acá (no de la tabla).
-- --     Alineamos payload/version, bumpeamos payload.updated_at (UnixMilli;
-- --     es lo que compara el LWW del sidecar al aplicar) y server_updated_at
-- --     (cursor del pull) para que el delta baje a todos los dispositivos.
-- --     Payload keys según emitMembershipTypeToSync (membership_type_postgres.go).
-- UPDATE sync_entities se
--    SET payload = se.payload || jsonb_build_object(
--                    'duration_months', mt.duration_months,
--                    'version',         mt.version,
--                    'updated_at',      (EXTRACT(EPOCH FROM NOW()) * 1000)::bigint),
--        version = mt.version,
--        server_updated_at = NOW()
--   FROM membership_types mt
--  WHERE se.entity_type = 'membership_types'
--    AND se.entity_id   = mt.id
--    AND mt.duration_months IS NOT NULL
--    AND (se.payload ->> 'duration_months') IS NULL;  -- cubre key ausente y JSON null
--
-- COMMIT;
--
-- Si algún plan corregido en 3a NO tiene fila en sync_entities (creado
-- cloud-side antes de que existiera emitMembershipTypeToSync), el delta no
-- baja solo: re-guardarlo una vez desde el form del desktop lo re-emite.
