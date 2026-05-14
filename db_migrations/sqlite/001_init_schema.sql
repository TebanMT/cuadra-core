-- Cuadra — SQLite init schema (ADR-002 §3, sidecar local).
-- Idempotent. Apply via `make migrate-sqlite`.
-- Type mapping (vs Postgres):
--   UUID                   -> TEXT(36)
--   TIMESTAMPTZ            -> INTEGER (epoch ms UTC)
--   NUMERIC(12,2)          -> INTEGER (centavos)
--   BOOLEAN                -> INTEGER (0/1)
--   JSONB                  -> TEXT (CHECK json_valid)
--   BYTEA                  -> BLOB
-- All sync'd tables include `synced_at INTEGER NULL`.
--
-- Notes:
--   * cloud-only tables (password_reset_tokens, conflict_log, gym_keys,
--     ownership_transfer_otps, refresh_token_blacklist) are NOT mirrored here.
--   * sync_queue + sync_state are sidecar-only (cloud doesn't need them).

PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;

BEGIN;

CREATE TABLE IF NOT EXISTS _migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at INTEGER NOT NULL
);

-- ---------------------------------------------------------------------------
-- gyms
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS gyms (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    name            TEXT,
    city            TEXT,
    whatsapp        TEXT,
    country         TEXT NOT NULL DEFAULT 'MX',
    timezone        TEXT NOT NULL DEFAULT 'America/Mexico_City',

    rfc             TEXT,
    razon_social    TEXT,
    codigo_postal   TEXT,
    regimen_fiscal  TEXT,

    logo_url        TEXT,
    primary_color   TEXT,
    secondary_color TEXT,

    payment_methods TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(payment_methods)),
    open_time       TEXT,
    close_time      TEXT,

    subscription_plan       TEXT NOT NULL DEFAULT 'trial' CHECK (subscription_plan IN ('trial','pro_monthly','pro_annual')),
    trial_ends_at           INTEGER,
    subscription_ends_at    INTEGER,
    subscription_status     TEXT NOT NULL DEFAULT 'active' CHECK (subscription_status IN ('active','past_due','cancelled')),
    setup_completed_at      INTEGER,

    whatsapp_business_phone     TEXT,
    whatsapp_business_token_enc BLOB,
    whatsapp_connected_at       INTEGER,

    kiosk_settings  TEXT NOT NULL DEFAULT '{"audio_volume":80,"auto_close_seconds":5}' CHECK (json_valid(kiosk_settings)),

    CHECK (gym_id = id)
);
CREATE INDEX IF NOT EXISTS idx_gyms_sync ON gyms(updated_at);
CREATE INDEX IF NOT EXISTS idx_gyms_sync_pending ON gyms(synced_at) WHERE synced_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_gyms_rfc ON gyms(rfc COLLATE NOCASE) WHERE rfc IS NOT NULL AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    email                TEXT NOT NULL,
    password_hash        TEXT NOT NULL,
    full_name            TEXT NOT NULL,
    phone                TEXT,
    role                 TEXT NOT NULL CHECK (role IN ('owner','operator')),
    active               INTEGER NOT NULL DEFAULT 1,
    must_change_password INTEGER NOT NULL DEFAULT 0,
    last_login_at        INTEGER,
    created_by           TEXT REFERENCES users(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_email ON users(email COLLATE NOCASE) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_users_gym ON users(gym_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_users_sync ON users(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_users_sync_pending ON users(synced_at) WHERE synced_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_gym_owner ON users(gym_id) WHERE role = 'owner' AND active = 1 AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- membership_types
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS membership_types (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    name                    TEXT NOT NULL,
    price                   INTEGER NOT NULL CHECK (price > 0),  -- cents
    duration_days           INTEGER NOT NULL CHECK (duration_days >= 1),
    enrollment_fee          INTEGER NOT NULL DEFAULT 0,
    maintenance_fee         INTEGER NOT NULL DEFAULT 0,
    maintenance_frequency   TEXT,
    active                  INTEGER NOT NULL DEFAULT 1,

    CHECK (
        (maintenance_fee = 0 AND maintenance_frequency IS NULL) OR
        (maintenance_fee > 0 AND maintenance_frequency IN ('monthly','annual'))
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_membership_types_gym_name ON membership_types(gym_id, name COLLATE NOCASE) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_membership_types_sync ON membership_types(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_membership_types_sync_pending ON membership_types(synced_at) WHERE synced_at IS NULL;

-- ---------------------------------------------------------------------------
-- members
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS members (
    id          TEXT PRIMARY KEY,
    gym_id      TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    deleted_at  INTEGER,
    synced_at   INTEGER,

    folio       TEXT NOT NULL,
    full_name   TEXT NOT NULL,
    phone       TEXT NOT NULL,
    email       TEXT,
    birthdate   TEXT,
    photo_url   TEXT,
    notes       TEXT,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive','lost')),

    enrollment_paid         INTEGER NOT NULL DEFAULT 0,
    last_maintenance_paid   TEXT,

    pin_hash                TEXT,
    pin_plain               TEXT,
    pin_assigned_at         INTEGER,

    last_contact_attempt_at INTEGER,

    created_by              TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_members_gym_folio ON members(gym_id, folio) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_members_gym ON members(gym_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_members_sync ON members(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_members_sync_pending ON members(synced_at) WHERE synced_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_members_search_name ON members(gym_id, full_name COLLATE NOCASE) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_members_search_phone ON members(gym_id, phone) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_members_status ON members(gym_id, status) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- memberships
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memberships (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    member_id               TEXT NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    membership_type_id      TEXT NOT NULL REFERENCES membership_types(id) ON DELETE RESTRICT,

    type_name_snapshot      TEXT NOT NULL,
    price_snapshot          INTEGER NOT NULL,
    duration_days_snapshot  INTEGER NOT NULL,

    start_date      TEXT NOT NULL,
    -- expiry_date null cuando status = 'pending_payment' (socio inscrito
    -- pero sin pago todavía — la vigencia se calcula al recibir el
    -- primer abono, parcial o completo).
    expiry_date     TEXT,
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','expired','replaced','cancelled','pending_payment')),
    replaced_by     TEXT REFERENCES memberships(id),

    CHECK (expiry_date IS NULL OR expiry_date >= start_date)
);
CREATE INDEX IF NOT EXISTS idx_memberships_member ON memberships(member_id, status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memberships_gym_expiry ON memberships(gym_id, expiry_date) WHERE status = 'active' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memberships_sync ON memberships(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_memberships_sync_pending ON memberships(synced_at) WHERE synced_at IS NULL;
-- Un socio sólo puede tener UNA membresía vigente o pendiente a la vez.
-- Antes era sólo 'active'; ahora incluimos 'pending_payment' porque
-- semánticamente es la misma "slot" — un socio no puede estar inscrito
-- en dos planes simultáneamente, esté pagado o no.
CREATE UNIQUE INDEX IF NOT EXISTS uq_memberships_member_active ON memberships(member_id) WHERE status IN ('active','pending_payment') AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- membership_adjustments
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS membership_adjustments (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    membership_id   TEXT NOT NULL REFERENCES memberships(id) ON DELETE RESTRICT,
    adjusted_by     TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    reason          TEXT NOT NULL CHECK (length(reason) >= 5),
    days_added      INTEGER NOT NULL,
    previous_expiry TEXT NOT NULL,
    new_expiry      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_membership_adjustments_membership ON membership_adjustments(membership_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_membership_adjustments_sync ON membership_adjustments(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- member_fingerprints
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS member_fingerprints (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    member_id           TEXT NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    template_encrypted  BLOB NOT NULL,
    template_format     TEXT NOT NULL DEFAULT 'dp_uareu',
    quality_score       INTEGER,
    registered_by       TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_member_fingerprints_member ON member_fingerprints(member_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_member_fingerprints_sync ON member_fingerprints(gym_id, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_member_fingerprints_member ON member_fingerprints(member_id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- products
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS products (
    id          TEXT PRIMARY KEY,
    gym_id      TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    deleted_at  INTEGER,
    synced_at   INTEGER,

    name            TEXT NOT NULL,
    price           INTEGER NOT NULL CHECK (price > 0),
    stock           INTEGER NOT NULL DEFAULT 0 CHECK (stock >= 0),
    stock_minimum   INTEGER NOT NULL DEFAULT 0,
    category        TEXT,
    image_url       TEXT,
    active          INTEGER NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_products_gym_name ON products(gym_id, name COLLATE NOCASE) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_products_gym_active ON products(gym_id, active) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_products_sync ON products(gym_id, updated_at);

CREATE TABLE IF NOT EXISTS stock_movements (
    id          TEXT PRIMARY KEY,
    gym_id      TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    deleted_at  INTEGER,
    synced_at   INTEGER,

    product_id      TEXT NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    movement_type   TEXT NOT NULL CHECK (movement_type IN ('sale','restock','shrinkage','count_correction','refund')),
    delta           INTEGER NOT NULL,
    reason          TEXT,
    cost            INTEGER,
    sale_item_id    TEXT,
    operator_id     TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_stock_movements_product ON stock_movements(product_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_stock_movements_sync ON stock_movements(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- payments
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payments (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    folio                   TEXT NOT NULL,
    member_id               TEXT REFERENCES members(id) ON DELETE RESTRICT,
    amount                  INTEGER NOT NULL,
    payment_method          TEXT NOT NULL CHECK (payment_method IN ('cash','transfer','card')),
    concept                 TEXT NOT NULL CHECK (concept IN ('membership','product','balance_settlement','refund','other')),
    parent_payment_id       TEXT REFERENCES payments(id) ON DELETE RESTRICT,

    discount_amount         INTEGER NOT NULL DEFAULT 0,
    discount_reason         TEXT,
    balance_pending         INTEGER NOT NULL DEFAULT 0,
    payment_date            TEXT NOT NULL,
    notes                   TEXT,
    breakdown               TEXT,
    operator_id             TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payments_gym_folio ON payments(gym_id, folio) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_payments_member ON payments(member_id, payment_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_payments_gym_date ON payments(gym_id, payment_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_payments_sync ON payments(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- sales / sale_items
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sales (
    id          TEXT PRIMARY KEY,
    gym_id      TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    deleted_at  INTEGER,
    synced_at   INTEGER,

    payment_id  TEXT NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    member_id   TEXT REFERENCES members(id) ON DELETE RESTRICT,
    subtotal    INTEGER NOT NULL,
    discount    INTEGER NOT NULL DEFAULT 0,
    total       INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_sales_payment ON sales(payment_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_sales_gym_date ON sales(gym_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_sales_sync ON sales(gym_id, updated_at);

CREATE TABLE IF NOT EXISTS sale_items (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    sale_id                 TEXT NOT NULL REFERENCES sales(id) ON DELETE RESTRICT,
    product_id              TEXT NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    product_name_snapshot   TEXT NOT NULL,
    unit_price_snapshot     INTEGER NOT NULL,
    quantity                INTEGER NOT NULL CHECK (quantity > 0),
    line_total              INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sale_items_sale ON sale_items(sale_id);
CREATE INDEX IF NOT EXISTS idx_sale_items_sync ON sale_items(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- checkins
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS checkins (
    id          TEXT PRIMARY KEY,
    gym_id      TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    deleted_at  INTEGER,
    synced_at   INTEGER,

    member_id           TEXT NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    checkin_at          INTEGER NOT NULL,
    method              TEXT NOT NULL CHECK (method IN ('fingerprint','manual','pin')),
    result              TEXT NOT NULL CHECK (result IN ('allowed_active','allowed_expiring_soon','allowed_override','denied_expired','denied_inactive','denied_no_membership','denied_unpaid_enrollment')),
    operator_id         TEXT REFERENCES users(id) ON DELETE RESTRICT,
    manual_override     INTEGER NOT NULL DEFAULT 0,
    override_reason     TEXT
);
CREATE INDEX IF NOT EXISTS idx_checkins_member_date ON checkins(member_id, checkin_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_checkins_gym_date ON checkins(gym_id, checkin_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_checkins_sync ON checkins(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- contact_attempts
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS contact_attempts (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    member_id       TEXT NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    attempt_at      INTEGER NOT NULL,
    channel         TEXT CHECK (channel IN ('whatsapp','phone','in_person','other')),
    note            TEXT,
    contacted_by    TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_contact_attempts_member ON contact_attempts(member_id, attempt_at DESC);
CREATE INDEX IF NOT EXISTS idx_contact_attempts_sync ON contact_attempts(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- cash_close_events
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cash_close_events (
    id          TEXT PRIMARY KEY,
    gym_id      TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    deleted_at  INTEGER,
    synced_at   INTEGER,

    close_date          TEXT NOT NULL,
    calculated_cash     INTEGER NOT NULL,
    counted_cash        INTEGER,
    discrepancy         INTEGER GENERATED ALWAYS AS (counted_cash - calculated_cash) STORED,
    discrepancy_reason  TEXT,
    closed_by           TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_cash_close_events_gym_date ON cash_close_events(gym_id, close_date DESC);
CREATE INDEX IF NOT EXISTS idx_cash_close_events_sync ON cash_close_events(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- gym_ownership_transfers
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS gym_ownership_transfers (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    from_user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    to_user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    executed_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_gym_ownership_transfers_gym ON gym_ownership_transfers(gym_id, executed_at DESC);
CREATE INDEX IF NOT EXISTS idx_gym_ownership_transfers_sync ON gym_ownership_transfers(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- audit_log
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_log (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    entity_type     TEXT NOT NULL,
    entity_id       TEXT NOT NULL,
    action          TEXT NOT NULL CHECK (length(action) > 0),
    actor_user_id   TEXT,
    changes         TEXT CHECK (changes IS NULL OR json_valid(changes)),
    ip_address      TEXT,
    user_agent      TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_log_gym_time ON audit_log(gym_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_log_entity ON audit_log(gym_id, entity_type, entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_log_sync ON audit_log(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- notification_queue
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS notification_queue (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    channel             TEXT NOT NULL CHECK (channel IN ('whatsapp','email','in_app')),
    template_key        TEXT NOT NULL,
    recipient_type      TEXT NOT NULL CHECK (recipient_type IN ('member','user')),
    recipient_id        TEXT NOT NULL,
    recipient_address   TEXT NOT NULL,
    payload             TEXT NOT NULL CHECK (json_valid(payload)),
    status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed','cancelled')),
    sent_at             INTEGER,
    failed_at           INTEGER,
    error_message       TEXT,
    retry_count         INTEGER NOT NULL DEFAULT 0,
    scheduled_for       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notification_queue_pending ON notification_queue(scheduled_for, status) WHERE status = 'pending' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notification_queue_gym ON notification_queue(gym_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notification_queue_sync ON notification_queue(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- sync_queue (sidecar-only, ADR-001 §3.2)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sync_queue (
    id              TEXT PRIMARY KEY,
    entity_type     TEXT NOT NULL,
    entity_id       TEXT NOT NULL,
    operation       TEXT NOT NULL CHECK (operation IN ('upsert','delete')),
    payload         TEXT NOT NULL CHECK (json_valid(payload)),
    client_version  INTEGER NOT NULL,
    enqueued_at     INTEGER NOT NULL,
    synced_at       INTEGER,
    retry_count     INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT
);
CREATE INDEX IF NOT EXISTS idx_sync_queue_pending ON sync_queue(synced_at) WHERE synced_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_sync_queue_entity ON sync_queue(entity_type, entity_id);

-- ---------------------------------------------------------------------------
-- sync_state
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sync_state (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  INTEGER NOT NULL
);

INSERT INTO _migrations (version, name, applied_at)
SELECT 1, '001_init_schema', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 1);

COMMIT;
