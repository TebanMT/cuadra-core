// Package repository declares the persistence contracts for the products BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"github.com/google/uuid"

	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ProductRepository — UC-023, UC-024, and the products half of UC-025.
type ProductRepository interface {
	Create(tx sharedDomain.Transaction, p *productDomain.Product) (*productDomain.Product, error)
	Update(tx sharedDomain.Transaction, p *productDomain.Product) (*productDomain.Product, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*productDomain.Product, error)
	List(tx sharedDomain.Transaction, q ListQuery) ([]*productDomain.Product, int, error)
	// ListAggregates devuelve totales sobre el filtro completo (no
	// solo la página visible). Se llama en paralelo con List para
	// alimentar las StatCards "Stock bajo" / "Agotados" / "Valor de
	// stock" — sin esto los contadores reflejaban únicamente lo que el
	// operador veía en pantalla, mintiendo cuando había paginación.
	ListAggregates(tx sharedDomain.Transaction, q ListQuery) (ProductAggregates, error)
	// ListUnitCosts devuelve, por producto del filtro, el costo unitario
	// promedio ponderado por cantidad all-time (en pesos) —
	// SUM(cost*delta)/SUM(delta) sobre las entradas `restock` con costo
	// unitario capturado. Solo incluye productos
	// con al menos una entrada con costo; los demás se omiten del mapa
	// ("sin costo capturado"). Alimenta la línea "Costo prom · Precio ·
	// Margen" de la ficha del producto. El redondeo a centavos es idéntico
	// en SQLite y Postgres (ver impls) para que ambos binarios coincidan.
	ListUnitCosts(tx sharedDomain.Transaction, q ListQuery) (map[uuid.UUID]float64, error)
	ExistsByGymAndName(tx sharedDomain.Transaction, gymID uuid.UUID, name string, excludeID *uuid.UUID) (bool, error)
}

// ProductAggregates — counters y totales del filtro completo.
// TotalValue siempre considera solo productos activos (precio venta ×
// stock); inactivos no cuentan porque no están a la venta.
type ProductAggregates struct {
	TotalValue float64 // precio venta × existencias, solo activos
	LowCount   int     // activos con stock <= stock_minimum y > 0
	OutCount   int     // activos con stock = 0

	// Ganancia potencial sobre el stock (Standard, sin gate Plus). El
	// costo unitario es el promedio ponderado all-time de las ENTRADAS
	// con costo capturado (stock_movements `restock` con `cost`). Los
	// productos SIN costo capturado se EXCLUYEN de los montos de costo y
	// ganancia, pero SÍ cuentan en ProductsTotal — la cobertura honesta
	// vive en ProductsWithCost/ProductsTotal ("14 de 18 con costo"). Todos
	// los montos en pesos, solo productos activos del filtro.
	CostValue         float64 // SUM(stock × costoProm) de los activos con costo
	PotentialProfit   float64 // SUM(stock × (precio − costoProm)) de los activos con costo
	SaleValueWithCost float64 // SUM(stock × precio) de los MISMOS activos con costo — denominador del margen %
	ProductsTotal     int     // # de productos activos en el filtro
	ProductsWithCost  int     // # de esos activos con costo promedio capturado
}

// ActiveFilter restringe la consulta por el flag `active` del producto.
// Tres estados expuestos por la UI: solo activos (default), solo
// inactivos (desactivados), o todos. La opción "inactivos" existe para
// que el operador pueda reactivar productos sin tener que mostrar todo
// el catálogo.
const (
	ActiveFilterActive   = "active"
	ActiveFilterInactive = "inactive"
	ActiveFilterAll      = "all"
)

// Sort columns expuestas por el header de la tabla del FE. Cualquier
// otro valor cae al default (SortName) en los repos — defensa en
// profundidad por si llega basura del cliente.
const (
	SortName     = "name"
	SortPrice    = "price"
	SortStock    = "stock"
	SortCategory = "category"
)

const (
	SortDirAsc  = "asc"
	SortDirDesc = "desc"
)

// Paging bounds for ListQuery.PageSize. Shared by the list use case and both
// repo implementations (via normalizePage) so the clamp semantics stay
// identical across every layer.
//
//   - PageSize below 1 falls back to DefaultPageSize.
//   - PageSize above MaxPageSize is CAPPED at MaxPageSize — never reset to the
//     default. Asking for "as many as possible" (e.g. the venta grid / global
//     search pulling the whole active catalogue) must never yield FEWER than
//     the cap. Antes un page_size=500 caía a 50 y ocultaba productos vendibles
//     en gyms con >50 activos. Callers que necesitan TODO paginan hasta agotar.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ListQuery is the input for UC-023 (list products). Filters are intentionally
// minimal — the recepcionist UI is the main caller, the dashboard is secondary.
type ListQuery struct {
	GymID        uuid.UUID
	Search       string // case-insensitive substring on name (LIKE %s%)
	Category     string // exact match; empty = all
	ActiveFilter string // "active" | "inactive" | "all"; empty defaults to "active".
	LowStockOnly bool
	Sort         string // SortName (default) | SortPrice | SortStock | SortCategory
	Direction    string // SortDirAsc (default) | SortDirDesc
	Page         int
	PageSize     int
}

// StockMovementRepository — append-only history. Used by UC-024 and UC-025.
type StockMovementRepository interface {
	Create(tx sharedDomain.Transaction, m *stockMovementDomain.StockMovement) (*stockMovementDomain.StockMovement, error)
	ListByProduct(tx sharedDomain.Transaction, productID uuid.UUID, limit int) ([]*stockMovementDomain.StockMovement, error)
}
