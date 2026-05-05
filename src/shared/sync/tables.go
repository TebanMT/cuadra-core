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
			"subscription_status", "setup_completed_at",
			"whatsapp_business_phone", "whatsapp_business_token_enc", "whatsapp_connected_at",
			"kiosk_settings",
		},
	},
	{
		Type:  "users",
		Table: "users",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"email", "password_hash", "full_name", "phone", "role",
			"active", "must_change_password", "last_login_at", "created_by",
		},
	},
	{
		Type:  "membership_types",
		Table: "membership_types",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"name", "price", "duration_days", "enrollment_fee",
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
			"pin_hash", "pin_assigned_at",
			"last_contact_attempt_at", "created_by",
		},
	},
	{
		Type:  "memberships",
		Table: "memberships",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"member_id", "membership_type_id",
			"type_name_snapshot", "price_snapshot", "duration_days_snapshot",
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
		Type:  "payments",
		Table: "payments",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"folio", "member_id", "amount", "payment_method", "concept",
			"parent_payment_id", "discount_amount", "discount_reason",
			"balance_pending", "payment_date", "notes", "operator_id",
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
		Type:  "stock_movements",
		Table: "stock_movements",
		Columns: []string{
			"id", "gym_id", "version", "created_at", "updated_at", "deleted_at",
			"product_id", "movement_type", "delta", "reason", "cost",
			"sale_item_id", "operator_id",
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
