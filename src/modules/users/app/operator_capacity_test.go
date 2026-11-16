//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	tintaruntime "github.com/cuadra/cuadra-core/src/shared/runtime"
)

// El cupo de operadores cuenta sólo ACTIVOS (decisión de producto): un
// operador desactivado libera su lugar y reactivarlo lo vuelve a ocupar, con
// el mismo límite y el mismo error que el alta. El dueño nunca cuenta.

type capacityFixture struct {
	db      *sqlx.DB
	uow     sharedDomain.UnitOfWork
	users   *usersRepoLite.UserSQLiteRepository
	gymID   uuid.UUID
	ownerID uuid.UUID
	create  *usersApp.CreateOperator
	toggle  *usersApp.ToggleOperatorActive
}

// newCapacityFixture: withGyms=true cablea el repo de gym (cap por plan; el
// gym de prueba es trial, que desde el 16 nov 2026 trae extra_operators →
// 10 operadores); false deja el cap absoluto del producto (10).
func newCapacityFixture(t *testing.T, withGyms bool) *capacityFixture {
	t.Helper()
	// El modo dev relaja los gates Plus: el test fija el plan real.
	tintaruntime.SetForTest(t, tintaruntime.ModeTest)
	db, uow, gym := setupOperatorsFixture(t)
	users := usersRepoLite.NewUserSQLiteRepository()
	rec := audit.NewSQLiteRecorder()
	seedOwner(t, uow, users, gym.ID)
	f := &capacityFixture{
		db: db, uow: uow, users: users, gymID: gym.ID, ownerID: fetchOwnerID(t, db, gym.ID),
		create: usersApp.NewCreateOperator(users, uow, rec),
		toggle: usersApp.NewToggleOperatorActive(users, nil, uow, rec),
	}
	if withGyms {
		gyms := gymRepoLite.NewGymSQLiteRepository()
		f.create.WithGymRepo(gyms)
		f.toggle.WithGymRepo(gyms)
	}
	return f
}

func (f *capacityFixture) createOp(i int) (uuid.UUID, error) {
	out, err := f.create.Execute(context.Background(), usersApp.CreateOperatorInput{
		GymID: f.gymID, OwnerID: f.ownerID,
		FullName: fmt.Sprintf("Operador %d", i), Phone: fmt.Sprintf("+52 442 555 %04d", i),
	})
	return out.UserID, err
}

func (f *capacityFixture) setActive(id uuid.UUID, active bool) error {
	_, err := f.toggle.Execute(context.Background(), usersApp.ToggleOperatorActiveInput{
		GymID: f.gymID, ActorUserID: f.ownerID, TargetID: id, Active: active,
	})
	return err
}

func (f *capacityFixture) count(t *testing.T) int {
	t.Helper()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sharedDomain.CloseQuery(tx)
	n, err := f.users.CountOperatorsByGym(tx, f.gymID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func (f *capacityFixture) isActive(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	var active int
	if err := f.db.Get(&active, "SELECT active FROM users WHERE id = ?", id.String()); err != nil {
		t.Fatalf("read active: %v", err)
	}
	return active == 1
}

func TestCountOperatorsByGym_OnlyActiveOperators(t *testing.T) {
	f := newCapacityFixture(t, false)
	a, err := f.createOp(1)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := f.createOp(2); err != nil {
		t.Fatalf("create b: %v", err)
	}
	// El dueño ya existe y no cuenta.
	if n := f.count(t); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	if err := f.setActive(a, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if n := f.count(t); n != 1 {
		t.Fatalf("count tras desactivar = %d, want 1 (los inactivos no cuentan)", n)
	}
	// Borrado lógico tampoco cuenta.
	f.db.MustExec("UPDATE users SET deleted_at = 1 WHERE id <> ? AND role = 'operator' AND active = 1", a.String())
	if n := f.count(t); n != 0 {
		t.Fatalf("count tras borrar = %d, want 0", n)
	}
}

// La prueba (y Standard) traen el equipo completo: 10 operadores además del
// dueño, con el mismo error de cupo que el cap absoluto. Desactivar libera el
// lugar; reactivar sobre el cupo se bloquea; reactivar a alguien ya activo
// es no-op.
func TestCreateOperator_TrialGetsFullTeam(t *testing.T) {
	f := newCapacityFixture(t, true)
	ids := make([]uuid.UUID, 0, 10)
	for i := 1; i <= 10; i++ {
		id, err := f.createOp(i)
		if err != nil {
			t.Fatalf("operador %d en prueba: %v", i, err)
		}
		ids = append(ids, id)
	}
	if _, err := f.createOp(11); !errors.Is(err, userErrors.ErrOperatorLimitReached) {
		t.Fatalf("11º operador: err = %v, want ErrOperatorLimitReached", err)
	}
	a := ids[0]
	if err := f.setActive(a, false); err != nil {
		t.Fatalf("deactivate a: %v", err)
	}
	if _, err := f.createOp(11); err != nil {
		t.Fatalf("11º operador con a inactivo: %v", err)
	}
	if err := f.setActive(a, true); !errors.Is(err, userErrors.ErrOperatorLimitReached) {
		t.Fatalf("reactivar a sobre el cupo: err = %v, want ErrOperatorLimitReached", err)
	}
	if f.isActive(t, a) {
		t.Fatal("la reactivación bloqueada no debe persistir")
	}
	if err := f.setActive(ids[1], true); err != nil {
		t.Fatalf("reactivar a alguien ya activo: %v", err)
	}
	if n := f.count(t); n != 10 {
		t.Fatalf("count = %d, want 10", n)
	}
}

// Cap absoluto (10 operadores, Plus o sin repo de gym): mismo criterio.
func TestToggleOperatorActive_ReactivationBlockedAtAbsoluteCap(t *testing.T) {
	f := newCapacityFixture(t, false)
	var first uuid.UUID
	for i := 1; i <= 10; i++ {
		id, err := f.createOp(i)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if i == 1 {
			first = id
		}
	}
	if _, err := f.createOp(11); !errors.Is(err, userErrors.ErrOperatorLimitReached) {
		t.Fatalf("11º operador: err = %v, want ErrOperatorLimitReached", err)
	}
	if err := f.setActive(first, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := f.createOp(11); err != nil {
		t.Fatalf("11º operador con uno inactivo: %v", err)
	}
	if err := f.setActive(first, true); !errors.Is(err, userErrors.ErrOperatorLimitReached) {
		t.Fatalf("reactivar sobre el cupo: err = %v, want ErrOperatorLimitReached", err)
	}
	if n := f.count(t); n != 10 {
		t.Fatalf("count = %d, want 10", n)
	}
}
