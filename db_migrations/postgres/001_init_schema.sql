-- Cuadra — Postgres init schema (ADR-002 §3).
-- Idempotent. Apply via `make migrate-postgres` against a fresh DB.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- _migrations
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS _migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- gyms (BC: gyms) — self-referential gym_id = id
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS gyms (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    name            TEXT,
    city            TEXT,
    whatsapp        TEXT,
    country         CHAR(2) NOT NULL DEFAULT 'MX',
    timezone        TEXT NOT NULL DEFAULT 'America/Mexico_City',

    rfc             VARCHAR(13),
    razon_social    TEXT,
    codigo_postal   VARCHAR(5),
    regimen_fiscal  TEXT,

    logo_url        TEXT,
    primary_color   VARCHAR(7),
    secondary_color VARCHAR(7),

    payment_methods JSONB NOT NULL DEFAULT '[]'::jsonb,
    open_time       TIME,
    close_time      TIME,

    subscription_plan       TEXT NOT NULL DEFAULT 'trial',
    trial_ends_at           TIMESTAMPTZ,
    subscription_ends_at    TIMESTAMPTZ,
    subscription_status     TEXT NOT NULL DEFAULT 'active',
    setup_completed_at      TIMESTAMPTZ,

    whatsapp_business_phone     TEXT,
    whatsapp_business_token_enc BYTEA,
    whatsapp_connected_at       TIMESTAMPTZ,

    kiosk_settings  JSONB NOT NULL DEFAULT '{"audio_volume":80,"auto_close_seconds":5}'::jsonb,

    CONSTRAINT chk_gyms_self_ref CHECK (gym_id = id),
    CONSTRAINT chk_gyms_subscription_plan CHECK (subscription_plan IN ('trial','pro_monthly','pro_annual')),
    CONSTRAINT chk_gyms_subscription_status CHECK (subscription_status IN ('active','past_due','cancelled'))
);

