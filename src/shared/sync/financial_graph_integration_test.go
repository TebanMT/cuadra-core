//go:build server && integration

package sync

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func completeSaleFixture(t *testing.T, db *gorm.DB, gym, owner uuid.UUID, amount float64) []PushItem {
	t.Helper()
	product, payment, sale, line, movement := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO products(id,gym_id,name,price,stock,stock_origin) VALUES(?,?,?, ?,10,10)`, product, gym, "Producto "+product.String(), amount/2).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute).UTC()
	makeItem := func(kind string, id uuid.UUID, raw map[string]any) PushItem {
		raw["id"] = id
		raw["gym_id"] = gym
		raw["version"] = 1
		raw["created_at"] = now.UnixMilli()
		raw["updated_at"] = now.UnixMilli()
		payload, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: kind, EntityID: id.String(), Operation: OpUpsertStr, ClientVersion: 1, Payload: payload, EnqueuedAt: now}
	}
	return []PushItem{
		makeItem("payments", payment, map[string]any{"folio": "PRD-" + payment.String(), "amount": amount, "recognized_amount": amount, "balance_pending": 0, "discount_amount": 0, "concept": "product", "payment_method": "transfer", "payment_date": now.Format("2006-01-02"), "operator_id": owner, "idempotency_result": map[string]any{"SaleID": sale, "Items": []any{map[string]any{"SaleItemID": line}}}}),
		makeItem("sales", sale, map[string]any{"payment_id": payment, "subtotal": amount, "discount": 0, "total": amount, "correction_version": 0}),
		makeItem("sale_items", line, map[string]any{"sale_id": sale, "product_id": product, "product_name_snapshot": "Producto", "unit_price_snapshot": amount / 2, "unit_cost_snapshot": 1, "quantity": 2, "line_total": amount}),
		makeItem("stock_movements", movement, map[string]any{"product_id": product, "movement_type": "sale", "delta": -2, "sale_item_id": line, "operator_id": owner}),
	}
}
func TestFinancialGraphSplitRejectRepairAndLostResponse(t *testing.T) {
	for _, manifest := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "manifest"}[manifest], func(t *testing.T) {
			db := projectorTestDB(t)
			gym, owner := seedGymAndOwner(t, db)
			router, tokens := newRealHandler(t, db)
			token, _ := tokens.GenerateAccessToken(owner, gym, "owner")
			batch := completeSaleFixture(t, db, gym, owner, 39.50)
			keys := []string{}
			for _, item := range batch {
				keys = append(keys, expenseMemberKey(item.EntityType, item.EntityID))
			}
			if manifest {
				for i := range batch {
					raw := decodePushPayload(batch[i].Payload)
					raw[financialMembersKey] = keys
					batch[i].Payload, _ = json.Marshal(raw)
				}
			}
			send := func(items []PushItem) PushResponse {
				t.Helper()
				resp, status, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: items})
				if err != nil || status != 200 {
					t.Fatalf("push %d: %v", status, err)
				}
				return resp
			}
			absent := func() {
				t.Helper()
				for _, item := range batch {
					var count int64
					if err := db.Raw("SELECT COUNT(*) FROM "+item.EntityType+" WHERE gym_id=? AND id=?", gym, item.EntityID).Scan(&count).Error; err != nil || count != 0 {
						t.Fatalf("partial %s count=%d err=%v", item.EntityType, count, err)
					}
				}
				var count int64
				db.Raw(`SELECT COUNT(*) FROM sync_entities WHERE gym_id=?`, gym).Scan(&count)
				if count != 0 {
					t.Fatal("partial journal")
				}
			}
			for missing := range batch {
				split := append([]PushItem{}, batch[:missing]...)
				split = append(split, batch[missing+1:]...)
				resp := send(split)
				for _, r := range resp.Results {
					if r.Status == StatusAccepted || r.Status == StatusConflictClientWins || r.Status == StatusConflictServerWins {
						t.Fatalf("split %d accepted %s", missing, r.EntityID)
					}
				}
				absent()
			}
			// A permanently invalid dependency also rolls back the otherwise valid money.
			bad := append([]PushItem{}, batch...)
			raw := decodePushPayload(bad[3].Payload)
			raw["product_id"] = uuid.NewString()
			bad[3].Payload, _ = json.Marshal(raw)
			for _, r := range send(bad).Results {
				if r.Status == StatusAccepted {
					t.Fatal("partial acceptance on permanent rejection")
				}
			}
			absent()
			// Transport metadata cannot smuggle a catalog write into a financial
			// group or leave an unprocessed item acknowledged with an empty status.
			for _, first := range []bool{false, true} {
				id := uuid.NewString()
				payload, _ := json.Marshal(map[string]any{"id": id, "gym_id": gym.String(), financialMembersKey: keys})
				extra := PushItem{QueueID: uuid.NewString(), EntityType: "products", EntityID: id, Operation: OpUpsertStr, ClientVersion: 1, Payload: payload}
				tampered := append(append([]PushItem{}, batch...), extra)
				if first {
					tampered = append([]PushItem{extra}, batch...)
				}
				for _, r := range send(tampered).Results {
					if r.Status != StatusRejectedFinancialConflict {
						t.Fatalf("tampered graph status: %+v", r)
					}
				}
				absent()
			}
			for replay := 0; replay < 2; replay++ {
				for _, r := range send(batch).Results {
					if r.Error != "" {
						t.Fatalf("repair/replay rejected: %+v", r)
					}
				}
			}
			var cents, stock, n int64
			db.Raw(`SELECT ROUND(SUM(amount)*100)::bigint FROM payments WHERE gym_id=?`, gym).Scan(&cents)
			db.Raw(`SELECT stock FROM products WHERE gym_id=?`, gym).Scan(&stock)
			db.Raw(`SELECT COUNT(*) FROM stock_movements WHERE gym_id=?`, gym).Scan(&n)
			if cents != 3950 || stock != 8 || n != 1 {
				t.Fatalf("replay lost or duplicated money/stock: %d %d %d", cents, stock, n)
			}
		})
	}
}
