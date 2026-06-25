//go:build sidecar

package app_test

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"

	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
)

// Ganancia potencial sobre el stock + costo unitario promedio ponderado.
// Semántica de `cost`: UNITARIO (coincide con el KPI de egresos
// SUM(cost*delta) y la etiqueta "Costo unitario" del alta). El promedio es
// SUM(cost*delta)/SUM(delta), redondeado a centavos.

func floatEq(a, b float64) bool { return math.Abs(a-b) < 0.005 }

// restockWithCost agrega una entrada de inventario con costo UNITARIO.
func (f *productsFixture) restockWithCost(t *testing.T, productID uuid.UUID, qty int, unitCost float64) {
	t.Helper()
	c := unitCost
	_, err := f.adjustStockUC().Execute(context.Background(), prodApp.AdjustStockInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ProductID: productID,
		MovementType: "restock", Quantity: qty, Cost: &c,
	})
	if err != nil {
		t.Fatalf("restock: %v", err)
	}
}

func (f *productsFixture) aggregates(t *testing.T) prodRepo.ProductAggregates {
	t.Helper()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	aggs, err := f.productRepo.ListAggregates(tx, prodRepo.ListQuery{GymID: f.gymID})
	if err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	return aggs
}

func (f *productsFixture) unitCosts(t *testing.T) map[string]float64 {
	t.Helper()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	raw, err := f.productRepo.ListUnitCosts(tx, prodRepo.ListQuery{GymID: f.gymID})
	if err != nil {
		t.Fatalf("unit costs: %v", err)
	}
	out := make(map[string]float64, len(raw))
	for id, c := range raw {
		out[id.String()] = c
	}
	return out
}

// TestMargin_WeightedAvgCost_PotentialAndCoverage cubre el caso central:
// dos productos con costo (uno con dos entradas a distinto costo → promedio
// ponderado) y uno SIN costo (excluido del agregado pero contado en la
// cobertura).
func TestMargin_WeightedAvgCost_PotentialAndCoverage(t *testing.T) {
	f := setupProducts(t)

	// Agua: sin stock inicial; dos resurtidos a costo unitario distinto.
	// avg = (5·10 + 7·30) / (10+30) = 260/40 = 6.50. stock=40, precio=10.
	aguaOut, _ := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Agua", Price: 10, InitialStock: 0,
	})
	f.restockWithCost(t, aguaOut.ProductID, 10, 5.00)
	f.restockWithCost(t, aguaOut.ProductID, 30, 7.00)

	// Barra: stock inicial con costo unitario 8.00. stock=5, precio=20.
	barraCost := 8.00
	barraOut, _ := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Barra", Price: 20,
		InitialStock: 5, InitialCost: &barraCost,
	})

	// SinCosto: stock inicial sin costo. stock=12, precio=15. Excluido del
	// agregado de costo/ganancia pero cuenta en TotalValue y ProductsTotal.
	_, _ = f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "SinCosto", Price: 15, InitialStock: 12,
	})

	aggs := f.aggregates(t)

	// TotalValue = 40·10 + 5·20 + 12·15 = 400 + 100 + 180 = 680 (todos los activos).
	if !floatEq(aggs.TotalValue, 680) {
		t.Errorf("TotalValue = %v, want 680", aggs.TotalValue)
	}
	// Cobertura: 3 activos, 2 con costo (Agua, Barra).
	if aggs.ProductsTotal != 3 || aggs.ProductsWithCost != 2 {
		t.Errorf("cobertura = %d/%d, want 2/3", aggs.ProductsWithCost, aggs.ProductsTotal)
	}
	// Solo los que tienen costo:
	//   potential = 40·(10−6.5) + 5·(20−8) = 140 + 60 = 200
	//   costValue = 40·6.5 + 5·8 = 260 + 40 = 300
	//   saleValueWithCost = 40·10 + 5·20 = 400 + 100 = 500
	if !floatEq(aggs.PotentialProfit, 200) {
		t.Errorf("PotentialProfit = %v, want 200", aggs.PotentialProfit)
	}
	if !floatEq(aggs.CostValue, 300) {
		t.Errorf("CostValue = %v, want 300", aggs.CostValue)
	}
	if !floatEq(aggs.SaleValueWithCost, 500) {
		t.Errorf("SaleValueWithCost = %v, want 500", aggs.SaleValueWithCost)
	}
	// margen % = 200/500 = 40%.
	if pct := aggs.PotentialProfit / aggs.SaleValueWithCost * 100; !floatEq(pct, 40) {
		t.Errorf("marginPct = %v, want 40", pct)
	}

	// ListUnitCosts: Agua=6.50, Barra=8.00, SinCosto ausente.
	costs := f.unitCosts(t)
	if !floatEq(costs[aguaOut.ProductID.String()], 6.50) {
		t.Errorf("Agua avg cost = %v, want 6.50", costs[aguaOut.ProductID.String()])
	}
	if !floatEq(costs[barraOut.ProductID.String()], 8.00) {
		t.Errorf("Barra avg cost = %v, want 8.00", costs[barraOut.ProductID.String()])
	}
	if len(costs) != 2 {
		t.Errorf("unit cost map size = %d, want 2 (SinCosto excluido)", len(costs))
	}
}

// TestMargin_NoCostProducts_ZeroAggregate — sin ningún costo capturado el
// agregado de ganancia es 0 y la cobertura es 0/N (no rompe; el FE oculta
// el chip de margen).
func TestMargin_NoCostProducts_ZeroAggregate(t *testing.T) {
	f := setupProducts(t)
	if _, err := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Mani", Price: 10, InitialStock: 5,
	}); err != nil {
		t.Fatalf("create Mani: %v", err)
	}
	if _, err := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Toalla", Price: 20, InitialStock: 3,
	}); err != nil {
		t.Fatalf("create Toalla: %v", err)
	}
	aggs := f.aggregates(t)
	if aggs.ProductsTotal != 2 || aggs.ProductsWithCost != 0 {
		t.Errorf("cobertura = %d/%d, want 0/2", aggs.ProductsWithCost, aggs.ProductsTotal)
	}
	if !floatEq(aggs.PotentialProfit, 0) || !floatEq(aggs.CostValue, 0) || !floatEq(aggs.SaleValueWithCost, 0) {
		t.Errorf("montos de costo deben ser 0: potential=%v cost=%v saleWithCost=%v",
			aggs.PotentialProfit, aggs.CostValue, aggs.SaleValueWithCost)
	}
	// TotalValue sigue contando todos los activos.
	if !floatEq(aggs.TotalValue, 5*10+3*20) {
		t.Errorf("TotalValue = %v, want 110", aggs.TotalValue)
	}
	if len(f.unitCosts(t)) != 0 {
		t.Errorf("unit cost map debe estar vacío")
	}
}

// TestMargin_RoundsHalfUpToCentavos — promedio que cae en medio centavo
// redondea hacia arriba, idéntico al binario cloud (paridad PG/SQLite).
func TestMargin_RoundsHalfUpToCentavos(t *testing.T) {
	f := setupProducts(t)
	// dos unidades: una a 0.01 y otra a 0.02 → avg = 0.015 → 0.02.
	out, _ := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Centavo", Price: 1, InitialStock: 0,
	})
	f.restockWithCost(t, out.ProductID, 1, 0.01)
	f.restockWithCost(t, out.ProductID, 1, 0.02)
	costs := f.unitCosts(t)
	if !floatEq(costs[out.ProductID.String()], 0.02) {
		t.Errorf("avg cost = %v, want 0.02 (half-up)", costs[out.ProductID.String()])
	}
}