CREATE INDEX IF NOT EXISTS idx_gyms_sync ON gyms(updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_gyms_rfc ON gyms(rfc) WHERE rfc IS NOT NULL AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- users (BC: users)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    email                TEXT NOT NULL,
    password_hash        TEXT NOT NULL,
    full_name            TEXT NOT NULL,
    phone                TEXT,
    role                 TEXT NOT NULL,
    active               BOOLEAN NOT NULL DEFAULT TRUE,
    must_change_password BOOLEAN NOT NULL DEFAULT FALSE,
    last_login_at        TIMESTAMPTZ,
    created_by           UUID REFERENCES users(id) ON DELETE RESTRICT,

    CONSTRAINT chk_users_role CHECK (role IN ('owner','operator'))
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_users_email ON users(LOWER(email)) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_users_gym ON users(gym_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_users_sync ON users(gym_id, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_gym_owner ON users(gym_id) WHERE role = 'owner' AND active = TRUE AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- password_reset_tokens (cloud-only, not synced)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS password_reset_tokens (
    id          UUID PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    token_hash  BYTEA NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_password_reset_tokens_user ON password_reset_tokens(user_id, expires_at);

-- ---------------------------------------------------------------------------
-- ownership_transfer_otps (cloud-only, UC-010)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ownership_transfer_otps (
    id          UUID PRIMARY KEY,
    gym_id      UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    from_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    to_user_id  UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    code_hash   BYTEA NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ownership_transfer_otps_gym ON ownership_transfer_otps(gym_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- refresh_token_blacklist (cloud-only, UC-003 logout)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS refresh_token_blacklist (
    token_hash  BYTEA PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_refresh_token_blacklist_user ON refresh_token_blacklist(user_id);
CREATE INDEX IF NOT EXISTS idx_refresh_token_blacklist_exp ON refresh_token_blacklist(expires_at);

-- ---------------------------------------------------------------------------
-- membership_types (BC: members)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS membership_types (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    name                    TEXT NOT NULL,
    price                   NUMERIC(12,2) NOT NULL,
    duration_days           INTEGER NOT NULL,
    enrollment_fee          NUMERIC(12,2) NOT NULL DEFAULT 0,
    maintenance_fee         NUMERIC(12,2) NOT NULL DEFAULT 0,
    maintenance_frequency   TEXT,
    active                  BOOLEAN NOT NULL DEFAULT TRUE,

    CONSTRAINT chk_membership_types_price CHECK (price > 0),
    CONSTRAINT chk_membership_types_duration CHECK (duration_days >= 1),
    CONSTRAINT chk_membership_types_frequency CHECK (
        (maintenance_fee = 0 AND maintenance_frequency IS NULL) OR
        (maintenance_fee > 0 AND maintenance_frequency IN ('monthly','annual'))
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_membership_types_gym_name ON membership_types(gym_id, LOWER(name)) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_membership_types_sync ON membership_types(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- members (BC: members)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS members (
    id          UUID PRIMARY KEY,
    gym_id      UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ,

    folio       TEXT NOT NULL,
    full_name   TEXT NOT NULL,
    phone       TEXT NOT NULL,
    email       TEXT,
    birthdate   DATE,
    photo_url   TEXT,
    notes       TEXT,
    status      TEXT NOT NULL DEFAULT 'active',

    enrollment_paid         BOOLEAN NOT NULL DEFAULT FALSE,
    last_maintenance_paid   DATE,

    pin_hash                TEXT,
    pin_assigned_at         TIMESTAMPTZ,

    last_contact_attempt_at TIMESTAMPTZ,

    created_by              UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    CONSTRAINT chk_members_status CHECK (status IN ('active','inactive','lost'))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_members_gym_folio ON members(gym_id, folio) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_members_gym ON members(gym_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_members_sync ON members(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_members_search_name ON members(gym_id, LOWER(full_name) text_pattern_ops) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_members_search_phone ON members(gym_id, phone) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_members_status ON members(gym_id, status) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- memberships (BC: members)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memberships (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    member_id               UUID NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    membership_type_id      UUID NOT NULL REFERENCES membership_types(id) ON DELETE RESTRICT,

    type_name_snapshot      TEXT NOT NULL,
    price_snapshot          NUMERIC(12,2) NOT NULL,
    duration_days_snapshot  INTEGER NOT NULL,

    start_date      DATE NOT NULL,
    -- expiry_date NULL cuando status = 'pending_payment'.
    expiry_date     DATE,
    status          TEXT NOT NULL DEFAULT 'active',
    replaced_by     UUID REFERENCES memberships(id),

    CONSTRAINT chk_memberships_status CHECK (status IN ('active','expired','replaced','cancelled','pending_payment')),
    CONSTRAINT chk_memberships_dates CHECK (expiry_date IS NULL OR expiry_date >= start_date)
);
CREATE INDEX IF NOT EXISTS idx_memberships_member ON memberships(member_id, status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memberships_gym_expiry ON memberships(gym_id, expiry_date) WHERE status = 'active' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memberships_sync ON memberships(gym_id, updated_at);
-- Un socio sólo puede tener una "slot" vigente o pendiente a la vez.
CREATE UNIQUE INDEX IF NOT EXISTS uq_memberships_member_active ON memberships(member_id) WHERE status IN ('active','pending_payment') AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- membership_adjustments
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS membership_adjustments (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    membership_id   UUID NOT NULL REFERENCES memberships(id) ON DELETE RESTRICT,
    adjusted_by     UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    reason          TEXT NOT NULL,
    days_added      INTEGER NOT NULL,
    previous_expiry DATE NOT NULL,
    new_expiry      DATE NOT NULL,

    CONSTRAINT chk_adjustments_reason_length CHECK (LENGTH(reason) >= 5)
);
CREATE INDEX IF NOT EXISTS idx_membership_adjustments_membership ON membership_adjustments(membership_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_membership_adjustments_sync ON membership_adjustments(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- member_fingerprints (UC-028)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS member_fingerprints (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    member_id           UUID NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    template_encrypted  BYTEA NOT NULL,
    template_format     TEXT NOT NULL DEFAULT 'dp_uareu',
    quality_score       INTEGER,
    registered_by       UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_member_fingerprints_member ON member_fingerprints(member_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_member_fingerprints_sync ON member_fingerprints(gym_id, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_member_fingerprints_member ON member_fingerprints(member_id) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- products (BC: products)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS products (
    id          UUID PRIMARY KEY,
    gym_id      UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ,

    name            TEXT NOT NULL,
    price           NUMERIC(12,2) NOT NULL,
    stock           INTEGER NOT NULL DEFAULT 0,
    stock_minimum   INTEGER NOT NULL DEFAULT 0,
    category        TEXT,
    image_url       TEXT,
    active          BOOLEAN NOT NULL DEFAULT TRUE,

    CONSTRAINT chk_products_price CHECK (price > 0),
    CONSTRAINT chk_products_stock CHECK (stock >= 0)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_products_gym_name ON products(gym_id, LOWER(name)) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_products_gym_active ON products(gym_id, active) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_products_low_stock ON products(gym_id) WHERE stock <= stock_minimum AND active = TRUE AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_products_sync ON products(gym_id, updated_at);

CREATE TABLE IF NOT EXISTS stock_movements (
    id          UUID PRIMARY KEY,
    gym_id      UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ,

    product_id      UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    movement_type   TEXT NOT NULL,
    delta           INTEGER NOT NULL,
    reason          TEXT,
    cost            NUMERIC(12,2),
    sale_item_id    UUID,
    operator_id     UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    CONSTRAINT chk_stock_movements_type CHECK (movement_type IN ('sale','restock','shrinkage','count_correction','refund'))
);
CREATE INDEX IF NOT EXISTS idx_stock_movements_product ON stock_movements(product_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_stock_movements_sync ON stock_movements(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- payments (BC: billing) — append-only
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payments (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    folio                   TEXT NOT NULL,
    member_id               UUID REFERENCES members(id) ON DELETE RESTRICT,
    amount                  NUMERIC(12,2) NOT NULL,
    payment_method          TEXT NOT NULL,
    concept                 TEXT NOT NULL,
    parent_payment_id       UUID REFERENCES payments(id) ON DELETE RESTRICT,

    discount_amount         NUMERIC(12,2) NOT NULL DEFAULT 0,
    discount_reason         TEXT,
    balance_pending         NUMERIC(12,2) NOT NULL DEFAULT 0,
    payment_date            DATE NOT NULL,
    notes                   TEXT,
    operator_id             UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    CONSTRAINT chk_payments_method CHECK (payment_method IN ('cash','transfer','card')),
    CONSTRAINT chk_payments_concept CHECK (concept IN ('membership','product','balance_settlement','refund','other'))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payments_gym_folio ON payments(gym_id, folio) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_payments_member ON payments(member_id, payment_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_payments_gym_date ON payments(gym_id, payment_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_payments_sync ON payments(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_payments_concept ON payments(gym_id, concept, payment_date DESC) WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- sales + sale_items (BC: billing)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sales (
    id          UUID PRIMARY KEY,
    gym_id      UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ,

    payment_id  UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    member_id   UUID REFERENCES members(id) ON DELETE RESTRICT,
    subtotal    NUMERIC(12,2) NOT NULL,
    discount    NUMERIC(12,2) NOT NULL DEFAULT 0,
    total       NUMERIC(12,2) NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_sales_payment ON sales(payment_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_sales_gym_date ON sales(gym_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_sales_sync ON sales(gym_id, updated_at);

CREATE TABLE IF NOT EXISTS sale_items (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    sale_id                 UUID NOT NULL REFERENCES sales(id) ON DELETE RESTRICT,
    product_id              UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    product_name_snapshot   TEXT NOT NULL,
    unit_price_snapshot     NUMERIC(12,2) NOT NULL,
    quantity                INTEGER NOT NULL,
    line_total              NUMERIC(12,2) NOT NULL,

    CONSTRAINT chk_sale_items_quantity CHECK (quantity > 0)
);
CREATE INDEX IF NOT EXISTS idx_sale_items_sale ON sale_items(sale_id);
CREATE INDEX IF NOT EXISTS idx_sale_items_product ON sale_items(product_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_sale_items_sync ON sale_items(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- checkins (BC: checkins)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS checkins (
    id          UUID PRIMARY KEY,
    gym_id      UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ,

    member_id           UUID NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    checkin_at          TIMESTAMPTZ NOT NULL,
    method              TEXT NOT NULL,
    result              TEXT NOT NULL,
    operator_id         UUID REFERENCES users(id) ON DELETE RESTRICT,
    manual_override     BOOLEAN NOT NULL DEFAULT FALSE,
    override_reason     TEXT,

    CONSTRAINT chk_checkins_method CHECK (method IN ('fingerprint','manual','pin')),
    CONSTRAINT chk_checkins_result CHECK (result IN (
        'allowed_active','allowed_expiring_soon','allowed_override',
        'denied_expired','denied_inactive','denied_no_membership','denied_unpaid_enrollment'
    ))
);
CREATE INDEX IF NOT EXISTS idx_checkins_member_date ON checkins(member_id, checkin_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_checkins_gym_date ON checkins(gym_id, checkin_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_checkins_sync ON checkins(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- contact_attempts (UC-035)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS contact_attempts (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    member_id       UUID NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    attempt_at      TIMESTAMPTZ NOT NULL,
    channel         TEXT,
    note            TEXT,
    contacted_by    UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    CONSTRAINT chk_contact_attempts_channel CHECK (channel IN ('whatsapp','phone','in_person','other'))
);
CREATE INDEX IF NOT EXISTS idx_contact_attempts_member ON contact_attempts(member_id, attempt_at DESC);
CREATE INDEX IF NOT EXISTS idx_contact_attempts_sync ON contact_attempts(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- cash_close_events (UC-027)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS cash_close_events (
    id          UUID PRIMARY KEY,
    gym_id      UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ,

    close_date          DATE NOT NULL,
    calculated_cash     NUMERIC(12,2) NOT NULL,
    counted_cash        NUMERIC(12,2),
    discrepancy         NUMERIC(12,2) GENERATED ALWAYS AS (counted_cash - calculated_cash) STORED,
    discrepancy_reason  TEXT,
    closed_by           UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_cash_close_events_gym_date ON cash_close_events(gym_id, close_date DESC);
CREATE INDEX IF NOT EXISTS idx_cash_close_events_sync ON cash_close_events(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- gym_ownership_transfers (UC-010)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS gym_ownership_transfers (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    from_user_id    UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    to_user_id      UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    executed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_gym_ownership_transfers_gym ON gym_ownership_transfers(gym_id, executed_at DESC);
CREATE INDEX IF NOT EXISTS idx_gym_ownership_transfers_sync ON gym_ownership_transfers(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- audit_log
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_log (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    entity_type     TEXT NOT NULL,
    entity_id       UUID NOT NULL,
    action          TEXT NOT NULL,
    actor_user_id   UUID REFERENCES users(id) ON DELETE RESTRICT,
    changes         JSONB,
    ip_address      INET,
    user_agent      TEXT,

    CONSTRAINT chk_audit_log_action CHECK (LENGTH(action) > 0)
);
CREATE INDEX IF NOT EXISTS idx_audit_log_gym_time ON audit_log(gym_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_log_entity ON audit_log(gym_id, entity_type, entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_log_actor ON audit_log(actor_user_id, created_at DESC) WHERE actor_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_audit_log_sync ON audit_log(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- conflict_log (cloud-only)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS conflict_log (
    id                BIGSERIAL PRIMARY KEY,
    gym_id            UUID NOT NULL,
    entity_type       TEXT NOT NULL,
    entity_id         UUID NOT NULL,
    client_id         UUID NOT NULL,
    client_version    INTEGER NOT NULL,
    server_version    INTEGER NOT NULL,
    client_payload    JSONB NOT NULL,
    server_payload    JSONB NOT NULL,
    resolution        TEXT NOT NULL,
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_conflict_log_resolution CHECK (resolution IN ('server_wins','client_wins'))
);
CREATE INDEX IF NOT EXISTS idx_conflict_log_gym ON conflict_log(gym_id, detected_at DESC);

-- ---------------------------------------------------------------------------
-- notification_queue (sync'd)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS notification_queue (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    channel             TEXT NOT NULL,
    template_key        TEXT NOT NULL,
    recipient_type      TEXT NOT NULL,
    recipient_id        UUID NOT NULL,
    recipient_address   TEXT NOT NULL,
    payload             JSONB NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending',
    sent_at             TIMESTAMPTZ,
    failed_at           TIMESTAMPTZ,
    error_message       TEXT,
    retry_count         INTEGER NOT NULL DEFAULT 0,
    scheduled_for       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_notification_queue_channel CHECK (channel IN ('whatsapp','email','in_app')),
    CONSTRAINT chk_notification_queue_status CHECK (status IN ('pending','sent','failed','cancelled')),
    CONSTRAINT chk_notification_queue_recipient_type CHECK (recipient_type IN ('member','user'))
);
CREATE INDEX IF NOT EXISTS idx_notification_queue_pending ON notification_queue(scheduled_for, status) WHERE status = 'pending' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notification_queue_gym ON notification_queue(gym_id, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_notification_queue_sync ON notification_queue(gym_id, updated_at);

-- ---------------------------------------------------------------------------
-- gym_keys (cloud-only, ADR-006 §2.3)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS gym_keys (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL UNIQUE REFERENCES gyms(id) ON DELETE RESTRICT,
    encrypted_gmk   BYTEA NOT NULL,
    smk_version     INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO _migrations (version, name) VALUES (1, '001_init_schema')
ON CONFLICT (version) DO NOTHING;

COMMIT;
