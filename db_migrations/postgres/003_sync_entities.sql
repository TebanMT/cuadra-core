-- Migration 003 — sync_entities (cloud-only, ADR-001 Sesión 8)
--
-- This is the canonical store for sync-protocol mirrors of every entity
-- pushed from a sidecar. The sidecar's `sync_queue` snapshots land here
-- via POST /api/v1/sync/push, and GET /sync/pull / /sync/full read from
-- here exclusively. Domain tables on the cloud (members, payments…)
-- continue to be populated by cloud-side handlers (signup, login, …);
-- bridging sidecar-pushed data into those domain tables is a follow-up
-- that will live in cuadra-core's read model layer (Sesión 6+).
--
-- Why a generic table:
--   * Single code path for push/pull/full regardless of entity type.
--   * Forward-compat: new entity types do not require new SQL.
--   * Idempotent UPSERT trivially expressed with ON CONFLICT.
--
-- Shape mirrors ADR-001 §3.1 contract — `version`, `server_updated_at`,
-- `deleted_at` are the LWW dimensions.

BEGIN;

CREATE TABLE IF NOT EXISTS sync_entities (
    gym_id              UUID NOT NULL,
    entity_type         TEXT NOT NULL,
    entity_id           UUID NOT NULL,
    version             INTEGER NOT NULL,
    payload             JSONB NOT NULL,
    server_updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,
    PRIMARY KEY (gym_id, entity_type, entity_id)
);

CREATE INDEX IF NOT EXISTS idx_sync_entities_pull
    ON sync_entities(gym_id, server_updated_at);

CREATE INDEX IF NOT EXISTS idx_sync_entities_type_pull
    ON sync_entities(gym_id, entity_type, server_updated_at);

INSERT INTO _migrations (version, name) VALUES (3, '003_sync_entities')
ON CONFLICT (version) DO NOTHING;

COMMIT;
