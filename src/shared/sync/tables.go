package sync

// EntityTable describes one synced entity for protocol purposes:
//
//   - Type: the wire `entity_type` (members, payments, ...).
//   - Table: the SQL table name on both Postgres and SQLite (they match
//     exactly per ADR-002).
//   - Columns: full list of column names. The wire payload is a JSON
//     object whose keys are exactly these names. JSON-extras the receiver
//     doesn't recognise are dropped silently (forward-compat).
//
// The list omits `synced_at` — that column is sidecar-only state and is
// never sent over the wire (servers don't have it; sidecars set it
// locally after a successful push).
type EntityTable struct {
	Type    string
	Table   string
	Columns []string
	// CompositeKey, when non-empty, names the natural-key columns the
	// projector and store use instead of the implicit `id`. Tables with
	// no surrogate id (e.g. owner_alert_configs keyed by
	// (gym_id, alert_key)) declare it here so the upsert hits ON CONFLICT
	// (col, ...) and the wire `entity_id` is treated as a composed string
	// rather than a UUID. Empty means "use id" — every legacy table.
	CompositeKey []string
}

// SyncedTables is the canonical, ordered registry of every entity that
// participates in sync. Order is **topological** per ADR-001 §3.5: a
// table appears strictly after every table it has a foreign key into.
// Full-sync emits chunks in this order so the client can replay without
// FK violations.
//
// The order also implicitly defines push-side processing in the rare
// case where a single batch contains items from multiple tables — we
// process in registry order so dependents land last.
var SyncedTables = []EntityTable{
	{
		Type:  "gyms",
		Table: "gyms",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"name", "city", "whatsapp", "country", "timezone",
			"rfc", "razon_social", "codigo_postal", "regimen_fiscal",
			"logo_url", "primary_color", "secondary_color",
			"payment_methods", "open_time", "close_time",
			"subscription_plan", "trial_ends_at", "subscription_ends_at",
			"subscription_status", "stripe_customer_id", "setup_completed_at",
			"whatsapp_business_phone", "whatsapp_business_token_enc", "whatsapp_connected_at",
			"kiosk_settings", "charge_settings",
		},
	},
	{
		Type:  "users",
		Table: "users",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"email", "password_hash", "full_name", "phone", "role",
			"active", "must_change_password", "last_login_at", "created_by",
			// pin_hash + pin_assigned_at: the desktop's /auth/login-pin
			// validates offline against the local bcrypt hash, so both
			// columns must round-trip via the projector. The hash is
			// already opaque so we don't add a separate encryption
			// layer here (same posture as password_hash above).
			"pin_hash", "pin_assigned_at",
		},
	},
	{
		Type:  "membership_types",
		Table: "membership_types",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"name", "price", "duration_days", "duration_months", "enrollment_fee",
			"maintenance_fee", "maintenance_frequency", "active",
		},
	},
	{
		Type:  "products",
		Table: "products",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"name", "price", "stock", "stock_minimum", "category", "image_url", "active",
		},
	},
	{
		Type:  "members",
		Table: "members",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"folio", "full_name", "phone", "email", "birthdate",
			"photo_url", "notes", "status",
			"enrollment_paid", "last_maintenance_paid",
			// pin_plain travels alongside pin_hash because the operator
			// surfaces it in the member detail page (4-digit convenience
			// code, not a secret — see migration 012 rationale).
			"pin_hash", "pin_plain", "pin_assigned_at",
			"last_contact_attempt_at", "gender", "created_by",
		},
	},
	{
		Type:  "memberships",
		Table: "memberships",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"member_id", "membership_type_id",
			"type_name_snapshot", "price_snapshot",
			"duration_days_snapshot", "duration_months_snapshot",
			"start_date", "expiry_date", "status", "replaced_by",
		},
	},
	{
		Type:  "membership_adjustments",
		Table: "membership_adjustments",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"membership_id", "adjusted_by", "reason",
			"days_added", "previous_expiry", "new_expiry",
		},
	},
	{
		Type:  "member_fingerprints",
		Table: "member_fingerprints",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"member_id", "template_encrypted", "template_format",
			"quality_score", "registered_by",
		},
	},
	{
		// promotions — catálogo del gym. Topológicamente va después de
		// membership_types (no FK directa pero "vive en la misma capa"
		// — el operador las administra desde la misma sección de UI) y
		// estrictamente ANTES de applied_promotions (que FK a esta tabla).
		Type:  "promotions",
		Table: "promotions",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"name", "description", "kind", "value", "buy_n", "companion_count",
			"applies_to", "code", "valid_from", "valid_until",
			"max_uses_total", "max_uses_per_member", "active",
		},
	},
	{
		Type:  "payments",
		Table: "payments",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"folio", "member_id", "amount", "payment_method", "concept",
			"parent_payment_id", "discount_amount", "discount_reason",
			"balance_pending", "payment_date", "notes", "breakdown", "operator_id",
		},
	},
	{
		Type:  "sales",
		Table: "sales",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"payment_id", "member_id", "subtotal", "discount", "total",
		},
	},
	{
		Type:  "sale_items",
		Table: "sale_items",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"sale_id", "product_id", "product_name_snapshot",
			"unit_price_snapshot", "quantity", "line_total",
		},
	},
	{
		// applied_promotions — FK a promotions, payments, members, users.
		// Va después de sale_items (último dependiente de payments en el
		// orden topológico actual) para garantizar que en un full-sync
		// el cliente reciba primero la promo y el payment, después la
		// aplicación. El proyector genérico maneja los upserts; FKs
		// `ON DELETE RESTRICT` impiden borrar la promo si quedaron
		// aplicaciones.
		Type:  "applied_promotions",
		Table: "applied_promotions",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"promotion_id", "payment_id", "member_id", "applied_by_user_id",
			"promotion_name_snapshot", "kind_snapshot", "value_snapshot",
			"discount_amount", "extra_days_applied", "notes",
		},
	},
	{
		Type:  "stock_movements",
		Table: "stock_movements",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"product_id", "movement_type", "delta", "reason", "cost",
			"sale_item_id", "operator_id",
		},
	},
	{
		// expenses — gastos generales del gym (renta, servicios, etc.).
		// Topológicamente va después de users porque created_by referencia
		// a users(id). No tiene FKs hacia products / sales.
		Type:  "expenses",
		Table: "expenses",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"expense_date", "amount", "category", "description",
			"payment_method", "created_by",
		},
	},
	{
		Type:  "checkins",
		Table: "checkins",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"member_id", "checkin_at", "method", "result",
			"operator_id", "manual_override", "override_reason",
		},
	},
	{
		Type:  "contact_attempts",
		Table: "contact_attempts",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"member_id", "attempt_at", "channel", "note", "contacted_by",
		},
	},
	{
		Type:  "cash_close_events",
		Table: "cash_close_events",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"close_date", "calculated_cash", "counted_cash",
			"discrepancy_reason", "closed_by",
		},
	},
	{
		Type:  "gym_ownership_transfers",
		Table: "gym_ownership_transfers",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"from_user_id", "to_user_id", "executed_at",
		},
	},
	{
		Type:  "notification_queue",
		Table: "notification_queue",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"channel", "template_key", "recipient_type",
			"recipient_id", "recipient_address", "payload",
			"status", "sent_at", "failed_at", "error_message",
			"retry_count", "scheduled_for",
		},
	},
	{
		Type:  "audit_log",
		Table: "audit_log",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"entity_type", "entity_id", "action", "actor_user_id",
			"changes", "ip_address", "user_agent",
		},
	},
	{
		// subscription_events — pull-only desde la perspectiva del sidecar.
		// El cloud es la fuente de verdad (webhooks de Stripe/MP escriben
		// acá), y el sidecar sólo necesita lectura para mostrar el historial
		// en la página Suscripción. La tabla es append-only: version siempre
		// 1, deleted_at siempre null. raw_payload es JSONB en Postgres.
		Type:  "subscription_events",
		Table: "subscription_events",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"provider", "type", "external_id", "plan",
			"amount", "currency", "period_ends_at",
			"raw_payload", "occurred_at", "recorded_at",
		},
	},
	{
		// owner_alert_configs has no surrogate id — its identity is the
		// (gym_id, alert_key) pair. The sidecar composes entity_id as
		// "<gym_id>:<alert_key>" for sync_queue dedupe; the projector
		// applies composite-key UPSERT using CompositeKey below and
		// reads alert_key out of the payload.
		Type:  "owner_alert_configs",
		Table: "owner_alert_configs",
		Columns: []string{
			"gym_id", "alert_key", "enabled", "version", "updated_at", "deleted_at",
		},
		CompositeKey: []string{"gym_id", "alert_key"},
	},
	// ─── challenges (retos) ─────────────────────────────────────────────────
	// All four tables flow through the generic projector. The
	// scoring/IR is computed at query time from these rows; the
	// projector itself is dumb and just mirrors columns.
	{
		Type:  "challenges",
		Table: "challenges",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"name", "description",
			"starts_at", "measurement_t0_deadline", "measurement_t1_start", "ends_at",
			"status",
			"inscription_fee_cents", "inscription_refundable",
			"min_weekly_attendance", "attendance_grace_weeks",
			"strength_cap_pct", "tie_margin_ir",
			"bf_floor_male_pct", "bf_floor_female_pct",
		},
	},
	{
		Type:  "challenge_categories",
		Table: "challenge_categories",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"challenge_id", "name", "sort_order",
		},
	},
	{
		Type:  "challenge_participants",
		Table: "challenge_participants",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"challenge_id", "member_id", "category_id",
			"exercise_legs", "exercise_push", "exercise_pull",
			"inscription_fee_paid", "inscription_paid_at", "inscription_refunded_at",
			"status", "disqualification_reason", "disqualified_at",
		},
	},
	{
		// Immutable rows in semantics: corrections are written as a new
		// row with the prior's id stored on superseded_by_id. The
		// projector still treats this as an upsert because creating the
		// successor row also updates the predecessor (sets superseded_at +
		// superseded_by_id). Both upserts go through this projector entry.
		Type:  "challenge_measurements",
		Table: "challenge_measurements",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"participant_id", "moment", "measured_at",
			"body_weight_kg", "body_fat_pct",
			"legs_weight_kg", "legs_reps",
			"push_weight_kg", "push_reps",
			"pull_weight_kg", "pull_reps",
			"notes", "created_by_user_id",
			"superseded_at", "superseded_by_id",
		},
	},
}

// FindTable returns the registry entry for an entity_type, or nil if the
// type isn't registered (push handler rejects with rejected_unknown_entity_type).
func FindTable(entityType string) *EntityTable {
	for i := range SyncedTables {
		if SyncedTables[i].Type == entityType {
			return &SyncedTables[i]
		}
	}
	return nil
}
