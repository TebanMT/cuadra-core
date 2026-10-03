//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	cashclose "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingRepos "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
)

func TestCashDrawers_CatalogIsStableIdempotentAndOfflineSynced(t *testing.T) {
	f := setupSales(t)
	repo := billingRepos.NewCashDrawerSQLiteRepository()
	uc := billingApp.NewCashDrawers(repo, f.uow, f.recorder)
	ctx := context.Background()

	rows, err := uc.List(ctx, billingApp.ListCashDrawersInput{GymID: f.gymID, IncludeInactive: true})
	if err != nil || len(rows) != 1 || !rows[0].IsMain || rows[0].ID != f.gymID || rows[0].Name != cashclose.DefaultDrawerName {
		t.Fatalf("seeded main drawer=%+v err=%v", rows, err)
	}

	input := billingApp.CreateCashDrawerInput{GymID: f.gymID, ActorUserID: f.ownerID,
		ActorRole: "owner", Name: "Recepción norte", IdempotencyKey: "drawer:north"}
	created, err := uc.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := uc.Create(ctx, input)
	if err != nil || replayed.ID != created.ID || replayed.Version != created.Version {
		t.Fatalf("idempotent replay=%+v err=%v", replayed, err)
	}
	input.Name = "Otro nombre"
	if _, err := uc.Create(ctx, input); !errors.Is(err, cashclose.ErrDrawerIdempotencyConflict) {
		t.Fatalf("changed idempotent payload error=%v", err)
	}
	if created.ID != cashclose.DeterministicDrawerID(f.gymID, "drawer:north") {
		t.Fatalf("drawer id=%s is not deterministic", created.ID)
	}

	if _, err := uc.Create(ctx, billingApp.CreateCashDrawerInput{GymID: f.gymID,
		ActorUserID: f.ownerID, ActorRole: "owner", Name: "Sin actividad",
		IdempotencyKey: "drawer:unused"}); err != nil {
		t.Fatal(err)
	}
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithCashDrawers(repo)
	report, err := cash.Report(ctx, reportsApp.CashCloseReportInput{GymID: f.gymID, Date: time.Now().UTC()})
	if err != nil || len(report.Drawers) != 3 {
		t.Fatalf("stable drawer report=%+v err=%v", report, err)
	}
	foundUnused := false
	for _, drawer := range report.Drawers {
		if drawer.Name == "Sin actividad" {
			foundUnused = !drawer.HasActivity && drawer.SessionCount == 0 && drawer.Active
		}
	}
	if !foundUnused {
		t.Fatalf("unused catalog drawer missing from report: %+v", report.Drawers)
	}

	var queued int
	if err := f.db.Get(&queued, `SELECT COUNT(*) FROM sync_queue
		WHERE entity_type='cash_drawers' AND entity_id=? AND synced_at IS NULL`, created.ID.String()); err != nil || queued != 1 {
		t.Fatalf("offline cash drawer queue=%d err=%v", queued, err)
	}
}

