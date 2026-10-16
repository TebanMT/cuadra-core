//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	app "github.com/cuadra/cuadra-core/src/modules/products/app"
	"github.com/google/uuid"
	"sync"
	"testing"
	"time"
)

func TestStockCountSQLiteAtomicReplayAndConflict(t *testing.T) {
	f := setupProducts(t)
	ctx := context.Background()
	uc := app.NewStockCount(f.productRepo, f.productRepo, f.stockMovementRepo, f.uow, f.recorder)
	makeLine := func(name string, stock, qty int) app.CountLine {
		out, err := f.createProductUC().Execute(ctx, app.CreateProductInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: name, Price: 10, InitialStock: stock})
		if err != nil {
			t.Fatal(err)
		}
		tx, _ := f.uow.Query(ctx)
		p, err := f.productRepo.GetByID(tx, out.ProductID)
		if err != nil {
			t.Fatal(err)
		}
		return app.CountLine{ProductID: p.ID, Revision: app.CountSnapshot(p).Revision, Quantity: &qty}
	}
	in := app.CountInput{ID: uuid.New(), GymID: f.gymID, ActorID: f.ownerID, ActorRole: "operator", Reason: "Conteo de prueba", PurchasesChecked: true, Lines: []app.CountLine{makeLine("Cero", 10, 0), makeLine("Positivo", 2, 7), makeLine("Sin diferencia", 3, 3)}}
	empty := in
	empty.ID = uuid.New()
	empty.Lines = []app.CountLine{{ProductID: in.Lines[0].ProductID, Revision: in.Lines[0].Revision}}
	if _, err := uc.Execute(ctx, empty); err == nil {
		t.Fatal("empty quantity accepted")
	}
	for _, qty := range []int{-1, 1000001} {
		invalid := in
		invalid.ID = uuid.New()
		invalid.Lines = []app.CountLine{{ProductID: in.Lines[0].ProductID, Revision: in.Lines[0].Revision, Quantity: &qty}}
		if _, err := uc.Execute(ctx, invalid); err == nil {
			t.Fatalf("invalid quantity accepted: %d", qty)
		}
	}
	out, err := uc.Execute(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			again, e := uc.Execute(ctx, in)
			if e != nil {
				t.Error(e)
				return
			}
			a, _ := json.Marshal(again)
			b, _ := json.Marshal(out)
			if string(a) != string(b) {
				t.Error("replay changed result")
			}
		}()
	}
	wg.Wait()
	var n int
	f.db.Get(&n, `SELECT COUNT(*) FROM stock_movements WHERE movement_type='count_correction'`)
	if n != 3 {
		t.Fatalf("movements=%d", n)
	}
	f.db.Get(&n, `SELECT COUNT(*) FROM inventory_purchases`)
	if n != 0 {
		t.Fatal("count created purchase")
	}
	f.db.Get(&n, `SELECT COUNT(*) FROM stock_movements WHERE movement_type='count_correction' AND (cost IS NOT NULL OR is_purchase<>0)`)
	if n != 0 {
		t.Fatal("count created costs")
	}
	f.db.Get(&n, `SELECT COUNT(*) FROM audit_log WHERE action='stock_count_confirmed'`)
	if n != 1 {
		t.Fatalf("audit=%d", n)
	}
	// A new movement invalidates the snapshot, and a second valid line must
	// roll back with it. The immutable sale/delta remains untouched.
	changed := makeLine("Cambio durante conteo", 10, 8)
	other := makeLine("Debe quedar igual", 10, 7)
	_, err = f.adjustStockUC().Execute(ctx, app.AdjustStockInput{GymID: f.gymID, ActorUserID: f.ownerID, ProductID: changed.ProductID, MovementType: "shrinkage", Quantity: 1, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	conflict := in
	conflict.ID = uuid.New()
	conflict.Lines = []app.CountLine{changed, other}
	_, err = uc.Execute(ctx, conflict)
	var ce *app.CountConflict
	if !errors.As(err, &ce) || len(ce.Items) != 1 {
		t.Fatalf("conflict=%v", err)
	}
	f.db.Get(&n, `SELECT stock FROM products WHERE id=?`, other.ProductID.String())
	if n != 10 {
		t.Fatal("partial count committed")
	}
	conflict.Lines[0].Revision = ce.Items[0].Revision
	if _, err = uc.Execute(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	// Replays still work after deactivation; new counts do not.
	f.db.Exec(`UPDATE products SET active=0 WHERE id=?`, other.ProductID.String())
	if _, err = uc.Execute(ctx, conflict); err != nil {
		t.Fatal("lost response replay after deactivate", err)
	}
	conflict.ID = uuid.New()
	if _, err = uc.Execute(ctx, conflict); !errors.As(err, &ce) {
		t.Fatal("inactive accepted", err)
	}
	deleted := makeLine("Eliminado durante conteo", 5, 2)
	if _, err = f.db.Exec(`UPDATE products SET deleted_at=? WHERE id=?`, time.Now().UTC().UnixMilli(), deleted.ProductID.String()); err != nil {
		t.Fatal(err)
	}
	conflict.ID = uuid.New()
	conflict.Lines = []app.CountLine{deleted}
	if _, err = uc.Execute(ctx, conflict); !errors.As(err, &ce) || len(ce.Items) != 1 || ce.Items[0].Available {
		t.Fatalf("deleted product accepted: %v", err)
	}
}
