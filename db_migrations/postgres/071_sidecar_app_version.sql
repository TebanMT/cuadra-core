-- Migration 071 — app_version en sidecar_credentials (cloud-only)
--
-- Hasta hoy el cloud no sabía qué versión de Tinta Desktop corre cada
-- gym: el update-check de Tauri la recibe en la URL pero no persiste
-- nada, y el sync tampoco la mandaba. Soporte lo necesita para saber
-- quién está atrás cuando se reporta un bug ya corregido.
--
-- El sidecar ahora manda `X-Tinta-App-Version` en push/pull/full y el
-- middleware de sidecar-auth la guarda junto al touch de last_seen_at
-- (mismo fire-and-forget; sin requests extra). NULL = credencial que
-- todavía no sincroniza con un build que mande el header.

BEGIN;

ALTER TABLE sidecar_credentials
    ADD COLUMN IF NOT EXISTS app_version TEXT;

INSERT INTO _migrations(version, name)
VALUES (71, '071_sidecar_app_version')
ON CONFLICT (version) DO NOTHING;

COMMIT;
