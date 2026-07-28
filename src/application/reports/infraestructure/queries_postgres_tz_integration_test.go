//go:build server && integration

package infraestructure_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// Frontera del día local del gym en Postgres.
//
// DOS motivos para que esto sea un test de INTEGRACIÓN y no unitario:
//
//  1. Semántica: verifica que los check-ins de la tarde-noche (el horario
//     PICO del gym) caigan en su día y no en el siguiente. Antes se
//     filtraba y agrupaba por día UTC, y en CDMX la medianoche UTC son las
//     6 PM.
//
//  2. Tipado de parámetros: `AT TIME ZONE ?` mete un parámetro BINDEADO
//     dentro de una expresión SQL — exactamente la clase de query que tumbó
//     los recordatorios 3 días en julio-2026 (`::date - ?` resolvía al
//     operador equivocado y reventaba AL PLANEAR, sin importar los datos).
//     Aquello no se reprodujo con literales en psql. Regla de la casa desde
//     entonces: se prueba A TRAVÉS del driver. Eso hace este archivo.
//
// Run:
//
//	go test -tags 'server integration' ./src/application/reports/...
const (
	cdmxTZ = "America/Mexico_City"
	// 27-jul-2026 7:30 PM en CDMX — en UTC ya es el 28.
	nocheDel27 = "2026-07-28T01:30:00Z"
	// 27-jul-2026 9:00 AM en CDMX — mismo día en ambas zonas.
	mananaDel27 = "2026-07-27T15:00:00Z"
)

type tzFixture struct {
	db       *gorm.DB
	uow      sharedDomain.UnitOfWork
	gymID    uuid.UUID
	userID   uuid.UUID
	memberID uuid.UUID
}

