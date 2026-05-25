-- Migration 026 — tabla `releases` para auto-updates de Tinta Desktop
-- (ADR-005 §2.1). Cloud-only: el sidecar no la sincroniza, sólo la
-- consume vía GET /api/v1/releases/latest/:target/:current_version.
--
-- Una fila por (version, target_platform). Múltiples plataformas del
-- mismo release coexisten — el handler matchea por target.
--
-- rollout_percent (0..100) implementa staged rollout determinista por
-- gym_id (ADR-005 §2.4). 100 = release general.
-- force_immediate=true marca security-critical: Tauri lo aplica con
-- restart forzado en 5 min (ADR-005 §2.7).
-- channel: 'stable' por default. Selector beta/stable diferido post-v1
-- pero el campo vive desde día 1 para no migrar después.

BEGIN;

CREATE TABLE IF NOT EXISTS releases (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version          VARCHAR(32)  NOT NULL,
    target_platform  VARCHAR(64)  NOT NULL,
    channel          VARCHAR(16)  NOT NULL DEFAULT 'stable',
    url              TEXT         NOT NULL,
    signature_ed25519 TEXT        NOT NULL,
    pub_date         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    notes            TEXT         NOT NULL DEFAULT '',
    rollout_percent  SMALLINT     NOT NULL DEFAULT 0,
    force_immediate  BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ
);

-- Cada (version, target_platform, channel) es único — un release-target
-- vive una sola vez. Si hay que re-emitir tras un cambio (raro), se
-- soft-deletea la fila vieja y se inserta una nueva versión bumped.
CREATE UNIQUE INDEX IF NOT EXISTS uq_releases_version_target_channel
    ON releases (version, target_platform, channel)
    WHERE deleted_at IS NULL;

-- Index principal del handler: pub_date DESC para "última versión por
-- target/channel". El WHERE en el index se aplica al query — Postgres
-- evita escanear filas soft-deleted.
CREATE INDEX IF NOT EXISTS idx_releases_target_pubdate
    ON releases (target_platform, channel, pub_date DESC)
    WHERE deleted_at IS NULL;

ALTER TABLE releases DROP CONSTRAINT IF EXISTS chk_releases_rollout;
ALTER TABLE releases ADD CONSTRAINT chk_releases_rollout
    CHECK (rollout_percent BETWEEN 0 AND 100);

ALTER TABLE releases DROP CONSTRAINT IF EXISTS chk_releases_channel;
ALTER TABLE releases ADD CONSTRAINT chk_releases_channel
    CHECK (channel IN ('stable','beta'));

INSERT INTO _migrations (version, name) VALUES (26, '026_releases')
ON CONFLICT (version) DO NOTHING;

COMMIT;
