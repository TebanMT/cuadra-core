//go:build server && integration

package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// Pin del reset pre-migración HDLEON (jul-2026): la lista original de
// wipeGym se quedó corta contra el esquema actual y reventaba con FKs
// RESTRICT en cuanto el gym tenía datos reales (applied_promotions →
// payments, member_fingerprints → members, challenge_participants →
// members). El escenario de este test es el grafo del gym piloto: socio
// con huella + pago con promo aplicada + venta con items y stock + reto
// con participante y medición + notificación encolada.
//
// Run: DATABASE_URL=… go test -tags 'server integration' ./cmd/import-gym/

func seedFullGraph(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	gymID := uuid.New()
	ownerID := uuid.New()
	memberID := uuid.New()
	mtID := uuid.New()
	msID := uuid.New()
	payID := uuid.New()
	promoID := uuid.New()
	prodID := uuid.New()
	saleID := uuid.New()
	saleItemID := uuid.New()
	chID := uuid.New()
	catID := uuid.New()
	partID := uuid.New()

	exec := func(q string, args ...any) {
		t.Helper()
		if err := db.Exec(q, args...).Error; err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	exec(`INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Wipe Test Gym', 'MX', 'America/Mexico_City')`, gymID, gymID)
	exec(`INSERT INTO users (id, gym_id, version, created_at, updated_at, full_name, role, email)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Owner', 'owner', ?)`, ownerID, gymID, uuid.NewString()+"@t.mx")
	exec(`INSERT INTO members (id, gym_id, version, created_at, updated_at, full_name, phone, status, folio, created_by)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Socio Prueba', '+525511122233', 'active', 1, ?)`, memberID, gymID, ownerID)
	exec(`INSERT INTO member_fingerprints (id, gym_id, version, created_at, updated_at, member_id, template_encrypted, template_format, registered_by)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, '\x00'::bytea, 'ansi-378', ?)`, uuid.New(), gymID, memberID, ownerID)
	exec(`INSERT INTO membership_types (id, gym_id, version, created_at, updated_at, name, price, duration_days)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Mensual', 500, 30)`, mtID, gymID)
	exec(`INSERT INTO memberships (id, gym_id, version, created_at, updated_at, member_id, membership_type_id,
	          type_name_snapshot, price_snapshot, duration_days_snapshot, start_date, expiry_date, status)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 'Mensual', 500, 30, CURRENT_DATE - 10, CURRENT_DATE + 20, 'active')`,
		msID, gymID, memberID, mtID)
	exec(`INSERT INTO payments (id, gym_id, version, created_at, updated_at, member_id, operator_id, concept, amount, payment_method, payment_date, folio)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 'membership', 500, 'cash', CURRENT_DATE, 'P-001')`,
		payID, gymID, memberID, ownerID)
	exec(`INSERT INTO promotions (id, gym_id, version, created_at, updated_at, name, kind, value, buy_n, applies_to, active)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Promo Test', 'percent', 10, 1, 'membership', TRUE)`, promoID, gymID)
	exec(`INSERT INTO applied_promotions (id, gym_id, version, created_at, updated_at, promotion_id, payment_id, member_id,
	          applied_by_user_id, promotion_name_snapshot, kind_snapshot, value_snapshot, discount_amount)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, ?, ?, ?, 'Promo Test', 'percent', 10, 50)`,
		uuid.New(), gymID, promoID, payID, memberID, ownerID)
	exec(`INSERT INTO products (id, gym_id, version, created_at, updated_at, name, price, stock)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Agua', 20, 10)`, prodID, gymID)
	exec(`INSERT INTO sales (id, gym_id, version, created_at, updated_at, payment_id, member_id, subtotal, total)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 20, 20)`, saleID, gymID, payID, memberID)
	exec(`INSERT INTO sale_items (id, gym_id, version, created_at, updated_at, sale_id, product_id,
	          product_name_snapshot, unit_price_snapshot, quantity, line_total)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 'Agua', 20, 1, 20)`, saleItemID, gymID, saleID, prodID)
	exec(`INSERT INTO stock_movements (id, gym_id, version, created_at, updated_at, product_id, movement_type, delta, sale_item_id, operator_id)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, 'sale', -1, ?, ?)`, uuid.New(), gymID, prodID, saleItemID, ownerID)
	exec(`INSERT INTO checkins (id, gym_id, version, created_at, updated_at, member_id, method, result, checkin_at)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, 'fingerprint', 'allowed_active', NOW())`, uuid.New(), gymID, memberID)
	exec(`INSERT INTO challenges (id, gym_id, version, created_at, updated_at, name, starts_at,
	          measurement_t0_deadline, measurement_t1_start, ends_at)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Reto Verano', NOW(), NOW() + interval '7 days',
	          NOW() + interval '21 days', NOW() + interval '30 days')`, chID, gymID)
	exec(`INSERT INTO challenge_categories (id, gym_id, challenge_id, version, created_at, updated_at, name)
	      VALUES (?, ?, ?, 1, NOW(), NOW(), 'Peso')`, catID, gymID, chID)
	exec(`INSERT INTO challenge_participants (id, gym_id, version, created_at, updated_at, challenge_id, member_id, category_id)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, ?, ?)`, partID, gymID, chID, memberID, catID)
	exec(`INSERT INTO challenge_measurements (id, gym_id, version, created_at, updated_at, participant_id, moment,
	          measured_at, body_weight_kg, body_fat_pct, legs_weight_kg, legs_reps, push_weight_kg, push_reps,
	          pull_weight_kg, pull_reps, created_by_user_id)
	      VALUES (?, ?, 1, NOW(), NOW(), ?, 't0', NOW(), 80.5, 22.1, 100, 10, 60, 10, 70, 10, ?)`,
		uuid.New(), gymID, partID, ownerID)
	exec(`INSERT INTO notification_queue (id, gym_id, version, created_at, updated_at, channel, template_key,
	          recipient_type, recipient_id, recipient_address, payload, status, scheduled_for)
	      VALUES (?, ?, 1, NOW(), NOW(), 'whatsapp', 'expiry_reminder_3d', 'member', ?, '+525511122233', '{}', 'pending', NOW())`,
		uuid.New(), gymID, memberID)
	exec(`INSERT INTO notification_templates (id, gym_id, version, created_at, updated_at, template_key, body, enabled)
	      VALUES (?, ?, 1, NOW(), NOW(), 'expiry_reminder_3d', '', FALSE)`, uuid.New(), gymID)
	exec(`INSERT INTO owner_alert_configs (gym_id, alert_key, enabled, version, updated_at)
	      VALUES (?, 'owner_alert_low_stock', TRUE, 1, NOW())`, gymID)
	// Journal: una entrada de tipo borrable y una de tipo conservable.
	exec(`INSERT INTO sync_entities (gym_id, entity_type, entity_id, version, payload, server_updated_at)
	      VALUES (?, 'members', ?, 1, '{}', NOW()), (?, 'notification_templates', ?, 1, '{}', NOW())`,
		gymID, memberID, gymID, uuid.New())

	t.Cleanup(func() {
		// Best-effort: el propio wipe deja casi todo limpio; esto remata lo
		// conservado. Orden FK: hijos de gym primero.
		for _, tbl := range []string{"sync_entities", "notification_templates", "owner_alert_configs",
			"members", "users", "gyms"} {
			_ = db.Exec("DELETE FROM "+tbl+" WHERE gym_id = ?", gymID).Error
		}
	})
	return gymID
}

func TestWipeGym_FullGraphNoFKExplosion(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gymID := seedFullGraph(t, db)

	if err := db.Transaction(func(tx *gorm.DB) error {
		return wipeGym(tx, gymID)
	}); err != nil {
		t.Fatalf("wipeGym con grafo completo: %v", err)
	}

	count := func(table string) int {
		var n int
		if err := db.Raw("SELECT COUNT(*) FROM "+table+" WHERE gym_id = ?", gymID).Scan(&n).Error; err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// Todo lo operativo quedó en cero…
	for _, tbl := range []string{
		"members", "memberships", "membership_types", "payments", "sales", "sale_items",
		"stock_movements", "products", "promotions", "applied_promotions", "checkins",
		"member_fingerprints", "challenges", "challenge_categories", "challenge_participants",
		"challenge_measurements", "notification_queue",
	} {
		if n := count(tbl); n != 0 {
			t.Errorf("%s: quedan %d filas, want 0", tbl, n)
		}
	}
	// …y la identidad + config del gym sobrevive.
	for _, tbl := range []string{"gyms", "users", "notification_templates", "owner_alert_configs"} {
		if n := count(tbl); n == 0 {
			t.Errorf("%s: se borró y debía sobrevivir", tbl)
		}
	}
	// Journal selectivo: la entrada de members se fue, la del template queda.
	var journalMembers, journalTemplates int
	_ = db.Raw(`SELECT COUNT(*) FROM sync_entities WHERE gym_id = ? AND entity_type = 'members'`, gymID).Scan(&journalMembers).Error
	_ = db.Raw(`SELECT COUNT(*) FROM sync_entities WHERE gym_id = ? AND entity_type = 'notification_templates'`, gymID).Scan(&journalTemplates).Error
	if journalMembers != 0 {
		t.Errorf("sync_entities/members: quedan %d (un full-sync resucitaría a los socios de prueba)", journalMembers)
	}
	if journalTemplates != 1 {
		t.Errorf("sync_entities/notification_templates = %d, want 1 (los toggles deben viajar al sidecar fresco)", journalTemplates)
	}
}

// Pin del incidente ×100 post-migración (10-jul-2026): el import emitía los
// campos de dinero del journal en CENTAVOS (toCents), pero el wire de sync
// transporta PESOS — el apply del sidecar multiplica ×100 al aterrizar en
// SQLite (moneyColumns), así que el full-sync del gym migrado mostró todos
// los montos ×100 (ingresos del mes de $6,250 como $625,000). La regla: el
// payload de sync_entities lleva EXACTAMENTE lo que la columna NUMERIC del
// cloud guarda — pesos.
func TestImportEmitsMoneyInPesos(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gymID := uuid.New()
	ownerID := uuid.New()

	exec := func(q string, args ...any) {
		t.Helper()
		if err := db.Exec(q, args...).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Money Test Gym', 'MX', 'America/Mexico_City')`, gymID, gymID)
	exec(`INSERT INTO users (id, gym_id, version, created_at, updated_at, full_name, role, email)
	      VALUES (?, ?, 1, NOW(), NOW(), 'Owner', 'owner', ?)`, ownerID, gymID, uuid.NewString()+"@t.mx")
	t.Cleanup(func() {
		for _, tbl := range []string{"sync_entities", "payments", "memberships", "membership_types",
			"members", "users", "gyms"} {
			_ = db.Exec("DELETE FROM "+tbl+" WHERE gym_id = ?", gymID).Error
		}
	})

	// Dump sintético mínimo: plan → socio → sociomembresia → pago de $550.
	when := time.Now().Add(-24 * time.Hour)
	src := sourceData{
		memberships: []srcMembresia{{ID: 1, Nombre: "Mensual", Estado: 1, Precio: 550, Meses: 1}},
		socios:      []srcSocio{{ID: 1, Estado: 1, Nombre: "Rosa", Paterno: "Robles", Telefono: "4151112233", FechaCreacion: &when}},
		socioMembs: []srcSocioMembresia{{ID: 1, Estado: 1, IDSocio: 1, IDMembresia: 1, Precio: 550,
			FechaInicioMembresia: &when, Meses: 1, Vencimiento: &when}},
		socioMembsPagos: []srcSocioMembresiaPago{{ID: 1, Folio: 1, IDSocioMembresia: 1, Fecha: &when,
			Estado: 1, Importe: 550, IDTypePayment: 1}},
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return importAll(tx, src, gymID, ownerID)
	}); err != nil {
		t.Fatalf("importAll: %v", err)
	}

	// El dominio (NUMERIC pesos) y el journal deben decir LO MISMO: 550.
	var domainAmount float64
	if err := db.Raw(`SELECT amount FROM payments WHERE gym_id = ? AND concept = 'membership'`, gymID).
		Scan(&domainAmount).Error; err != nil {
		t.Fatalf("domain amount: %v", err)
	}
	if domainAmount != 550 {
		t.Errorf("payments.amount = %v, want 550 (pesos)", domainAmount)
	}
	var wireAmount float64
	if err := db.Raw(`
		SELECT (payload->>'amount')::float FROM sync_entities
		WHERE gym_id = ? AND entity_type = 'payments'`, gymID).
		Scan(&wireAmount).Error; err != nil {
		t.Fatalf("wire amount: %v", err)
	}
	if wireAmount != 550 {
		t.Errorf("journal amount = %v, want 550 (PESOS — el apply del sidecar hace la conversión a centavos; con toCents aquí el desktop mostraba ×100)", wireAmount)
	}
}
