-- Migration 018 — UNIQUE constraint para gyms.whatsapp.
-- Espeja sqlite/015_gyms_whatsapp_unique.sql.
--
-- Por qué: cuando el dueño conecta WhatsApp Business como canal de
-- notificaciones, Twilio/Meta sólo permiten un sender por número telefónico.
-- Si dos gyms se registraron con el mismo whatsapp, el segundo iba a
-- explotar al intentar conectar — semanas después del signup, sin contexto
-- claro para el dueño. Mejor rechazarlo de entrada con un mensaje claro
-- en el wizard.
--
-- Estrategia para gyms duplicados existentes:
--   1) Identificar grupos por mismo `whatsapp` (NOT NULL, NOT soft-deleted).
--   2) Soft-delete (set deleted_at) al MÁS RECIENTE de cada grupo,
--      preservando el más antiguo. Razones:
--        - Soft delete es la regla del proyecto (CLAUDE.md).
--        - Preserva audit_log y referencias FK que apuntan al gym.
--        - El índice partial WHERE deleted_at IS NULL libera la unicidad,
--          así que el efecto práctico es el mismo que un DELETE.
--   3) Crear el índice UNIQUE partial.

BEGIN;

-- Paso 1+2: soft-delete duplicados (preservando el más viejo de cada grupo).
WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (PARTITION BY whatsapp ORDER BY created_at ASC, id ASC) AS rn
    FROM gyms
    WHERE whatsapp IS NOT NULL
      AND deleted_at IS NULL
)
UPDATE gyms
   SET deleted_at = NOW(),
       updated_at = NOW(),
       version    = version + 1
 WHERE id IN (SELECT id FROM ranked WHERE rn > 1);

-- Paso 3: índice UNIQUE partial, mismo patrón que uq_gyms_rfc.
CREATE UNIQUE INDEX IF NOT EXISTS uq_gyms_whatsapp
    ON gyms(whatsapp)
    WHERE whatsapp IS NOT NULL AND deleted_at IS NULL;

INSERT INTO _migrations (version, name) VALUES (18, '018_gyms_whatsapp_unique')
ON CONFLICT (version) DO NOTHING;

COMMIT;
