//go:build server && integration

package repositories

import (
	"testing"

	"github.com/google/uuid"

	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// TestPostgresMemberList corre el escenario compartido
// (member_list_scenario_test.go) contra el repo del cloud, a través del
// driver (la relevancia lleva parámetros enlazados en el ORDER BY).
//
// Run:
//
//	go test -tags 'server integration' ./src/modules/members/infraestructure/db/repositories/
func TestPostgresMemberList(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gymID, userID, planID := uuid.New(), uuid.New(), uuid.New()
	t.Cleanup(func() {
		// cash_drawers: la caja principal la crea un trigger al insertar el gym.
		for _, table := range []string{"memberships", "members", "membership_types", "users", "cash_drawers", "gyms"} {
			db.Exec("DELETE FROM "+table+" WHERE gym_id = ?", gymID)
		}
	})
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := db.Exec(sql, args...).Error; err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
		VALUES (?, ?, 1, NOW(), NOW(), 'Member list test', 'MX', 'America/Mexico_City')`, gymID, gymID)
	exec(`INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'x', 'Operador', 'operator')`, userID, gymID, "op-"+userID.String()[:8]+"@t.mx")
	exec(`INSERT INTO membership_types (id, gym_id, version, created_at, updated_at, name, price, duration_days, active)
		VALUES (?, ?, 1, NOW(), NOW(), 'Mensual', 500, 30, true)`, planID, gymID)

	for _, m := range memberListScenario() {
		id := uuid.New()
		status := "active"
		if m.inactive {
			status = "inactive"
		}
		exec(`INSERT INTO members (id, gym_id, version, created_at, updated_at, deleted_at, folio, full_name, phone, status, member_number, created_by)
			VALUES (?, ?, 1, NOW(), NOW(), CASE WHEN ? THEN NOW() END, ?, ?, ?, ?, ?, ?)`,
			id, gymID, m.deleted, m.folio, m.name, m.phone, status, m.memberNumber, userID)
		if m.membership == "" {
			continue
		}
		start, expiry := scenarioExpiry(m)
		var expiryArg any
		if expiry != "" {
			expiryArg = expiry
		}
		exec(`INSERT INTO memberships (id, gym_id, version, created_at, updated_at, member_id, membership_type_id,
			type_name_snapshot, price_snapshot, duration_days_snapshot, start_date, expiry_date, status)
			VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 'Mensual', 500, 30, ?::date, ?::date, ?)`,
			uuid.New(), gymID, id, planID, start, expiryArg, m.membership)
	}

	uow := sharedDomain.NewPostgresUnitOfWork(db)
	repo := NewMemberPostgresRepository()
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
