//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	expenseApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	expenseRepos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	gymRepos "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	repos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	"github.com/google/uuid"
)

func TestPurchaseRegistrationPaymentPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, origin, role, method, source string
		paid, allowed                      bool
	}{
		{"reception_pending", "desktop", "operator", "", "", false, true},
		{"reception_cash", "desktop", "operator", "cash", "cash_drawer", true, true},
		{"reception_transfer_denied", "desktop", "operator", "transfer", "gym_fund", true, false},
		{"reception_external_cash_denied", "desktop", "operator", "cash", "external", true, false},
		{"reception_card_from_drawer_denied", "desktop", "operator", "card", "cash_drawer", true, false},
		{"cloud_reception_cash_denied", "cloud", "operator", "cash", "cash_drawer", true, false},
		{"cloud_reception_pending_denied", "cloud", "operator", "", "", false, false},
		{"unknown_role_denied", "desktop", "visitor", "cash", "cash_drawer", true, false},
		{"owner_cash", "desktop", "owner", "cash", "cash_drawer", true, true},
		{"cloud_owner_cash", "cloud", "owner", "cash", "cash_drawer", true, true},
		{"cloud_owner_card_from_drawer_denied", "cloud", "owner", "card", "cash_drawer", true, false},
		{"cloud_owner_transfer_from_drawer_denied", "cloud", "owner", "transfer", "cash_drawer", true, false},
		{"cloud_owner_transfer", "cloud", "owner", "transfer", "gym_fund", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupProducts(t)
			cash := expenseApp.NewInventoryPurchaseCashService(expenseRepos.NewCashMovementSQLiteRepository())
			uc := prodApp.NewRegisterInventoryPurchase(repos.NewInventoryPurchaseSQLiteRepository(), repos.NewInventoryPurchaseReceiptSQLiteRepository(), f.productRepo, gymRepos.NewGymSQLiteRepository(), cash, f.uow, f.recorder, tc.origin)
			in := prodApp.RegisterInventoryPurchaseInput{ID: uuid.New(), GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: tc.role,
				Received: true, Paid: tc.paid, PaymentMethod: tc.method, PaidFrom: tc.source,
				Items: []prodApp.PurchaseLineInput{{ProductID: uuid.New(), Quantity: 2, UnitCost: 12.50, NewProduct: &prodApp.PurchaseProductInput{Name: "Agua", Price: 20}}}}
			if tc.paid {
				in.PaidOn = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			}
			_, err := uc.Execute(context.Background(), in)
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("unauthorized payment accepted")
			}
			if tc.role != "owner" && !errors.Is(err, prodErrors.ErrPurchaseOwnerRequired) {
				t.Fatalf("wrong permission error: %v", err)
			}
			for _, table := range []string{"products", "inventory_purchases", "inventory_purchase_receipts", "cash_movements"} {
				var count int
				if e := f.db.Get(&count, "SELECT COUNT(*) FROM "+table); e != nil || count != 0 {
					t.Fatalf("%s count=%d err=%v", table, count, e)
				}
			}
		})
	}
}

func TestReceptionPurchaseCashAtomicityAndReplay(t *testing.T) {
	f := setupProducts(t)
	ctx := context.Background()
	operator := uuid.New()
	now := time.Now().UTC().UnixMilli()
	if _, err := f.db.Exec(`INSERT INTO users (id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role,active) VALUES (?,?,1,?,?,?,'unused','Reception','operator',1)`, operator, f.gymID, now, now, operator.String()+"@test.local"); err != nil {
		t.Fatal(err)
	}
	cash := expenseApp.NewInventoryPurchaseCashService(expenseRepos.NewCashMovementSQLiteRepository())
	uc := prodApp.NewRegisterInventoryPurchase(repos.NewInventoryPurchaseSQLiteRepository(), repos.NewInventoryPurchaseReceiptSQLiteRepository(), f.productRepo, gymRepos.NewGymSQLiteRepository(), cash, f.uow, f.recorder, "desktop")
	in := prodApp.RegisterInventoryPurchaseInput{ID: uuid.New(), GymID: f.gymID, ActorUserID: operator, ActorRole: "operator", Received: true, Paid: true,
		PaidOn: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), PaymentMethod: "cash", PaidFrom: "cash_drawer",
		Items: []prodApp.PurchaseLineInput{
			{ProductID: uuid.New(), Quantity: 20, UnitCost: 10, NewProduct: &prodApp.PurchaseProductInput{Name: "Agua", Price: 20}},
			{ProductID: uuid.New(), Quantity: 5, UnitCost: 12.50, NewProduct: &prodApp.PurchaseProductInput{Name: "Barra", Price: 30}},
		}}
	count := func(query string) int {
		t.Helper()
		var n int
		if err := f.db.Get(&n, query); err != nil {
			t.Fatal(err)
		}
		return n
	}
	beforeQueue := count("SELECT COUNT(*) FROM sync_queue")
	broken := in
	broken.Items = append([]prodApp.PurchaseLineInput(nil), in.Items...)
	broken.Items[1].UnitCost = 0
	if _, err := uc.Execute(ctx, broken); err == nil {
		t.Fatal("invalid second line accepted")
	}
	for _, table := range []string{"products", "inventory_purchases", "inventory_purchase_receipts", "cash_movements"} {
		if n := count("SELECT COUNT(*) FROM " + table); n != 0 {
			t.Fatalf("partial %s: %d", table, n)
		}
	}
	if count("SELECT COUNT(*) FROM sync_queue") != beforeQueue {
		t.Fatal("rolled back purchase left sync events")
	}
	for range 3 {
		if _, err := uc.Execute(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	// A retry by the owner must retain the original operator and payment.
	retry := in
	retry.ActorUserID = f.ownerID
	retry.ActorRole = "owner"
	if _, err := uc.Execute(ctx, retry); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"products", "inventory_purchases", "inventory_purchase_receipts", "cash_movements"} {
		if n := count("SELECT COUNT(*) FROM " + table); n != 2 {
			t.Fatalf("duplicate/missing %s: %d", table, n)
		}
	}
	if count("SELECT SUM(amount) FROM cash_movements") != 26250 {
		t.Fatal("wrong cash outflow")
	}
	if count("SELECT SUM(stock) FROM products") != 25 {
		t.Fatal("wrong inventory")
	}
	if count("SELECT COUNT(*) FROM expenses") != 0 {
		t.Fatal("purchase also recorded as a general expense")
	}
	if count(`SELECT COUNT(*) FROM inventory_purchases p JOIN cash_movements c ON c.id=p.cash_movement_id WHERE p.created_by=c.operator_id AND c.classification_status='inventory_purchase' AND c.expense_id IS NULL AND p.status='paid'`) != 2 {
		t.Fatal("purchase and cash were not linked to the operator")
	}
	if count(`SELECT COUNT(*) FROM inventory_purchases WHERE created_by='`+operator.String()+`'`) != 2 {
		t.Fatal("retry changed original operator")
	}
	for _, entity := range []string{"products", "inventory_purchases", "inventory_purchase_receipts", "cash_movements"} {
		if count("SELECT COUNT(*) FROM sync_queue WHERE entity_type='"+entity+"'") != 2 {
			t.Fatalf("sync events duplicated for %s", entity)
		}
	}
	if count("SELECT COUNT(*) FROM pragma_foreign_key_check") != 0 {
		t.Fatal("broken foreign keys")
	}
}
