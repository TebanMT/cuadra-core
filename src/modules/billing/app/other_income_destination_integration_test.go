//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	migrations "github.com/cuadra/cuadra-core/db_migrations"
	dbinfra "github.com/cuadra/cuadra-core/infraestructure/db"
	reports "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	billingRepos "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	expRepos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
	"github.com/google/uuid"
)

func TestOtherIncome_DestinationSurvivesCorrectionAndSyncWithoutInventingDrawerCash(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	day := gymTestDay(0)
	register := billingApp.NewRegisterOtherIncome(f.paymentRepo, f.folios, f.uow, f.recorder).WithGyms(f.gymRepo)
	in := billingApp.RegisterOtherIncomeInput{GymID: f.gymID, ActorUserID: f.ownerID, Amount: 19.99, Method: "cash", CashDestination: "gym_fund", Description: "Venta de equipo usado", PaymentDate: day, IdempotencyKey: "outside-cash-income"}
	got, err := register.Execute(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := register.Execute(ctx, in)
	if err != nil || repeat.PaymentID != got.PaymentID {
		t.Fatalf("repeat=%+v err=%v", repeat, err)
	}
	in.CashDestination = "cash_drawer"
	if _, err = register.Execute(ctx, in); !errors.Is(err, billingErrors.ErrIdempotencyKeyConflict) {
		t.Fatalf("changed destination retry err=%v", err)
	}
	tx, _ := f.uow.Query(ctx)
	p, err := f.paymentRepo.GetByID(tx, got.PaymentID)
	if err != nil {
		t.Fatal(err)
	}
	if p.EffectiveCashDrawerID() != uuid.Nil || p.CashDrawerID != nil || p.EffectiveCashDestination() != "gym_fund" {
		t.Fatalf("payment=%+v", p)
	}
	// A capture correction preserves the receipt's destination.
	amount := 21.29
	corrected, err := billingApp.NewCorrectPayment(f.paymentRepo, billingRepos.NewPaymentCorrectionSQLiteRepository(), nil, f.uow, f.recorder).WithGyms(f.gymRepo).Execute(ctx, billingApp.CorrectPaymentInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PaymentID: p.ID, ExpectedVersion: p.Version, Amount: &amount, Reason: "El importe recibido fue 21.29", IdempotencyKey: "correct-outside-cash"})
	if err != nil {
		t.Fatal(err)
	}
	if corrected.After.CashDrawerID != nil || corrected.After.CashDestination != "gym_fund" {
		t.Fatalf("correction=%+v", corrected)
	}
	var payload []byte
	if err = f.db.Get(&payload, `SELECT payload FROM sync_queue WHERE entity_type='payments' AND entity_id=? ORDER BY id DESC LIMIT 1`, p.ID); err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["cash_destination"] != "gym_fund" || wire["cash_drawer_id"] != nil {
		t.Fatalf("sync payload=%s", payload)
	}
	version := int(wire["version"].(float64))
	if _, err = f.db.Exec(`UPDATE payments SET version=0,cash_destination='cash_drawer',amount=1 WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.uow.Command(ctx, func(tx shared.Transaction) error {
		return syncpkg.ApplyPullChange(ctx, tx, syncpkg.PullChange{EntityType: "payments", EntityID: p.ID.String(), Version: version, Payload: payload, ServerUpdatedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	// Contributions are physical entries only, never business income.
	_, err = expApp.NewCreateCashMovement(expRepos.NewCashMovementSQLiteRepository(), f.uow, f.recorder, expApp.OperationalPolicy{}).Execute(ctx, expApp.CashMovementInput{GymID: f.gymID, ActorUserID: f.ownerID, Amount: 100, MovementOn: day, MovementType: "cash_in", Reason: "Aportación para cambio", IdempotencyKey: "contribution"})
	if err != nil {
		t.Fatal(err)
	}
	reader := reports.NewSQLiteReader()
	tx, _ = f.uow.Query(ctx)
	finances, err := reader.CanonicalFinancialBetween(tx, f.gymID, "America/Mexico_City", day, day)
	if err != nil {
		t.Fatal(err)
	}
	cash, err := reader.SumCashClosedBetween(tx, f.gymID, day, day)
	if err != nil {
		t.Fatal(err)
	}
	if finances.OtherIncome != 21.29 || cash != 100 {
		t.Fatalf("income=%v cash=%v", finances.OtherIncome, cash)
	}
	// Starting the application again must not backfill reception for this income.
	if err = dbinfra.ApplySQLiteMigrations(f.db, migrations.SQLite, "sqlite"); err != nil {
		t.Fatal(err)
	}
	var drawerID *string
	if err = f.db.Get(&drawerID, `SELECT cash_drawer_id FROM payments WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if drawerID != nil {
		t.Fatalf("startup assigned outside-cash income to drawer %s", *drawerID)
	}
}
