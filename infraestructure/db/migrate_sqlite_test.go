//go:build sidecar

package db

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

// TestMigration013_SubscriptionPlansRename es la red de seguridad para el
// bug específico que se nos coló: la migración 013 (recreación de la tabla
// gyms para cambiar el CHECK constraint) reventaba con "FOREIGN KEY
// constraint failed" en el binario real porque otras tablas (users, members,
// payments, etc.) tienen `REFERENCES gyms(id)`.
//
// Reproducimos exactamente el escenario de producción: DB fresco con
// `_foreign_keys=on`, todas las migraciones que tocan gyms o crean tablas
// con FK a gyms, y luego 013.
func TestMigration013_SubscriptionPlansRename(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sqlx.Open("sqlite3", dbPath+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Cargamos las migraciones críticas para reproducir el bug FK de 013:
	// 001 crea gyms + users + tablas con `REFERENCES gyms(id)`, 008 amplía
	// gyms con charge_settings (entra al recreate). Las migraciones 009-012
	// añadieron columnas que después se consolidaron en 001 vía refactor;
	// en un DB fresco vuelan con "duplicate column name". En producción no
	// se nota porque los DBs vienen con `_migrations` poblado de installs
	// anteriores. Para este test sólo nos importa que 013 maneje bien los FK.
	migrationsToLoad := []string{
		"../../db_migrations/sqlite/001_init_schema.sql",
		"../../db_migrations/sqlite/002_notifications.sql",
		"../../db_migrations/sqlite/003_sync_local.sql",
		"../../db_migrations/sqlite/004_owner_alert_configs.sql",
		"../../db_migrations/sqlite/008_gym_charge_settings.sql",
	}
	for _, m := range migrationsToLoad {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}

	// Insertamos un gym con SKU viejo + un user que lo referencia. Si la
	// migración 013 maneja mal las FKs, el INSERT INTO ... SELECT * FROM
	// _gyms_old o el DROP _gyms_old van a fallar por FK violation.
	gymID := "00000000-0000-0000-0000-000000000001"
	now := "1715000000000"
	_, err = db.Exec(`
		INSERT INTO gyms (id, gym_id, name, subscription_plan, subscription_status, created_at, updated_at)
		VALUES (?, ?, 'Test Gym', 'pro_monthly', 'active', `+now+`, `+now+`)`,
		gymID, gymID)
	if err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	userID := "00000000-0000-0000-0000-000000000002"
	_, err = db.Exec(`
		INSERT INTO users (id, gym_id, email, password_hash, full_name, role, active, created_at, updated_at)
		VALUES (?, ?, 'owner@test.mx', 'hash', 'Owner', 'owner', 1, `+now+`, `+now+`)`,
		userID, gymID)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Aplicar 013 — el escenario que estaba reventando.
	b, err := os.ReadFile("../../db_migrations/sqlite/013_subscription_plans_rename.sql")
	if err != nil {
		t.Fatalf("read 013: %v", err)
	}
	if _, err := db.Exec(string(b)); err != nil {
		t.Fatalf("apply 013: %v (este es el bug que reportó el usuario)", err)
	}

	// Verificaciones post-migración:
	// 1) El gym sigue ahí con el SKU renombrado.
	var plan string
	if err := db.Get(&plan, "SELECT subscription_plan FROM gyms WHERE id = ?", gymID); err != nil {
		t.Fatalf("read gym post-013: %v", err)
	}
	if plan != "standard_monthly" {
		t.Errorf("subscription_plan = %q, want standard_monthly (renombrado desde pro_monthly)", plan)
	}

	// 2) El user sigue ahí y su FK a gyms.id está intacta.
	var count int
	if err := db.Get(&count, "SELECT COUNT(*) FROM users WHERE id = ?", userID); err != nil {
		t.Fatalf("read user post-013: %v", err)
	}
	if count != 1 {
		t.Errorf("user count = %d, want 1 (FK rota)", count)
	}

	// 3) FK enforcement quedó re-encendido tras el PRAGMA toggle.
	var fk int
	if err := db.Get(&fk, "PRAGMA foreign_keys"); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d post-013, want 1 (debió quedar encendido)", fk)
	}

	// 4) El CHECK nuevo acepta los SKUs nuevos.
	_, err = db.Exec(`
		INSERT INTO gyms (id, gym_id, name, subscription_plan, subscription_status, created_at, updated_at)
		VALUES ('00000000-0000-0000-0000-000000000099', '00000000-0000-0000-0000-000000000099', 'Plus Test', 'plus_annual', 'active', ` + now + `, ` + now + `)`)
	if err != nil {
		t.Errorf("CHECK should accept plus_annual: %v", err)
	}

	// 5) El CHECK nuevo rechaza los SKUs viejos.
	_, err = db.Exec(`
		INSERT INTO gyms (id, gym_id, name, subscription_plan, subscription_status, created_at, updated_at)
		VALUES ('00000000-0000-0000-0000-000000000098', '00000000-0000-0000-0000-000000000098', 'Old SKU', 'pro_monthly', 'active', ` + now + `, ` + now + `)`)
	if err == nil {
		t.Errorf("CHECK should reject pro_monthly post-013, but insert succeeded")
	}
}

