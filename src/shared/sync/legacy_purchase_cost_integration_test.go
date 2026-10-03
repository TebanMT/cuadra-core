//go:build server && integration

package sync

import (
	"context"
	"encoding/json"
	reportRepo "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	gymRepos "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	app "github.com/cuadra/cuadra-core/src/modules/products/app"
	product "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	stock "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	repos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	controllers "github.com/cuadra/cuadra-core/src/modules/products/interfaces/controllers"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"net/http"
	"testing"
	"time"
)

func TestLegacyPurchaseCost_HTTPAndSyncPreserveReceipt(t *testing.T) {
	for _, origin := range []string{"cloud", "desktop"} {
		t.Run(origin, func(t *testing.T) {
			db := projectorTestDB(t)
			gym, user := seedGymAndOwner(t, db)
			ctx := context.Background()
			uow := shared.NewPostgresUnitOfWork(db)
			pid, mid := uuid.New(), uuid.New()
			instant := time.Date(2026, 9, 5, 3, 30, 0, 0, time.UTC)
			day := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
			err := uow.Command(ctx, func(tx shared.Transaction) error {
				p, e := product.New(pid, gym, "Agua", 20, 16, 0, nil, nil, instant)
				if e != nil {
					return e
				}
				if _, e = repos.NewProductPostgresRepository().Create(tx, p); e != nil {
					return e
				}
				m, e := stock.New(mid, gym, pid, user, "restock", 16, nil, nil, nil, instant)
				if e != nil {
					return e
				}
				_, e = repos.NewStockMovementPostgresRepository().Create(tx, m)
				return e
			})
			if err != nil {
				t.Fatal(err)
			}
			router, tokens := newRealHandler(t, db)
			token, _ := tokens.GenerateAccessToken(user, gym, "owner")
			repo := repos.NewInventoryPurchasePostgresRepository()
			list := app.NewListMissingPurchaseCosts(repo, gymRepos.NewGymPostgresRepository(), uow)
			complete := app.NewCompleteLegacyPurchaseCost(repo, repo, uow, audit.NewPostgresRecorder())
			controllers.NewProductController(nil, nil, nil, nil, nil, nil, tokens).WithLegacyPurchaseCosts(list, complete).RegisterRoutes(router)
			path := "/api/v1/inventory-purchase-missing-costs"
			result := doJSONReq(t, router, http.MethodGet, path+"?from=2026-09-04&to=2026-09-04", token, nil)
			if result.Code != 200 {
				t.Fatalf("list %d %s", result.Code, result.Body.String())
			}
			rows, e := list.Execute(ctx, app.MissingPurchaseCostsInput{GymID: gym, From: day, To: day, Page: 1})
			if e != nil || rows.Total != 1 || rows.Items[0].RecordedOn != "2026-09-04" {
				t.Fatalf("list=%+v err=%v", rows, e)
			}
			body := map[string]any{"version": 1, "unit_cost": 12.35, "reason": "Factura 204"}
			operator, _ := tokens.GenerateAccessToken(user, gym, "operator")
			if r := doJSONReq(t, router, http.MethodPost, path+"/"+mid.String()+"/complete", operator, body); r.Code != 403 {
				t.Fatalf("operator code=%d", r.Code)
			}
			if origin == "cloud" {
				for range 2 {
					r := doJSONReq(t, router, http.MethodPost, path+"/"+mid.String()+"/complete", token, body)
					if r.Code != 200 {
						t.Fatalf("complete %d %s", r.Code, r.Body.String())
					}
				}
			} else {
				// Same canonical payload produced by the desktop repository; replay it
				// through the real HTTP push handler to prove it cannot duplicate stock.
				now := time.Now().UTC()
				id := uuid.NewSHA1(mid, []byte("legacy-purchase-cost"))
				payload := mustJSON(t, map[string]any{"id": id, "gym_id": gym, "version": 1, "created_at": instant.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil, "stock_movement_id": mid, "product_id": pid, "quantity": 16, "unit_cost": 12.35, "total_amount": 197.60, "status": "legacy_incomplete", "paid_on": nil, "payment_method": nil, "paid_from": nil, "cash_movement_id": nil, "idempotency_key": "legacy-purchase-cost:" + mid.String(), "created_by": user})
				request := PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion, Batch: []PushItem{{QueueID: uuid.NewString(), EntityID: id.String(), EntityType: "inventory_purchases", Operation: OpUpsertStr, ClientVersion: 1, Payload: payload, EnqueuedAt: now}}}
				for range 2 {
					response, status, e := performSyncPush(router, token, request)
					if e != nil || status != 200 || len(response.Results) != 1 || response.Results[0].Status == StatusRejectedFinancialConflict {
						t.Fatalf("push status=%d response=%+v err=%v", status, response, e)
					}
				}
			}
			var purchases, movements, cash int64
			db.Table("inventory_purchases").Where("gym_id=?", gym).Count(&purchases)
			db.Table("stock_movements").Where("gym_id=?", gym).Count(&movements)
			db.Table("cash_movements").Where("gym_id=?", gym).Count(&cash)
			var units int
			db.Raw(`SELECT stock FROM products WHERE id=?`, pid).Scan(&units)
			if purchases != 1 || movements != 1 || cash != 0 || units != 16 {
				t.Fatalf("physical changes purchases=%d movements=%d cash=%d units=%d", purchases, movements, cash, units)
			}
			err = shared.ReadSnapshot(ctx, uow, func(tx shared.Transaction) error {
				result, e := reportRepo.NewPostgresReader().CanonicalFinancialBetween(tx, gym, "America/Mexico_City", day, day)
				if e != nil {
					return e
				}
				if result.InventoryPurchases != 197.60 || result.LegacyPurchaseCount != 0 {
					t.Fatalf("report=%+v", result)
				}
				changes, _, e := NewPostgresStore().ListSince(ctx, tx, gym, time.Time{}, 1000)
				if e != nil {
					return e
				}
				found := false
				for _, change := range changes {
					if change.EntityType != "inventory_purchases" {
						continue
					}
					var wire map[string]any
					if e = json.Unmarshal(change.Payload, &wire); e != nil {
						return e
					}
					if wire["total_amount"] != 197.60 || wire["unit_cost"] != 12.35 {
						t.Fatalf("wire=%v", wire)
					}
					found = true
				}
				if !found {
					t.Fatal("completed purchase missing from pull")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
