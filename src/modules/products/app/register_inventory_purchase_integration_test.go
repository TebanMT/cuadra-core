//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	expenseApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	expenseRepos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	gymRepos "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	repos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestPurchaseRegistrationStatesAndReplay(t *testing.T) {
	for _, received := range []bool{false, true} {
		for _, paid := range []bool{false, true} {
			name := "pending"
			if received {
				name = "received"
			}
			if paid {
				name += "_paid"
			} else {
				name += "_unpaid"
			}
			t.Run(name, func(t *testing.T) {
				f := setupProducts(t)
				ctx := context.Background()
				a, e := f.createProductUC().Execute(ctx, prodApp.CreateProductInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: "Agua", Price: 20})
				if e != nil {
					t.Fatal(e)
				}
				b, e := f.createProductUC().Execute(ctx, prodApp.CreateProductInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: "Barra", Price: 30})
				if e != nil {
					t.Fatal(e)
				}
				cash := expenseApp.NewInventoryPurchaseCashService(expenseRepos.NewCashMovementSQLiteRepository())
				uc := prodApp.NewRegisterInventoryPurchase(repos.NewInventoryPurchaseSQLiteRepository(), repos.NewInventoryPurchaseReceiptSQLiteRepository(), f.productRepo, gymRepos.NewGymSQLiteRepository(), cash, f.uow, f.recorder, "desktop")
				in := prodApp.RegisterInventoryPurchaseInput{ID: uuid.New(), GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", Received: received, Paid: paid, Items: []prodApp.PurchaseLineInput{{ProductID: a.ProductID, Quantity: 20, UnitCost: 10}, {ProductID: b.ProductID, Quantity: 5, UnitCost: 12.50}}}
				if paid {
					in.PaidOn = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
					in.PaymentMethod = "cash"
					in.PaidFrom = "cash_drawer"
				}
				out, e := uc.Execute(ctx, in)
				if e != nil {
					t.Fatal(e)
				}
				if len(out) != 2 {
					t.Fatal(out)
				}
				for range 2 {
					if _, e = uc.Execute(ctx, in); e != nil {
						t.Fatal(e)
					}
				}
				var count int
				f.db.Get(&count, "SELECT COUNT(*) FROM inventory_purchases")
				if count != 2 {
					t.Fatalf("purchases %d", count)
				}
				f.db.Get(&count, "SELECT COUNT(*) FROM inventory_purchase_receipts")
				want := 0
				if received {
					want = 2
				}
				if count != want {
					t.Fatalf("receipts %d", count)
				}
				f.db.Get(&count, "SELECT COUNT(*) FROM cash_movements WHERE deleted_at IS NULL")
				want = 0
				if paid {
					want = 2
				}
				if count != want {
					t.Fatalf("cash %d", count)
				}
				var stock int
				f.db.Get(&stock, "SELECT stock FROM products WHERE id=?", a.ProductID.String())
				want = 0
				if received {
					want = 20
				}
				if stock != want {
					t.Fatalf("stock %d", stock)
				}
				altered := in
				altered.Received = !received
				if _, e = uc.Execute(ctx, altered); e == nil {
					t.Fatal("changed retry accepted")
				}
				// An invalid second line rolls back the first product, receipt and cash.
				broken := in
				broken.ID = uuid.New()
				broken.Items = append([]prodApp.PurchaseLineInput(nil), in.Items...)
				broken.Items[1].UnitCost = 0
				if _, e = uc.Execute(ctx, broken); e == nil {
					t.Fatal("bad cost accepted")
				}
				f.db.Get(&count, "SELECT COUNT(*) FROM inventory_purchases")
				if count != 2 {
					t.Fatalf("partial purchase persisted: %d", count)
				}
				f.db.Get(&count, "SELECT COUNT(*) FROM pragma_foreign_key_check")
				if count != 0 {
					t.Fatalf("broken foreign keys: %d", count)
				}
				if !paid {
					in.ID = uuid.New()
					in.ActorRole = "operator"
					if _, e = uc.Execute(ctx, in); e != nil {
						t.Fatal(e)
					}
					in.ID = uuid.New()
					in.Paid = true
					if _, e = uc.Execute(ctx, in); e == nil {
						t.Fatal("operator recorded payment")
					}
				}
			})
		}
	}
}

