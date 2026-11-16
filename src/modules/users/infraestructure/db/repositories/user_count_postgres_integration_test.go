//go:build server && integration

package repositories

import (
	"context"
	"testing"

	"github.com/google/uuid"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// El cupo de operadores cuenta sólo activos y no borrados; el dueño no
// cuenta. Mismo criterio que UserSQLiteRepository.CountOperatorsByGym
// (cubierto en src/modules/users/app/operator_capacity_test.go).
func TestPostgresCountOperatorsByGym_OnlyActive(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gymID := uuid.New()
	t.Cleanup(func() {
		for _, table := range []string{"users", "cash_drawers", "gyms"} {
			db.Exec("DELETE FROM "+table+" WHERE gym_id=?", gymID)
		}
	})
	if err := db.Exec(`INSERT INTO gyms(id,gym_id,version,created_at,updated_at,name,country,timezone) VALUES(?,?,1,NOW(),NOW(),'Operator cap test','MX','America/Mexico_City')`, gymID, gymID).Error; err != nil {
		t.Fatal(err)
	}
	for i, u := range []struct {
		role            string
		active, deleted bool
	}{
		{role: "owner", active: true},
		{role: "operator", active: true},
		{role: "operator", active: true},
		{role: "operator", active: false},
		{role: "operator", active: true, deleted: true},
	} {
		id := uuid.New()
		if err := db.Exec(`INSERT INTO users(id,gym_id,version,created_at,updated_at,deleted_at,email,full_name,role,active)
			VALUES(?,?,1,NOW(),NOW(),CASE WHEN ? THEN NOW() END,?,'Staff',?,?)`,
			id, gymID, u.deleted, "cap-"+id.String()[:8]+"@t.mx", u.role, u.active).Error; err != nil {
			t.Fatalf("seed user %d: %v", i, err)
		}
	}
	uow := shared.NewPostgresUnitOfWork(db)
	tx, err := uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(tx)
	n, err := NewUserPostgresRepository().CountOperatorsByGym(tx, gymID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("CountOperatorsByGym = %d, want 2 (sólo operadores activos no borrados)", n)
	}
}
