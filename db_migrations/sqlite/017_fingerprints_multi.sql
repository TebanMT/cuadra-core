-- Migration 017 — múltiples huellas por socio (UC-028 best-of-3).
--
-- El enrollment ahora guarda hasta 3 templates del mismo dedo por socio
-- (matching best-of-N en el checkin). El índice ÚNICO
-- uq_member_fingerprints_member imponía "1 huella por socio" del diseño
-- original y hacía fallar el segundo INSERT del batch — se reemplaza por
-- uno NO único que conserva la búsqueda rápida por member_id.

BEGIN;

DROP INDEX IF EXISTS uq_member_fingerprints_member;

CREATE INDEX IF NOT EXISTS idx_member_fingerprints_member
    ON member_fingerprints(member_id) WHERE deleted_at IS NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 17, '017_fingerprints_multi', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 17);

COMMIT;
