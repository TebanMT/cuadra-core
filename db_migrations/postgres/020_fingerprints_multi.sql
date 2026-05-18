-- Migration 020 — múltiples huellas por socio (UC-028 best-of-3).
--
-- El enrollment ahora guarda hasta 3 templates del mismo dedo por socio
-- (matching best-of-N en el checkin). El índice ÚNICO
-- uq_member_fingerprints_member imponía "1 huella por socio" del diseño
-- original — se reemplaza por uno NO único que conserva la búsqueda
-- rápida por member_id.

BEGIN;

DROP INDEX IF EXISTS uq_member_fingerprints_member;

CREATE INDEX IF NOT EXISTS idx_member_fingerprints_member
    ON member_fingerprints(member_id) WHERE deleted_at IS NULL;

INSERT INTO _migrations (version, name) VALUES (20, '020_fingerprints_multi')
ON CONFLICT (version) DO NOTHING;

COMMIT;
