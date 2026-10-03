//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	reportRepo "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	gymRepos "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	app "github.com/cuadra/cuadra-core/src/modules/products/app"
	repos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestCompleteLegacyCost_UpdatesHistoricalReportWithoutPhysicalChanges(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "unlinked"
		if existing {
			name = "legacy_purchase"
		}
		t.Run(name, func(t *testing.T) {
			f := setupProducts(t)
			ctx := context.Background()
			product, err := f.createProductUC().Execute(ctx, app.CreateProductInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: "Agua", Price: 20, InitialStock: 16})
			if err != nil {
				t.Fatal(err)
			}
			// Reproduce the old physical-only purchase, late evening in the gym.
			instant := time.Date(2026, 9, 5, 3, 30, 0, 0, time.UTC)
			var mid string
			if err = f.db.Get(&mid, `SELECT id FROM stock_movements WHERE product_id=?`, product.ProductID.String()); err != nil {
				t.Fatal(err)
			}
			if _, err = f.db.Exec(`UPDATE stock_movements SET is_purchase=1,cost=NULL,created_at=?,updated_at=? WHERE id=?`, instant.UnixMilli(), instant.UnixMilli(), mid); err != nil {
				t.Fatal(err)
			}
			if existing {
				_, err = f.db.Exec(`INSERT INTO inventory_purchases(id,gym_id,version,created_at,updated_at,stock_movement_id,product_id,quantity,status,idempotency_key,created_by) VALUES(?,?,1,?,?,?,?,16,'legacy_incomplete',?,?)`, uuid.NewString(), f.gymID.String(), instant.UnixMilli(), instant.UnixMilli(), mid, product.ProductID.String(), "legacy:"+mid, f.ownerID.String())
				if err != nil {
					t.Fatal(err)
				}
			}
			repo := repos.NewInventoryPurchaseSQLiteRepository()
			list := app.NewListMissingPurchaseCosts(repo, gymRepos.NewGymSQLiteRepository(), f.uow)
			day := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
			query := app.MissingPurchaseCostsInput{GymID: f.gymID, From: day, To: day, Page: 1}
			rows, err := list.Execute(ctx, query)
			if err != nil || rows.Total != 1 || len(rows.Items) != 1 || rows.Items[0].RecordedOn != "2026-09-04" {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
			in := app.CompleteLegacyPurchaseCostInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", MovementID: uuid.MustParse(mid), ExpectedVersion: rows.Items[0].Version, UnitCost: 12.35, Reason: "Factura 204"}
			uc := app.NewCompleteLegacyPurchaseCost(repo, repo, f.uow, f.recorder)
			forbidden := in
			forbidden.ActorRole = "operator"
			if uc.Execute(ctx, forbidden) == nil {
				t.Fatal("operator accepted")
			}
			foreign := in
			foreign.GymID = uuid.New()
			if uc.Execute(ctx, foreign) == nil {
				t.Fatal("foreign gym accepted")
			}
			bad := in
			bad.UnitCost = 0
			if uc.Execute(ctx, bad) == nil {
				t.Fatal("zero accepted")
			}
			stale := in
			stale.ExpectedVersion++
			if uc.Execute(ctx, stale) == nil {
				t.Fatal("stale version accepted")
			}
			for range 2 {
				if err = uc.Execute(ctx, in); err != nil {
					t.Fatal(err)
				}
			}
			changed := in
			changed.UnitCost = 13
			if uc.Execute(ctx, changed) == nil {
				t.Fatal("changed retry overwrote documented cost")
			}
			rows, err = list.Execute(ctx, query)
			if err != nil || rows.Total != 0 {
				t.Fatalf("still missing: %+v %v", rows, err)
			}
			var n int
			for table, want := range map[string]int{"inventory_purchases": 1, "stock_movements": 1, "cash_movements": 0, "payments": 0} {
				if err = f.db.Get(&n, "SELECT COUNT(*) FROM "+table+" WHERE gym_id=?", f.gymID.String()); err != nil || n != want {
					t.Fatalf("%s=%d want %d err=%v", table, n, want, err)
				}
			}
			if err = f.db.Get(&n, `SELECT stock FROM products WHERE id=?`, product.ProductID.String()); err != nil || n != 16 {
				t.Fatalf("stock=%d err=%v", n, err)
			}
			if err = f.db.Get(&n, `SELECT COUNT(*) FROM audit_log WHERE gym_id=? AND action='complete_legacy_cost'`, f.gymID.String()); err != nil || n != 1 {
				t.Fatalf("audit count=%d err=%v", n, err)
			}
			if err = f.db.Get(&n, `SELECT total_amount FROM inventory_purchases WHERE gym_id=?`, f.gymID.String()); err != nil || n != 19760 {
				t.Fatalf("cents=%d err=%v", n, err)
			}
			err = shared.ReadSnapshot(ctx, f.uow, func(tx shared.Transaction) error {
				snapshot, e := reportRepo.NewSQLiteReader().CanonicalFinancialBetween(tx, f.gymID, "America/Mexico_City", day, day)
				if e != nil {
					return e
				}
				if snapshot.LegacyPurchaseCount != 0 || snapshot.InventoryPurchases != 197.60 {
					t.Fatalf("report=%+v", snapshot)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var payload string
			if err = f.db.Get(&payload, `SELECT payload FROM sync_queue WHERE json_extract(payload,'$.gym_id')=? AND entity_type='inventory_purchases' ORDER BY enqueued_at DESC LIMIT 1`, f.gymID.String()); err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err = json.Unmarshal([]byte(payload), &wire); err != nil {
				t.Fatal(err)
			}
			if wire["total_amount"] != 197.60 || wire["unit_cost"] != 12.35 || wire["stock_movement_id"] != mid || wire["cash_movement_id"] != nil {
				t.Fatalf("sync payload=%s", payload)
			}
		})
	}
}
