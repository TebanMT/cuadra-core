-- UC-001: aceptación de los Términos del Servicio y del Aviso de Privacidad
-- al crear la cuenta del dueño. Cloud only: el alta siempre pasa por el
-- cloud (el sidecar la reenvía). Evidencia append-only: quién, qué
-- documento, qué versión, cuándo y desde dónde. Nunca se actualiza ni se
-- borra; una versión nueva de un documento es una fila nueva.
BEGIN;

CREATE TABLE IF NOT EXISTS legal_acceptances (
    id          UUID PRIMARY KEY,
    gym_id      UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    document    TEXT NOT NULL CHECK (document IN ('terms', 'privacy')),
    version     TEXT NOT NULL CHECK (length(version) BETWEEN 1 AND 32),
    accepted_at TIMESTAMPTZ NOT NULL,
    ip_address  TEXT CHECK (ip_address IS NULL OR length(ip_address) <= 64),
    user_agent  TEXT CHECK (user_agent IS NULL OR length(user_agent) <= 512),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_legal_acceptances_user_doc_version UNIQUE (user_id, document, version)
);
CREATE INDEX IF NOT EXISTS idx_legal_acceptances_gym
    ON legal_acceptances(gym_id, accepted_at DESC);

-- Misma política de tenant que 057 para toda tabla con gym_id: la API
-- se niega a arrancar si una tabla así no tiene RLS.
ALTER TABLE legal_acceptances ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tinta_tenant ON legal_acceptances;
CREATE POLICY tinta_tenant ON legal_acceptances
    USING (gym_id = NULLIF(current_setting('tinta.gym_id', true), '')::uuid)
    WITH CHECK (gym_id = NULLIF(current_setting('tinta.gym_id', true), '')::uuid);

INSERT INTO _migrations(version, name)
VALUES (69, '069_legal_acceptances')
ON CONFLICT (version) DO NOTHING;

COMMIT;
