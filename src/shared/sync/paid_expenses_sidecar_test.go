//go:build sidecar

package sync_test

import (
	"context"
	reports "github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	gymRepos "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	repos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestPaidExpenses_RemoteReceiptRetriesKeepOneEconomicPayment(t *testing.T) {
	f := remoteSidecar(t)
	uc := reports.NewPaidExpenses(reportsInfra.NewSQLiteReader(), f.uow, gymRepos.NewGymSQLiteRepository())
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	in := reports.PaidExpensesInput{GymID: f.gym, From: day, To: day}
	assertPaid := func() {
		t.Helper()
		out, err := uc.Execute(context.Background(), in)
		if err != nil || out.Total != 1 || out.TotalAmount != 234.5 || out.Items[0].MovementID != f.order.String() {
			t.Fatalf("paid ledger: %+v %v", out, err)
		}
	}
	assertPaid()
	receive := prodApp.NewReceiveInventoryPurchase(repos.NewInventoryPurchaseSQLiteRepository(), repos.NewInventoryPurchaseReceiptSQLiteRepository(), f.uow, audit.NewSQLiteRecorder())
	for range 3 {
		if _, err := receive.Execute(context.Background(), prodApp.ReceiveInventoryPurchaseInput{GymID: f.gym, PurchaseID: f.order, ActorUserID: f.user, ActorRole: "operator", Quantity: 10}); err != nil {
			t.Fatal(err)
		}
		assertPaid()
	}
	f.assertStock(t, 15)
	in.GymID = uuid.New()
	out, err := uc.Execute(context.Background(), in)
	if err != nil || out.Total != 0 || out.TotalAmount != 0 {
		t.Fatalf("cross-tenant ledger: %+v %v", out, err)
	}
}
