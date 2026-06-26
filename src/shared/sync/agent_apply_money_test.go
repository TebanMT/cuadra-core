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
