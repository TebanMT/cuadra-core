//go:build sidecar

package repositories

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// TestSQLiteMemberList corre el escenario compartido (member_list_scenario_test.go)
// contra el repo del sidecar sobre el schema completo de migraciones.
func TestSQLiteMemberList(t *testing.T) {
	uow, gymID := seedSQLiteMemberScenario(t)
	repo := NewMemberSQLiteRepository()
	runMemberListScenario(t, memRepo.ListQuery{GymID: gymID}, func(t *testing.T, q memRepo.ListQuery) ([]*memRepo.MemberWithMembership, int) {
		t.Helper()
		tx, err := uow.Query(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer sharedDomain.CloseQuery(tx)
		rows, total, err := repo.List(tx, q)
		if err != nil {
			t.Fatalf("List(%+v): %v", q, err)
		}
		return rows, total
	})
}

func seedSQLiteMemberScenario(t *testing.T) (sharedDomain.UnitOfWork, uuid.UUID) {
	t.Helper()
	db, err := sqlx.Open("sqlite3", filepath.Join(t.TempDir(), "test.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	migDir := "../../../../../../db_migrations/sqlite"
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

	gymID, userID, planID := uuid.New(), uuid.New(), uuid.New()
	now := scenarioToday.UnixMilli()
	db.MustExec(`INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name) VALUES (?, ?, 1, ?, ?, 'Gym Test')`,
		gymID.String(), gymID.String(), now, now)
	db.MustExec(`INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, ?, ?, 'op@t.mx', 'x', 'Operador', 'operator')`, userID.String(), gymID.String(), now, now)
	db.MustExec(`INSERT INTO membership_types (id, gym_id, version, created_at, updated_at, name, price, duration_days, active)
		VALUES (?, ?, 1, ?, ?, 'Mensual', 50000, 30, 1)`, planID.String(), gymID.String(), now, now)

	for _, m := range memberListScenario() {
		id := uuid.New().String()
		status := "active"
		if m.inactive {
			status = "inactive"
		}
		var deletedAt, number any
		if m.deleted {
			deletedAt = now
		}
		if m.memberNumber != nil {
			number = *m.memberNumber
		}
		db.MustExec(`INSERT INTO members (id, gym_id, version, created_at, updated_at, deleted_at, folio, full_name, phone, status, member_number, created_by)
			VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, gymID.String(), now, now, deletedAt, m.folio, m.name, m.phone, status, number, userID.String())
		if m.membership == "" {
			continue
		}
		start, expiry := scenarioExpiry(m)
		var expiryArg any
		if expiry != "" {
			expiryArg = expiry
		}
		db.MustExec(`INSERT INTO memberships (id, gym_id, version, created_at, updated_at, member_id, membership_type_id,
			type_name_snapshot, price_snapshot, duration_days_snapshot, start_date, expiry_date, status)
			VALUES (?, ?, 1, ?, ?, ?, ?, 'Mensual', 50000, 30, ?, ?, ?)`,
			uuid.New().String(), gymID.String(), now, now, id, planID.String(), start, expiryArg, m.membership)
	}
	return sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue()), gymID
}