func TestPurchaseRegistrationCreatesProductsAtomically(t *testing.T) {
	for _, received := range []bool{false, true} {
		for _, paid := range []bool{false, true} {
			t.Run(fmt.Sprintf("received_%t_paid_%t", received, paid), func(t *testing.T) {
				f := setupProducts(t)
				ctx := context.Background()
				cash := expenseApp.NewInventoryPurchaseCashService(expenseRepos.NewCashMovementSQLiteRepository())
				uc := prodApp.NewRegisterInventoryPurchase(repos.NewInventoryPurchaseSQLiteRepository(), repos.NewInventoryPurchaseReceiptSQLiteRepository(), f.productRepo, gymRepos.NewGymSQLiteRepository(), cash, f.uow, f.recorder, "desktop")
				id := uuid.New()
				in := prodApp.RegisterInventoryPurchaseInput{ID: uuid.New(), GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", Received: received, Paid: paid, Items: []prodApp.PurchaseLineInput{{ProductID: id, Quantity: 12, UnitCost: 10, NewProduct: &prodApp.PurchaseProductInput{Name: "Agua nueva", Price: 20}}}}
				if paid {
					in.PaidOn = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
					in.PaymentMethod = "cash"
					in.PaidFrom = "cash_drawer"
				}
				out, err := uc.Execute(ctx, in)
				if err != nil {
					t.Fatal(err)
				}
				if len(out) != 1 || out[0].ProductName != "Agua nueva" {
					t.Fatal(out)
				}
				for range 2 {
					if _, err = uc.Execute(ctx, in); err != nil {
						t.Fatal(err)
					}
				}
				var count, stock int
				assertCount := func(table string, want int) {
					t.Helper()
					if e := f.db.Get(&count, "SELECT COUNT(*) FROM "+table); e != nil {
						t.Fatal(e)
					}
					if count != want {
						t.Fatalf("%s: got %d want %d", table, count, want)
					}
				}
				assertCount("products", 1)
				assertCount("inventory_purchases", 1)
				want := 0
				if received {
					want = 12
				}
				if e := f.db.Get(&stock, "SELECT stock FROM products WHERE id=?", id.String()); e != nil {
					t.Fatal(e)
				}
				if stock != want {
					t.Fatalf("stock %d want %d", stock, want)
				}
				want = 0
				if received {
					want = 1
				}
				assertCount("stock_movements", want) // Only the receipt movement, never a second initial-stock entry.
				want = 0
				if received {
					want = 1
				}
				assertCount("inventory_purchase_receipts", want)
				want = 0
				if paid {
					want = 1
				}
				assertCount("cash_movements", want)
				var queued int
				if e := f.db.Get(&queued, "SELECT COUNT(*) FROM sync_queue"); e != nil {
					t.Fatal(e)
				}
				// Invalid line two rolls back the new catalog item and all financial/sync effects.
				bad := in
				bad.ID = uuid.New()
				bad.Items = []prodApp.PurchaseLineInput{{ProductID: uuid.New(), Quantity: 2, UnitCost: 8, NewProduct: &prodApp.PurchaseProductInput{Name: "No debe quedar", Price: 18}}, {ProductID: id, Quantity: 2, UnitCost: 0}}
				if _, err = uc.Execute(ctx, bad); err == nil {
					t.Fatal("invalid cost accepted")
				}
				assertCount("products", 1)
				assertCount("inventory_purchases", 1)
				assertCount("sync_queue", queued)
				// Client-supplied ID must never mutate an existing catalog item.
				bad.Items = []prodApp.PurchaseLineInput{{ProductID: id, Quantity: 2, UnitCost: 8, NewProduct: &prodApp.PurchaseProductInput{Name: "Overwrite", Price: 99}}}
				if _, err = uc.Execute(ctx, bad); err == nil {
					t.Fatal("existing ID overwritten")
				}
				// Duplicate names and invalid sale prices cannot create orphan products.
				for _, details := range []prodApp.PurchaseProductInput{{Name: " agua nueva ", Price: 22}, {Name: "Otro", Price: 0}, {Name: "Otro", Price: 0.001}} {
					bad.ID = uuid.New()
					bad.Items = []prodApp.PurchaseLineInput{{ProductID: uuid.New(), Quantity: 2, UnitCost: 8, NewProduct: &details}}
					if _, err = uc.Execute(ctx, bad); err == nil {
						t.Fatal("invalid new product accepted", details)
					}
					assertCount("products", 1)
					assertCount("inventory_purchases", 1)
				}
				// The fingerprint includes new-product fields, even after creation.
				changed := in
				changed.Items = append([]prodApp.PurchaseLineInput(nil), in.Items...)
				changed.Items[0].NewProduct = &prodApp.PurchaseProductInput{Name: "Agua nueva", Price: 25}
				if _, err = uc.Execute(ctx, changed); err == nil {
					t.Fatal("changed price accepted on replay")
				}
			})
		}
	}
}

func TestPurchaseRegistrationLegacyLineFingerprint(t *testing.T) {
	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	data, err := json.Marshal(prodApp.PurchaseLineInput{ProductID: id, Quantity: 2, UnitCost: 10})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"product_id":"11111111-1111-4111-8111-111111111111","quantity":2,"unit_cost":10}` {
		t.Fatalf("legacy request fingerprint changed: %s", data)
	}
}