func setupTZFixture(t *testing.T) *tzFixture {
	t.Helper()
	db := testutil.OpenPostgres(t)
	f := &tzFixture{
		db:       db,
		uow:      sharedDomain.NewPostgresUnitOfWork(db),
		gymID:    uuid.New(),
		userID:   uuid.New(),
		memberID: uuid.New(),
	}
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
		VALUES (?, ?, 1, NOW(), NOW(), 'TZ Gym', 'MX', 'America/Mexico_City')`,
		f.gymID, f.gymID).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'x', 'Operador', 'operator')`,
		f.userID, f.gymID, "op-"+f.userID.String()[:8]+"@t.mx").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO members (id, gym_id, version, created_at, updated_at, folio, full_name, phone, created_by)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'Ana', '5512345678', ?)`,
		f.memberID, f.gymID, "SOC-"+f.memberID.String()[:8], f.userID).Error; err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return f
}

func (f *tzFixture) seedCheckin(t *testing.T, iso string) {
	t.Helper()
	at, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatalf("parse %q: %v", iso, err)
	}
	if err := f.db.Exec(`
		INSERT INTO checkins (id, gym_id, version, created_at, updated_at, member_id, checkin_at, method, result, operator_id)
		VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 'manual', 'allowed_active', ?)`,
		uuid.New(), f.gymID, f.memberID, at, f.userID).Error; err != nil {
		t.Fatalf("seed checkin: %v", err)
	}
}

func day(t *testing.T, ymd string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", ymd)
	if err != nil {
		t.Fatalf("parse day %q: %v", ymd, err)
	}
	return d
}

func TestPG_CountCheckins_NocheLocalCuentaEnSuDia(t *testing.T) {
	f := setupTZFixture(t)
	f.seedCheckin(t, nocheDel27)

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	reader := reportsInfra.NewPostgresReader()

	n, err := reader.CountCheckinsBetween(tx, f.gymID, cdmxTZ, day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("CountCheckinsBetween: %v", err)
	}
	if n != 1 {
		t.Errorf("check-ins del 27 (día local) = %d, want 1", n)
	}

	n, err = reader.CountCheckinsBetween(tx, f.gymID, cdmxTZ, day(t, "2026-07-28"), day(t, "2026-07-28"))
	if err != nil {
		t.Fatalf("CountCheckinsBetween: %v", err)
	}
	if n != 0 {
		t.Errorf("check-ins del 28 (día local) = %d, want 0 — el gym no había abierto", n)
	}
}

// TestPG_CheckinsDailySeries_AgrupaPorDiaLocal — además de la semántica,
// este es EL test que ejercita `AT TIME ZONE ?` con parámetro bindeado.
// Si Postgres no pudiera inferir el tipo del parámetro, reventaría aquí al
// planear, igual que el incidente de julio.
func TestPG_CheckinsDailySeries_AgrupaPorDiaLocal(t *testing.T) {
	f := setupTZFixture(t)
	f.seedCheckin(t, mananaDel27)
	f.seedCheckin(t, nocheDel27)

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewPostgresReader().
		CheckinsDailySeries(tx, f.gymID, cdmxTZ, day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("CheckinsDailySeries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("series = %+v, want 1 barra (ambos check-ins son del 27 local)", rows)
	}
	if got := rows[0].Date.Format("2006-01-02"); got != "2026-07-27" {
		t.Errorf("barra en %s, want 2026-07-27", got)
	}
	if rows[0].Count != 2 {
		t.Errorf("count = %d, want 2", rows[0].Count)
	}
}

// TestPG_ZonaInvalidaNoRevientaElQuery — un timezone basura en la config del
// gym degrada a UTC (tz.NameOrUTC), nunca tumba la página de reportes.
// Postgres resuelve zonas con SU catálogo, así que sin ese filtro previo un
// nombre inválido sería un error de query, no un fallback.
func TestPG_ZonaInvalidaNoRevientaElQuery(t *testing.T) {
	f := setupTZFixture(t)
	f.seedCheckin(t, mananaDel27)

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewPostgresReader().
		CheckinsDailySeries(tx, f.gymID, "No/Existe", day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("una zona inválida no debe romper el query: %v", err)
	}
	if len(rows) != 1 || rows[0].Count != 1 {
		t.Errorf("series con zona inválida = %+v, want 1 barra con 1", rows)
	}
}

// TestPG_ExpensesDailySeries_MercanciaCaeEnElDiaLocal — la gráfica de
// egresos mezcla tres fuentes: gastos (expense_date) y devoluciones
// (payment_date) ya venían en día local, pero la mercancía va por
// stock_movements.created_at, que es un instante. Un restock nocturno tiene
// que caer en la MISMA barra que el gasto capturado ese mismo día.
func TestPG_ExpensesDailySeries_MercanciaCaeEnElDiaLocal(t *testing.T) {
	f := setupTZFixture(t)
	productID := uuid.New()
	if err := f.db.Exec(`
		INSERT INTO products (id, gym_id, version, created_at, updated_at, name, price, stock)
		VALUES (?, ?, 1, NOW(), NOW(), 'Proteína', 500, 10)`,
		productID, f.gymID).Error; err != nil {
		t.Fatalf("seed product: %v", err)
	}
	at, _ := time.Parse(time.RFC3339, nocheDel27)
	if err := f.db.Exec(`
		INSERT INTO stock_movements (id, gym_id, version, created_at, updated_at, product_id, movement_type, delta, cost, operator_id)
		VALUES (?, ?, 1, ?, ?, ?, 'restock', 10, 200, ?)`,
		uuid.New(), f.gymID, at, at, productID, f.userID).Error; err != nil {
		t.Fatalf("seed stock movement: %v", err)
	}

	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewPostgresReader().
		ExpensesDailySeries(tx, f.gymID, cdmxTZ, day(t, "2026-07-27"), day(t, "2026-07-27"))
	if err != nil {
		t.Fatalf("ExpensesDailySeries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("series = %+v, want 1 barra en el 27 local", rows)
	}
	if got := rows[0].Date.Format("2006-01-02"); got != "2026-07-27" {
		t.Errorf("barra en %s, want 2026-07-27", got)
	}
}