func TestCashDrawers_DeactivationProtectsMainAndUnfinishedPhysicalSession(t *testing.T) {
	f := setupSales(t)
	repo := billingRepos.NewCashDrawerSQLiteRepository()
	drawers := billingApp.NewCashDrawers(repo, f.uow, f.recorder)
	ctx := context.Background()
	created, err := drawers.Create(ctx, billingApp.CreateCashDrawerInput{GymID: f.gymID,
		ActorUserID: f.ownerID, ActorRole: "owner", Name: "Caja barra", IdempotencyKey: "drawer:bar"})
	if err != nil {
		t.Fatal(err)
	}

	zero := 0.0
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithCashDrawers(repo)
	session, err := cash.Close(ctx, reportsApp.CashCloseInput{GymID: f.gymID,
		ActorUserID: f.ownerID, Date: time.Now().UTC(), DrawerID: &created.ID, CountedCash: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drawers.Deactivate(ctx, f.gymID, f.ownerID, "owner", created.ID, created.Version); !errors.Is(err, cashclose.ErrDrawerHasOpenActivity) {
		t.Fatalf("deactivate unfinished drawer error=%v", err)
	}
	if _, err := cash.Withdraw(ctx, reportsApp.CashWithdrawInput{GymID: f.gymID,
		ActorUserID: f.ownerID, SessionID: session.CashCloseID, CashLeft: 0,
		Destination: cashclose.DestinationGymFund}); err != nil {
		t.Fatal(err)
	}
	deactivated, err := drawers.Deactivate(ctx, f.gymID, f.ownerID, "owner", created.ID, created.Version)
	if err != nil || deactivated.Active || deactivated.Version != 2 {
		t.Fatalf("deactivated drawer=%+v err=%v", deactivated, err)
	}
	if _, err := cash.Close(ctx, reportsApp.CashCloseInput{GymID: f.gymID,
		ActorUserID: f.ownerID, Date: time.Now().UTC(), DrawerID: &created.ID}); !errors.Is(err, cashclose.ErrDrawerInactive) {
		t.Fatalf("inactive drawer close error=%v", err)
	}
	if _, err := drawers.Deactivate(ctx, f.gymID, f.ownerID, "owner", f.gymID, 1); !errors.Is(err, cashclose.ErrMainDrawerDeactivate) {
		t.Fatalf("main drawer deactivate error=%v", err)
	}
	if _, err := drawers.Update(ctx, billingApp.UpdateCashDrawerInput{GymID: f.gymID,
		ActorUserID: uuid.New(), ActorRole: "operator", DrawerID: created.ID,
		ExpectedVersion: deactivated.Version, Active: boolPtr(true)}); !errors.Is(err, cashclose.ErrDrawerOwnerRequired) {
		t.Fatalf("operator update error=%v", err)
	}
}

func TestCashDrawerValidator_RejectsInactiveMissingAndCrossGymBeforeCollection(t *testing.T) {
	f := setupSales(t)
	repo := billingRepos.NewCashDrawerSQLiteRepository()
	catalog := billingApp.NewCashDrawers(repo, f.uow, f.recorder)
	drawer, err := catalog.Create(context.Background(), billingApp.CreateCashDrawerInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner",
		Name: "Caja temporal", IdempotencyKey: "drawer:validation",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a catalog change arriving from another device after commit.
	// Normal UI deactivation is correctly blocked while this drawer has open
	// cash activity, but replay ordering must remain correct under convergence.
	if _, err := f.db.Exec(`UPDATE cash_drawers SET active=0,version=version+1 WHERE id=?`, drawer.ID.String()); err != nil {
		t.Fatal(err)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateActiveCashDrawer(tx, f.gymID, drawer.ID); !errors.Is(err, cashclose.ErrDrawerInactive) {
		t.Fatalf("inactive drawer error=%v", err)
	}
	if err := catalog.ValidateActiveCashDrawer(tx, f.gymID, uuid.New()); !errors.Is(err, cashclose.ErrDrawerNotFound) {
		t.Fatalf("missing drawer error=%v", err)
	}
	if err := catalog.ValidateActiveCashDrawer(tx, uuid.New(), drawer.ID); !errors.Is(err, cashclose.ErrDrawerNotFound) {
		t.Fatalf("cross-gym drawer error=%v", err)
	}
	if err := catalog.ValidateActiveCashDrawer(tx, f.gymID, f.gymID); err != nil {
		t.Fatalf("deterministic legacy main drawer rejected: %v", err)
	}

	_, err = f.registerPayment().WithCashDrawers(catalog).Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		MembershipTypeID: f.planID, Method: "cash", CashDrawerID: &drawer.ID,
		IdempotencyKey: "inactive-drawer-membership",
	})
	if !errors.Is(err, cashclose.ErrDrawerInactive) {
		t.Fatalf("membership inactive drawer error=%v", err)
	}
	var payments int
	if err := f.db.Get(&payments, `SELECT COUNT(*) FROM payments WHERE idempotency_key='inactive-drawer-membership'`); err != nil {
		t.Fatal(err)
	}
	if payments != 0 {
		t.Fatalf("rejected drawer left %d payment rows", payments)
	}
}

func TestCashDrawerValidation_DoesNotReopenCommittedIdempotentReplay(t *testing.T) {
	f := setupSales(t)
	repo := billingRepos.NewCashDrawerSQLiteRepository()
	catalog := billingApp.NewCashDrawers(repo, f.uow, f.recorder)
	drawer, err := catalog.Create(context.Background(), billingApp.CreateCashDrawerInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner",
		Name: "Caja de replay", IdempotencyKey: "drawer:replay",
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := f.registerPayment().WithCashDrawers(catalog)
	in := billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		MembershipTypeID: f.planID, Method: "cash", CashDrawerID: &drawer.ID,
		IdempotencyKey: "drawer-payment-replay",
	}
	first, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a catalog change arriving from another device after commit.
	// Normal UI deactivation is correctly blocked while physical activity is
	// open; replay still must return the already committed response.
	if _, err := f.db.Exec(`UPDATE cash_drawers SET active=0,version=version+1 WHERE id=?`, drawer.ID.String()); err != nil {
		t.Fatal(err)
	}
	replayed, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("exact replay revalidated now-inactive drawer: %v", err)
	}
	if replayed.PaymentID != first.PaymentID || replayed.Folio != first.Folio {
		t.Fatalf("replay=%+v first=%+v", replayed, first)
	}
}

func boolPtr(value bool) *bool { return &value }