// TestMigration015_GymsWhatsAppUnique cubre dos cosas de la migración 015:
// (a) soft-delete del más reciente cuando hay duplicados pre-existentes,
// (b) UNIQUE INDEX post-migración rechaza nuevos duplicados.
func TestMigration015_GymsWhatsAppUnique(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sqlx.Open("sqlite3", dbPath+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	migrationsToLoad := []string{
		"../../db_migrations/sqlite/001_init_schema.sql",
		"../../db_migrations/sqlite/002_notifications.sql",
		"../../db_migrations/sqlite/003_sync_local.sql",
		"../../db_migrations/sqlite/004_owner_alert_configs.sql",
		"../../db_migrations/sqlite/008_gym_charge_settings.sql",
		"../../db_migrations/sqlite/013_subscription_plans_rename.sql",
		"../../db_migrations/sqlite/014_subscription_events.sql",
	}
	for _, m := range migrationsToLoad {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}

	// Sembramos dos gyms con el MISMO whatsapp; el más reciente (created_at
	// mayor) debe quedar soft-deletado tras 015.
	oldID := "10000000-0000-0000-0000-000000000001"
	newID := "10000000-0000-0000-0000-000000000002"
	thirdID := "10000000-0000-0000-0000-000000000003"
	t0 := "1700000000000"
	t1 := "1710000000000"
	t2 := "1720000000000"
	for _, ins := range []struct{ id, createdAt string }{
		{oldID, t0},
		{newID, t1},
		{thirdID, t2},
	} {
		_, err := db.Exec(`
			INSERT INTO gyms (id, gym_id, name, whatsapp, created_at, updated_at)
			VALUES (?, ?, 'Dup Gym', '+5215555550000', ?, ?)`,
			ins.id, ins.id, ins.createdAt, ins.createdAt)
		if err != nil {
			t.Fatalf("seed dup gym %s: %v", ins.id, err)
		}
	}

	// Aplicar 015 — soft-delete + crear UNIQUE.
	b, err := os.ReadFile("../../db_migrations/sqlite/015_gyms_whatsapp_unique.sql")
	if err != nil {
		t.Fatalf("read 015: %v", err)
	}
	if _, err := db.Exec(string(b)); err != nil {
		t.Fatalf("apply 015: %v", err)
	}

	// El más antiguo (oldID) debe seguir vivo; los dos más recientes
	// soft-deletados (deleted_at NOT NULL).
	var deletedAt *int64
	if err := db.Get(&deletedAt, "SELECT deleted_at FROM gyms WHERE id = ?", oldID); err != nil {
		t.Fatalf("read oldID: %v", err)
	}
	if deletedAt != nil {
		t.Errorf("oldID got soft-deleted; debió quedar vivo. deleted_at=%v", *deletedAt)
	}
	for _, dupID := range []string{newID, thirdID} {
		var d *int64
		if err := db.Get(&d, "SELECT deleted_at FROM gyms WHERE id = ?", dupID); err != nil {
			t.Fatalf("read %s: %v", dupID, err)
		}
		if d == nil {
			t.Errorf("dup %s no fue soft-deletado", dupID)
		}
	}

	// El INSERT con whatsapp duplicado (entre gyms vivos) ahora rechaza.
	_, err = db.Exec(`
		INSERT INTO gyms (id, gym_id, name, whatsapp, created_at, updated_at)
		VALUES ('10000000-0000-0000-0000-0000000000ff', '10000000-0000-0000-0000-0000000000ff', 'New Conflict', '+5215555550000', ?, ?)`,
		t2, t2)
	if err == nil {
		t.Errorf("UNIQUE INDEX uq_gyms_whatsapp debió rechazar duplicado, pero pasó")
	}

	// Pero un INSERT con OTRO número distinto pasa sin problemas.
	_, err = db.Exec(`
		INSERT INTO gyms (id, gym_id, name, whatsapp, created_at, updated_at)
		VALUES ('10000000-0000-0000-0000-0000000000aa', '10000000-0000-0000-0000-0000000000aa', 'Other', '+5215555550001', ?, ?)`,
		t2, t2)
	if err != nil {
		t.Errorf("INSERT con whatsapp distinto debió pasar: %v", err)
	}

	// Y un INSERT con whatsapp NULL (gym recién creado, aún sin setup)
	// también pasa — el índice partial WHERE whatsapp IS NOT NULL lo permite.
	_, err = db.Exec(`
		INSERT INTO gyms (id, gym_id, name, created_at, updated_at)
		VALUES ('10000000-0000-0000-0000-0000000000bb', '10000000-0000-0000-0000-0000000000bb', 'Placeholder', ?, ?)`,
		t2, t2)
	if err != nil {
		t.Errorf("INSERT con whatsapp NULL debió pasar (gym placeholder): %v", err)
	}
}

// TestMigration021_FixMembershipsSelfFK reproduce el bug runtime
// "no such table: main.memberships_new" que aparecía al hacer
// POST /api/v1/members en sidecars que habían aplicado 011.
//
// Cadena del bug:
//   1. 011 crea memberships_new con `replaced_by REFERENCES memberships_new(id)`.
//   2. 011 corre con PRAGMA foreign_keys=OFF (necesario para el DROP).
//   3. Bajo foreign_keys=OFF, ALTER TABLE RENAME no reescribe FK refs
//      (SQLite docs: "modification of foreign-key-constraints only
//      happens when the foreign_keys pragma is enabled").
//   4. Tras RENAME memberships_new → memberships, el schema literal en
//      sqlite_schema sigue diciendo REFERENCES memberships_new(id).
//   5. Cualquier INSERT que dispare validación de FK rompe.
//
// La 021 rebuildea memberships con la self-FK escrita como
// REFERENCES memberships(id) (nombre canónico final). Como SQLite busca
// el token de la tabla TEMPORAL durante el rename, y el body no
// contiene ese token, la self-FK queda intacta apuntando al nombre
// correcto.
func TestMigration021_FixMembershipsSelfFK(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sqlx.Open("sqlite3", dbPath+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Sólo cargamos las migraciones necesarias para reproducir el bug:
	// 001 (crea gyms, members, membership_types, memberships con
	// self-FK correcta), 011 (rebuildea memberships con la self-FK
	// rota), 021 (el fix).
	for _, m := range []string{
		"../../db_migrations/sqlite/001_init_schema.sql",
		"../../db_migrations/sqlite/011_membership_pending_payment.sql",
	} {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}

	// Sanity informativa: el bug runtime "no such table:
	// main.memberships_new" depende de la versión de SQLite embebida en
	// el binario del cliente. SQLite >= 3.25 con legacy_alter_table=OFF
	// reescribe la self-FK durante el RENAME y NO reproduce el bug;
	// SQLite más viejo (o con la flag legacy_alter_table=ON) sí lo
	// reproduce. Logueamos el estado para diagnóstico pero no fallamos
	// el test si el schema ya quedó limpio aquí — la 021 es defensiva
	// y arregla cualquiera de los dos casos.
	var post011SQL string
	if err := db.Get(&post011SQL,
		`SELECT sql FROM sqlite_schema WHERE type='table' AND name='memberships'`); err != nil {
		t.Fatalf("read sqlite_schema post-011: %v", err)
	}
	t.Logf("post-011 memberships schema (SQLite local rewrote=%v):\n%s",
		!contains(post011SQL, "memberships_new"), post011SQL)

	// Aplicar 021 — el fix.
	b021, err := os.ReadFile("../../db_migrations/sqlite/021_fix_memberships_self_fk.sql")
	if err != nil {
		t.Fatalf("read 021: %v", err)
	}
	if _, err := db.Exec(string(b021)); err != nil {
		t.Fatalf("apply 021: %v", err)
	}

	// El schema post-021 NO debe mencionar memberships_new ni la tabla
	// temporal del fix (memberships_canon) — sólo la canónica.
	var fixedSQL string
	if err := db.Get(&fixedSQL,
		`SELECT sql FROM sqlite_schema WHERE type='table' AND name='memberships'`); err != nil {
		t.Fatalf("read sqlite_schema post-021: %v", err)
	}
	if contains(fixedSQL, "memberships_new") {
		t.Errorf("post-021 schema still references memberships_new:\n%s", fixedSQL)
	}
	if contains(fixedSQL, "memberships_canon") {
		t.Errorf("post-021 schema leaked memberships_canon (temp table):\n%s", fixedSQL)
	}

	// Seed mínimo para insertar memberships: necesitamos gym, member,
	// membership_type vivos.
	now := "1715000000000"
	gymID := "30000000-0000-0000-0000-000000000001"
	userID := "30000000-0000-0000-0000-000000000099"
	memberID := "30000000-0000-0000-0000-000000000002"
	typeID := "30000000-0000-0000-0000-000000000003"
	if _, err := db.Exec(`
		INSERT INTO gyms (id, gym_id, name, subscription_plan, subscription_status, created_at, updated_at)
		VALUES (?, ?, 'T', 'trial', 'active', `+now+`, `+now+`)`, gymID, gymID); err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO users (id, gym_id, email, password_hash, full_name, role, active, created_at, updated_at)
		VALUES (?, ?, 'owner@t.mx', 'hash', 'Owner', 'owner', 1, `+now+`, `+now+`)`,
		userID, gymID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO members (id, gym_id, full_name, phone, folio, status, enrollment_paid, created_by, created_at, updated_at)
		VALUES (?, ?, 'Test Member', '+5215555555555', 'F001', 'active', 1, ?, `+now+`, `+now+`)`,
		memberID, gymID, userID); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO membership_types (id, gym_id, name, price, duration_days, active, created_at, updated_at)
		VALUES (?, ?, 'Mensual', 50000, 30, 1, `+now+`, `+now+`)`,
		typeID, gymID); err != nil {
		t.Fatalf("seed membership_type: %v", err)
	}

	// El INSERT que rompía antes: nueva membresía con replaced_by NULL.
	// Bajo el bug pre-021, esto reventaba con "no such table:
	// main.memberships_new" porque SQLite valida el schema al primer
	// write.
	membershipID := "30000000-0000-0000-0000-000000000010"
	_, err = db.Exec(`
		INSERT INTO memberships
		    (id, gym_id, member_id, membership_type_id,
		     type_name_snapshot, price_snapshot, duration_days_snapshot,
		     start_date, expiry_date, status,
		     created_at, updated_at)
		VALUES (?, ?, ?, ?, 'Mensual', 50000, 30, '2026-05-22', '2026-06-21', 'active', `+now+`, `+now+`)`,
		membershipID, gymID, memberID, typeID)
	if err != nil {
		t.Fatalf("INSERT memberships post-021 con replaced_by NULL falló: %v", err)
	}

	// Marcamos la primera membresía como replaced para liberar el slot
	// del UNIQUE partial index (uq_memberships_member_active) — mismo
	// dance que hace el dominio en una renovación real.
	if _, err := db.Exec(
		`UPDATE memberships SET status='replaced' WHERE id=?`, membershipID); err != nil {
		t.Fatalf("demote first membership: %v", err)
	}

	// INSERT con replaced_by apuntando a una fila existente — ejercita
	// la self-FK que estaba rota antes.
	replacementID := "30000000-0000-0000-0000-000000000011"
	_, err = db.Exec(`
		INSERT INTO memberships
		    (id, gym_id, member_id, membership_type_id,
		     type_name_snapshot, price_snapshot, duration_days_snapshot,
		     start_date, expiry_date, status, replaced_by,
		     created_at, updated_at)
		VALUES (?, ?, ?, ?, 'Mensual', 50000, 30, '2026-06-22', '2026-07-21', 'active', ?, `+now+`, `+now+`)`,
		replacementID, gymID, memberID, typeID, membershipID)
	if err != nil {
		t.Fatalf("INSERT memberships con replaced_by apuntando a fila viva falló: %v", err)
	}

	// FK enforcement quedó re-encendido.
	var fk int
	if err := db.Get(&fk, "PRAGMA foreign_keys"); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d post-021, want 1", fk)
	}
}

// contains es un helper inline (strings.Contains aliased) para mantener
// el test compacto sin agregar imports.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
