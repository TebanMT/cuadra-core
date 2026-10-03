//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// El wire de sync transporta dinero en PESOS (para que el Postgres del cloud
// lo reciba sin transformar), pero SQLite guarda CENTAVOS. Antes ApplyPullChange
// coaccionaba float→int sin ×100 → un pull/full-sync escribía pesos en una
// columna de centavos y dividía cada monto entre 100 (catálogo a $0.50). Este
// test fija la conversión, incluyendo el caso traicionero de pesos enteros
// (50.00 se veía como int y quedaba en 50 centavos).
func TestApplyPullChange_Product_PesosConvertidosACentavos(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)

	applyProduct := func(t *testing.T, name string, pricePesos float64) string {
		t.Helper()
		id := uuid.New().String()
		now := time.Now().UTC().UnixMilli()
		payload, err := json.Marshal(map[string]any{
			"id":            id,
			"gym_id":        gymID.String(),
			"version":       1,
			"created_at":    now,
			"updated_at":    now,
			"name":          name,
			"price":         pricePesos, // PESOS en el wire
			"stock":         10,
			"stock_minimum": 2,
			"active":        true,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		change := syncpkg.PullChange{
			EntityType: "products", EntityID: id, Version: 1,
			Payload: payload, ServerUpdatedAt: time.Now().UTC(),
		}
		if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
			return syncpkg.ApplyPullChange(context.Background(), tx, change)
		}); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		return id
	}

	cases := []struct {
		name      string
		pesos     float64
		wantCents int64
	}{
		{"con-decimales", 50.50, 5050},
		{"pesos-enteros", 50.00, 5000}, // el caso que el código viejo rompía → 50
		{"medio-centavo", 0.05, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := applyProduct(t, tc.name, tc.pesos)
			var cents int64
			if err := db.Get(&cents, `SELECT price FROM products WHERE id = ?`, id); err != nil {
				t.Fatalf("read: %v", err)
			}
			if cents != tc.wantCents {
				t.Errorf("price = %d centavos, want %d (pesos %.2f × 100)", cents, tc.wantCents, tc.pesos)
			}
		})
	}
}

// Espejo de membership_types: mismo contrato pesos-en-wire → centavos-en-SQLite.
// Regresión del bug donde el cloud serializaba membership_types.price con
// toCents() (centavos) mientras el apply espera PESOS → el desktop mostraba los
// precios ×100 tras el primer pull-back (Trimestral $1,300 se veía $130,000).
// Con $500 en el wire, la columna price de SQLite (centavos) debe quedar 50000.
func TestApplyPullChange_MembershipType_PesosConvertidosACentavos(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)

	id := uuid.New().String()
	now := time.Now().UTC().UnixMilli()
	payload, err := json.Marshal(map[string]any{
		"id":              id,
		"gym_id":          gymID.String(),
		"version":         1,
		"created_at":      now,
		"updated_at":      now,
		"name":            "Mensual",
		"price":           500.00, // PESOS en el wire (igual que el cloud ya corregido)
		"duration_days":   30,
		"enrollment_fee":  0.00,
		"maintenance_fee": 0.00,
		"active":          true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	change := syncpkg.PullChange{
		EntityType: "membership_types", EntityID: id, Version: 1,
		Payload: payload, ServerUpdatedAt: time.Now().UTC(),
	}
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, change)
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var cents int64
	if err := db.Get(&cents, `SELECT price FROM membership_types WHERE id = ?`, id); err != nil {
		t.Fatalf("read: %v", err)
	}
	if cents != 50000 {
		t.Errorf("price = %d centavos, want 50000 ($500 × 100). Si da 5000000, el cloud está mandando centavos.", cents)
	}
}

// El costo de stock_movements sigue el mismo contrato: $5 en el wire debe
// aterrizar como 500 centavos. Este es el componente COGS que reconcilia el
// resultado operativo entre cloud y desktop.
func TestApplyPullChange_StockMovement_CostoPesosConvertidoACentavos(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	userID := uuid.New()
	productID := uuid.New()
	movementID := uuid.New()
	now := time.Now().UTC().UnixMilli()

	if _, err := db.Exec(`
		INSERT INTO users
		    (id, gym_id, version, created_at, updated_at, email,
		     password_hash, full_name, role, active)
		VALUES (?, ?, 1, ?, ?, 'owner@test.local', 'x', 'Owner', 'owner', 1)`,
		userID, gymID, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO products
		    (id, gym_id, version, created_at, updated_at, name,
		     price, stock, stock_minimum, active)
		VALUES (?, ?, 1, ?, ?, 'Agua de prueba', 1500, 24, 2, 1)`,
		productID, gymID, now, now); err != nil {
		t.Fatalf("seed product: %v", err)
	}

	payload, err := json.Marshal(map[string]any{
		"id":            movementID.String(),
		"gym_id":        gymID.String(),
		"version":       1,
		"created_at":    now,
		"updated_at":    now,
		"product_id":    productID.String(),
		"movement_type": "restock",
		"delta":         24,
		"reason":        "Inventario inicial",
		"cost":          5.0,
		"is_purchase":   false,
		"sale_item_id":  nil,
		"operator_id":   userID.String(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	change := syncpkg.PullChange{
		EntityType: "stock_movements", EntityID: movementID.String(), Version: 1,
		Payload: payload, ServerUpdatedAt: time.Now().UTC(),
	}
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, change)
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var cents int64
	if err := db.Get(&cents, `SELECT cost FROM stock_movements WHERE id = ?`, movementID); err != nil {
		t.Fatalf("read cost: %v", err)
	}
	if cents != 500 {
		t.Errorf("cost = %d centavos, want 500 ($5 × 100)", cents)
	}
}
