//go:build sidecar

package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func TestLegacyStockCostDoesNotBlockCompletedPurchase(t *testing.T) {
	for _, mode := range []string{"incremental_insert", "incremental_update", "full"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			gym, user, product, movement, purchase := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			db, uow := compositeKeyTestDB(t, gym)
			now := time.Now().UTC().Truncate(time.Millisecond)
			for _, statement := range []struct {
				query string
				args  []any
			}{
				{`INSERT INTO users(id,gym_id,created_at,updated_at,email,password_hash,full_name,role) VALUES(?,?,?,?,?,'x','Owner','owner')`, []any{user, gym, now.UnixMilli(), now.UnixMilli(), user.String() + "@test.local"}},
				{`INSERT INTO products(id,gym_id,created_at,updated_at,name,price,stock,stock_base) VALUES(?,?,?,?,'Agua',2000,16,16)`, []any{product, gym, now.UnixMilli(), now.UnixMilli()}},
			} {
				if _, err := db.Exec(statement.query, statement.args...); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "incremental_update" {
				if _, err := db.Exec(`INSERT INTO stock_movements(id,gym_id,version,created_at,updated_at,product_id,movement_type,delta,cost,operator_id) VALUES(?,?,18,?,?,?,'restock',16,NULL,?)`, movement, gym, now.UnixMilli(), now.UnixMilli(), product, user); err != nil {
					t.Fatal(err)
				}
			}
			change := func(kind string, id uuid.UUID, version int, fields map[string]any) PullChange {
				fields["id"], fields["gym_id"], fields["version"] = id, gym, version
				fields["created_at"], fields["updated_at"] = now.UnixMilli(), now.UnixMilli()
				payload, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				return PullChange{EntityType: kind, EntityID: id.String(), Version: version, Payload: payload, ServerUpdatedAt: now}
			}
			page := []PullChange{
				change("stock_movements", movement, 19, map[string]any{"product_id": product, "operator_id": user, "movement_type": "restock", "delta": 16, "cost": 0, "is_purchase": true}),
				change("inventory_purchases", purchase, 2, map[string]any{"stock_movement_id": movement, "product_id": product, "quantity": 16, "unit_cost": 12.35, "total_amount": 197.60, "status": "legacy_incomplete", "created_by": user, "origin": "desktop", "idempotency_key": "legacy:" + movement.String()}),
			}
			for range 2 {
				var err error
				if mode == "full" {
					err = applyFullSyncPage(ctx, uow, page, nil)
				} else {
					err = ApplyPullPage(ctx, uow, page, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			var cost sql.NullInt64
			if err := db.Get(&cost, `SELECT cost FROM stock_movements WHERE id=?`, movement); err != nil || cost.Valid {
				t.Fatalf("unknown cost=%v err=%v", cost, err)
			}
			for query, want := range map[string]int{
				`SELECT version FROM stock_movements`:          19,
				`SELECT stock FROM products`:                   16,
				`SELECT unit_cost FROM inventory_purchases`:    1235,
				`SELECT total_amount FROM inventory_purchases`: 19760,
				`SELECT COUNT(*) FROM stock_movements`:         1,
				`SELECT COUNT(*) FROM inventory_purchases`:     1,
				`SELECT COUNT(*) FROM cash_movements`:          0,
				`SELECT COUNT(*) FROM sync_queue`:              0,
			} {
				var got int
				if err := db.Get(&got, query); err != nil || got != want {
					t.Fatalf("%s: got %d want %d err %v", query, got, want, err)
				}
			}
			// Real corrections must still replace the unknown cost in cents.
			valid := change("stock_movements", movement, 20, map[string]any{"product_id": product, "operator_id": user, "movement_type": "restock", "delta": 16, "cost": 12.35, "is_purchase": true})
			if err := ApplyPullPage(ctx, uow, []PullChange{valid}, nil); err != nil {
				t.Fatal(err)
			}
			if err := db.Get(&cost, `SELECT cost FROM stock_movements WHERE id=?`, movement); err != nil || !cost.Valid || cost.Int64 != 1235 {
				t.Fatalf("corrected cost=%v err=%v", cost, err)
			}
			// New negative costs remain invalid; compatibility must not weaken
			// the schema or silently advance past unrelated corrupt records.
			bad := change("stock_movements", movement, 21, map[string]any{"product_id": product, "operator_id": user, "movement_type": "restock", "delta": 16, "cost": -1, "is_purchase": true})
			if err := uow.Command(ctx, func(tx shared.Transaction) error { return ApplyPullChange(ctx, tx, bad) }); err == nil {
				t.Fatal("negative cost accepted")
			}
		})
	}
}
