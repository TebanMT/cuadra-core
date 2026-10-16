//go:build server && integration

package main

import (
	"github.com/cuadra/cuadra-core/src/shared/testutil"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"testing"
)

func TestImportedProductsActivateStockLedger(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gym, owner := uuid.New(), uuid.New()
	exec := func(query string, args ...any) {
		t.Helper()
		if err := db.Exec(query, args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO gyms(id,gym_id,name,country,timezone) VALUES(?,?,'Import stock','MX','America/Mexico_City')`, gym, gym)
	exec(`INSERT INTO users(id,gym_id,full_name,role,email) VALUES(?,?,'Owner','owner',?)`, owner, gym, uuid.NewString()+"@test.invalid")
	t.Cleanup(func() {
		for _, table := range []string{"sync_entities", "stock_movements", "products", "users", "gyms"} {
			_ = db.Exec("DELETE FROM "+table+" WHERE gym_id=?", gym).Error
		}
	})
	source := sourceData{products: []srcProducto{{ID: 195704, Nombre: "Importado", Precio: 15, Estado: 1}}}
	if err := db.Transaction(func(tx *gorm.DB) error { return importAll(tx, source, gym, owner, 1) }); err != nil {
		t.Fatal(err)
	}
	product := legacyID("producto", 195704)
	var active bool
	if err := db.Raw(`SELECT stock_origin=0 FROM products WHERE gym_id=? AND id=?`, gym, product).Scan(&active).Error; err != nil || !active {
		t.Fatalf("import ledger active=%v err=%v", active, err)
	}
	exec(`INSERT INTO stock_movements(id,gym_id,product_id,movement_type,delta,operator_id) VALUES(?,?,?,'restock',3,?)`, uuid.New(), gym, product, owner)
	exec(`UPDATE products SET stock=999 WHERE gym_id=? AND id=?`, gym, product)
	var stock int
	if err := db.Raw(`SELECT stock FROM products WHERE gym_id=? AND id=?`, gym, product).Scan(&stock).Error; err != nil || stock != 3 {
		t.Fatalf("stock=%d err=%v", stock, err)
	}
}
