//go:build sidecar

package infraestructure_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	"github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// Fixture mínima para el reader: todas las migraciones reales + gym +
// operador (payments.operator_id y members.created_by son NOT NULL FK).
type readerFixture struct {
	db         *sqlx.DB
	uow        sharedDomain.UnitOfWork
	gymID      uuid.UUID
	operatorID uuid.UUID
}

func setupReaderDB(t *testing.T) *readerFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlx.Open("sqlite3", filepath.Join(dir, "test.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	migDir := "../../../../db_migrations/sqlite"
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(migDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply %s: %v", e.Name(), err)
		}
	}

	f := &readerFixture{
		db:         db,
		uow:        sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue()),
		gymID:      uuid.New(),
		operatorID: uuid.New(),
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name)
		VALUES (?, ?, 1, ?, ?, 'Gym Test')`,
		f.gymID, f.gymID, now, now); err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, ?, ?, 'op@t.mx', 'x', 'Operador', 'operator')`,
		f.operatorID, f.gymID, now, now); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	return f
}

func (f *readerFixture) seedMember(t *testing.T, fullName string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC().UnixMilli()
	if _, err := f.db.Exec(`
		INSERT INTO members (id, gym_id, version, created_at, updated_at, folio, full_name, phone, created_by)
		VALUES (?, ?, 1, ?, ?, ?, ?, '5512345678', ?)`,
		id, f.gymID, now, now, "SOC-"+id.String()[:8], fullName, f.operatorID); err != nil {
		t.Fatalf("seed member %s: %v", fullName, err)
	}
	return id
}

// seedPayment inserta un pago con la misma forma que el resto de la suite de
// integración. memberID nil = venta walk-in sin socio. balanceCents en
// centavos (así lo guarda SQLite); deleted marca soft-delete.
func (f *readerFixture) seedPayment(t *testing.T, memberID *uuid.UUID, concept string, balanceCents int64, paymentDate string, deleted bool) {
	t.Helper()
	var deletedAt any
	if deleted {
		deletedAt = time.Now().UTC().UnixMilli()
	}
	var mid any
	if memberID != nil {
		mid = memberID.String()
	}
	id := uuid.New()
	if _, err := f.db.Exec(`
		INSERT INTO payments
		   (id, gym_id, version, created_at, updated_at, deleted_at, folio, member_id,
		    amount, payment_method, concept, balance_pending, payment_date, operator_id)
		 VALUES (?, ?, 1, 1, 1, ?, ?, ?, 10000, 'cash', ?, ?, ?, ?)`,
		id, f.gymID, deletedAt, "P-"+id.String()[:8], mid, concept, balanceCents, paymentDate, f.operatorID); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
}

func (f *readerFixture) listPendingBalances(t *testing.T) []reports.PendingBalanceRow {
	t.Helper()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	rows, err := reportsInfra.NewSQLiteReader().ListPendingBalances(tx, f.gymID)
	if err != nil {
		t.Fatalf("ListPendingBalances: %v", err)
	}
	return rows
}

// Regresión del bug de "Saldos pendientes": la versión rn=1 tomaba sólo el
// pago MÁS RECIENTE del socio. Un socio con deuda vieja que luego hacía una
// compra pagada completa desaparecía de la lista de deudores (su pago más
// reciente traía balance 0). La deuda vieja DEBE seguir apareciendo.
func TestListPendingBalances_DeudaViejaSobreviveAPagoNuevoSaldado(t *testing.T) {
	f := setupReaderDB(t)
	ana := f.seedMember(t, "Ana")
	f.seedPayment(t, &ana, "membership", 15000, "2026-05-01", false) // debe $150
	f.seedPayment(t, &ana, "product", 0, "2026-07-01", false)        // compra nueva, pagada completa

	rows := f.listPendingBalances(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (la deuda vieja no puede desaparecer)", len(rows))
	}
	got := rows[0]
	if got.MemberID != ana {
		t.Errorf("member = %s, want Ana (%s)", got.MemberID, ana)
	}
	if got.BalancePending != 150.00 {
		t.Errorf("balance = %v, want 150.00", got.BalancePending)
	}
	if want := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC); !got.PaymentDate.Equal(want) {
		t.Errorf("payment_date = %v, want %v (debe desde la deuda abierta más vieja)", got.PaymentDate, want)
	}
}

func TestListPendingBalances_SumaTodasLasDeudasDelSocio(t *testing.T) {
	f := setupReaderDB(t)

	// Bruno: dos deudas vivas → una sola fila con la SUMA y la fecha de la
	// más vieja. Además una deuda soft-deleted que NO debe contar.
	bruno := f.seedMember(t, "Bruno")
	f.seedPayment(t, &bruno, "membership", 10000, "2026-06-15", false)
	f.seedPayment(t, &bruno, "product", 5025, "2026-06-20", false)
	f.seedPayment(t, &bruno, "product", 99900, "2026-06-01", true) // soft-deleted

	// Ana: deuda única + settlement posterior con balance 0 (flujo UC-019).
	ana := f.seedMember(t, "Ana")
	f.seedPayment(t, &ana, "membership", 15000, "2026-05-01", false)
	f.seedPayment(t, &ana, "balance_settlement", 0, "2026-06-30", false)

	// Carla: sin deuda → ausente.
	carla := f.seedMember(t, "Carla")
	f.seedPayment(t, &carla, "membership", 0, "2026-06-10", false)

	// Venta walk-in fiada sin socio: no puede aparecer (ni reventar el JOIN).
	f.seedPayment(t, nil, "product", 7777, "2026-06-25", false)

	rows := f.listPendingBalances(t)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (Bruno y Ana)", len(rows))
	}

	// Orden: mayor deuda primero (Bruno $150.25 > Ana $150.00).
	if rows[0].MemberID != bruno || rows[1].MemberID != ana {
		t.Fatalf("orden = [%s, %s], want [Bruno, Ana] (balance DESC)", rows[0].FullName, rows[1].FullName)
	}
	if rows[0].BalancePending != 150.25 {
		t.Errorf("Bruno balance = %v, want 150.25 (10000+5025 centavos, sin la soft-deleted)", rows[0].BalancePending)
	}
	if want := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC); !rows[0].PaymentDate.Equal(want) {
		t.Errorf("Bruno payment_date = %v, want %v", rows[0].PaymentDate, want)
	}
	if rows[1].BalancePending != 150.00 {
		t.Errorf("Ana balance = %v, want 150.00", rows[1].BalancePending)
	}
}
