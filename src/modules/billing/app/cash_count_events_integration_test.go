//go:build sidecar

package app_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
)

type recordingCashSubscriber struct {
	events []reportsApp.CashCloseClosedEvent
}

func (r *recordingCashSubscriber) OnCashCloseClosed(_ context.Context, evt reportsApp.CashCloseClosedEvent) {
	r.events = append(r.events, evt)
}

func TestCashSession_CountEventsCarrySessionNamesAndRecounts(t *testing.T) {
	ctx := context.Background()
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	customDrawerID := uuid.New()
	drawers := seedCustomCashDrawer(t, f, customDrawerID, "Caja 2")
	day := cashSessionBusinessDay(t)
	registerCashProductSale(t, f, productID)
	if _, err := f.registerSale().Execute(ctx, billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", CashDrawerID: &customDrawerID, PaymentDate: day,
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	events := &recordingCashSubscriber{}
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithCashDrawers(drawers).WithUsers(usersRepoLite.NewUserSQLiteRepository()).WithSubscriber(events)

	matched, short, zero := 20.0, 15.0, 0.0
	reason := "no había cambio"
	mainClose, err := cash.Close(ctx, reportsApp.CashCloseInput{GymID: f.gymID, ActorUserID: f.ownerID,
		Date: day, OpeningCash: &zero, CountedCash: &matched})
	if err != nil {
		t.Fatal(err)
	}
	customClose, err := cash.Close(ctx, reportsApp.CashCloseInput{GymID: f.gymID, ActorUserID: f.ownerID,
		Date: day, DrawerID: &customDrawerID, OpeningCash: &zero, CountedCash: &short, DiscrepancyReason: &reason})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.events) != 2 {
		t.Fatalf("close events = %+v, want one per close", events.events)
	}
	if got := events.events[0]; got.SessionID != mainClose.CashCloseID || got.Discrepancy == nil ||
		*got.Discrepancy != 0 || got.CountedByName != nil {
		t.Fatalf("matched close event = %+v; names are only looked up for a difference", got)
	}
	if got := events.events[1]; got.SessionID != customClose.CashCloseID || got.Discrepancy == nil ||
		*got.Discrepancy != -5 || got.CountedByName == nil || *got.CountedByName != "Owner" ||
		got.DrawerName == nil || *got.DrawerName != "Caja 2" || got.Reason == nil || *got.Reason != reason ||
		!got.CloseDate.Equal(day) {
		t.Fatalf("short close event = %+v", got)
	}

	// A late sale outdates the main count; the recount is a new result the
	// owner must hear about.
	registerCashProductSale(t, f, productID)
	recountReason := "faltan 10"
	view, err := cash.Reconcile(ctx, reportsApp.CashReconcileInput{GymID: f.gymID, ActorUserID: f.ownerID,
		ActorRole: "operator", SessionID: mainClose.CashCloseID, CountedCash: 30, DiscrepancyReason: &recountReason})
	if err != nil || view.Difference == nil || *view.Difference != -10 {
		t.Fatalf("recount view=%+v err=%v", view, err)
	}
	if len(events.events) != 3 {
		t.Fatalf("recount must publish a count event, got %+v", events.events)
	}
	if got := events.events[2]; got.SessionID != mainClose.CashCloseID || got.Discrepancy == nil ||
		*got.Discrepancy != -10 || got.CountedByName == nil || *got.CountedByName != "Owner" ||
		got.DrawerName != nil || got.Reason == nil || *got.Reason != recountReason {
		t.Fatalf("recount event = %+v; main drawer carries no drawer name", got)
	}

	// The owner correcting a certified count already knows about it.
	correction := "capturé mal el conteo"
	if _, err := cash.Reconcile(ctx, reportsApp.CashReconcileInput{GymID: f.gymID, ActorUserID: f.ownerID,
		ActorRole: "owner", SessionID: mainClose.CashCloseID, CountedCash: 40, CorrectionReason: &correction}); err != nil {
		t.Fatal(err)
	}
	if len(events.events) != 3 {
		t.Fatalf("owner correction must not alert the owner: %+v", events.events[3:])
	}
}
