//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	prodRepoLite "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

type productsFixture struct {
	t          *testing.T
	db         *sqlx.DB
	uow        sharedDomain.UnitOfWork
	recorder   audit.Recorder
	gymID      uuid.UUID
	ownerID    uuid.UUID
	productSvc *prodApp.ProductService

	productRepo       *prodRepoLite.ProductSQLiteRepository
	stockMovementRepo *prodRepoLite.StockMovementSQLiteRepository
}

func setupProducts(t *testing.T) *productsFixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	dsn := dbPath + "?_foreign_keys=on"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)

	for _, m := range []string{
		"../../../../db_migrations/sqlite/001_init_schema.sql",
		"../../../../db_migrations/sqlite/005_users_pin.sql",
		"../../../../db_migrations/sqlite/008_gym_charge_settings.sql",
		"../../../../db_migrations/sqlite/018_gyms_stripe_customer.sql",
	} {
		schema, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}
	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())
	recorder := audit.NewSQLiteRecorder()

	signup := usersApp.NewSignupOwner(
		usersRepoLite.NewUserSQLiteRepository(),
		gymRepoLite.NewGymSQLiteRepository(),
		uow,
		auth.NewJWTService("test-secret"),
		recorder,
		30,
	)
	owner, err := signup.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Owner",
		Email:           "owner@gym.com",
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	productRepo := prodRepoLite.NewProductSQLiteRepository()
	stockMovementRepo := prodRepoLite.NewStockMovementSQLiteRepository()

	return &productsFixture{
		t:                 t,
		db:                db,
		uow:               uow,
		recorder:          recorder,
		gymID:             owner.GymID,
		ownerID:           owner.UserID,
		productRepo:       productRepo,
		stockMovementRepo: stockMovementRepo,
		productSvc:        prodApp.NewProductService(productRepo, stockMovementRepo),
	}
}

func (f *productsFixture) createProductUC() *prodApp.CreateProduct {
	return prodApp.NewCreateProduct(f.productRepo, f.stockMovementRepo, f.uow, f.recorder)
}

func (f *productsFixture) adjustStockUC() *prodApp.AdjustStock {
	return prodApp.NewAdjustStock(f.productRepo, f.stockMovementRepo, f.uow, f.recorder)
}

func (f *productsFixture) listUC() *prodApp.ListProducts {
	return prodApp.NewListProducts(f.productRepo, f.uow)
}

// ---------------------------------------------------------------------------
// UC-023 — CreateProduct
// ---------------------------------------------------------------------------

func TestUC023_Create_PersistsAndSeedsStockMovement(t *testing.T) {
	f := setupProducts(t)
	uc := f.createProductUC()
	out, err := uc.Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Name: "Agua Ciel 600ml", Price: 20, InitialStock: 24, StockMinimum: 5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var n int
	if err := f.db.Get(&n, "SELECT COUNT(*) FROM products WHERE id=?", out.ProductID.String()); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("products = %d, want 1", n)
	}
	var stock int
	if err := f.db.Get(&stock, "SELECT stock FROM products WHERE id=?", out.ProductID.String()); err != nil {
		t.Fatalf("stock: %v", err)
	}
	if stock != 24 {
		t.Errorf("stock = %d, want 24", stock)
	}
	// Initial stock_movement (type=restock) should exist.
	var nMv int
	if err := f.db.Get(&nMv, "SELECT COUNT(*) FROM stock_movements WHERE product_id=? AND movement_type='restock'", out.ProductID.String()); err != nil {
		t.Fatalf("movements: %v", err)
	}
	if nMv != 1 {
		t.Errorf("initial restock movement count = %d, want 1", nMv)
	}
}

func TestUC023_Create_DuplicateNameRejected(t *testing.T) {
	f := setupProducts(t)
	uc := f.createProductUC()
	in := prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Name: "Gatorade", Price: 25,
	}
	if _, err := uc.Execute(context.Background(), in); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same name with different case — also rejected (uq is case-insensitive).
	in.Name = "gatorade"
	if _, err := uc.Execute(context.Background(), in); err == nil {
		t.Errorf("duplicate name should fail")
	}
}

func TestUC023_Create_ZeroStockSkipsMovement(t *testing.T) {
	f := setupProducts(t)
	uc := f.createProductUC()
	out, err := uc.Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Name: "Whey", Price: 40, InitialStock: 0,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var nMv int
	_ = f.db.Get(&nMv, "SELECT COUNT(*) FROM stock_movements WHERE product_id=?", out.ProductID.String())
	if nMv != 0 {
		t.Errorf("zero-initial-stock should not create movement, got %d", nMv)
	}
}

// ---------------------------------------------------------------------------
// UC-024 — AdjustStock
// ---------------------------------------------------------------------------

func TestUC024_Restock_IncreasesStockAndLogsMovement(t *testing.T) {
	f := setupProducts(t)
	createdOut, _ := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Agua", Price: 20, InitialStock: 10,
	})
	cost := 1500.0
	reason := "Proveedor Coca"
	out, err := f.adjustStockUC().Execute(context.Background(), prodApp.AdjustStockInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ProductID: createdOut.ProductID,
		MovementType: "restock", Quantity: 100, Cost: &cost, Reason: &reason,
	})
	if err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if out.NewStock != 110 || out.Delta != 100 {
		t.Errorf("got new=%d delta=%d", out.NewStock, out.Delta)
	}
	var nMv int
	_ = f.db.Get(&nMv, "SELECT COUNT(*) FROM stock_movements WHERE product_id=? AND movement_type='restock'", createdOut.ProductID.String())
	if nMv != 2 {
		t.Errorf("restock movements = %d (initial + 1), want 2", nMv)
	}
}

func TestUC024_Shrinkage_DecreasesAndRespectsFloor(t *testing.T) {
	f := setupProducts(t)
	createdOut, _ := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Botella", Price: 20, InitialStock: 5,
	})
	out, err := f.adjustStockUC().Execute(context.Background(), prodApp.AdjustStockInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ProductID: createdOut.ProductID,
		MovementType: "shrinkage", Quantity: 1,
	})
	if err != nil {
		t.Fatalf("shrinkage: %v", err)
	}
	if out.NewStock != 4 {
		t.Errorf("new stock = %d, want 4", out.NewStock)
	}
	// Cannot go below zero.
	if _, err := f.adjustStockUC().Execute(context.Background(), prodApp.AdjustStockInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ProductID: createdOut.ProductID,
		MovementType: "shrinkage", Quantity: 999,
	}); err == nil {
		t.Errorf("oversize shrinkage should fail")
	}
}

func TestUC024_CountCorrection_ReplacesStock(t *testing.T) {
	f := setupProducts(t)
	createdOut, _ := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Snacks", Price: 30, InitialStock: 18,
	})
	out, err := f.adjustStockUC().Execute(context.Background(), prodApp.AdjustStockInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ProductID: createdOut.ProductID,
		MovementType: "count_correction", Quantity: 15,
	})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if out.NewStock != 15 || out.Delta != -3 {
		t.Errorf("new=%d delta=%d", out.NewStock, out.Delta)
	}
	var movement struct {
		Delta        int    `db:"delta"`
		MovementType string `db:"movement_type"`
	}
	_ = f.db.Get(&movement, "SELECT delta, movement_type FROM stock_movements WHERE product_id=? AND movement_type='count_correction'", createdOut.ProductID.String())
	if movement.Delta != -3 {
		t.Errorf("logged delta = %d, want -3", movement.Delta)
	}
}

func TestUC024_RejectsSaleAndRefundMovementTypes(t *testing.T) {
	f := setupProducts(t)
	createdOut, err := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Xtra", Price: 10, InitialStock: 5,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, mt := range []string{"sale", "refund", "garbage"} {
		if _, err := f.adjustStockUC().Execute(context.Background(), prodApp.AdjustStockInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ProductID: createdOut.ProductID,
			MovementType: mt, Quantity: 1,
		}); err == nil {
			t.Errorf("movement_type=%s should be rejected", mt)
		}
	}
}

// ---------------------------------------------------------------------------
// UC-023 — ListProducts (low_stock filter)
// ---------------------------------------------------------------------------

func TestUC023_List_LowStockFilter(t *testing.T) {
	f := setupProducts(t)
	create := f.createProductUC()
	_, _ = create.Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Bajo", Price: 10, InitialStock: 2, StockMinimum: 5,
	})
	_, _ = create.Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Sano", Price: 10, InitialStock: 50, StockMinimum: 5,
	})
	all, err := f.listUC().Execute(context.Background(), prodApp.ListProductsInput{GymID: f.gymID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if all.Total != 2 {
		t.Errorf("total = %d, want 2", all.Total)
	}
	low, _ := f.listUC().Execute(context.Background(), prodApp.ListProductsInput{GymID: f.gymID, LowStockOnly: true})
	if low.Total != 1 || low.Items[0].Name != "Bajo" {
		t.Errorf("low_stock = %d items, items[0]=%v", low.Total, low.Items)
	}
}

// Round-trip del bug de paginación: la venta rápida / buscador global piden
// "todos los activos" con un page_size grande. Antes el clamp reseteaba
// cualquier page_size > 200 a 50, así que un gym con >50 productos activos
// perdía el resto — invisibles y no-vendibles en el picker. Con el
// clamp-al-cap, pedir de más devuelve HASTA el cap (200) en un request, y
// el frontend pagina para juntar todo. Este test crea 60 activos y prueba
// las dos mitades: (a) un page_size grande ya no cae a 50, y (b) paginar
// junta el catálogo completo.
func TestUC023_List_LargePageSizeReturnsAllActive(t *testing.T) {
	f := setupProducts(t)
	create := f.createProductUC()
	const nProducts = 60
	for i := 0; i < nProducts; i++ {
		if _, err := create.Execute(context.Background(), prodApp.CreateProductInput{
			GymID: f.gymID, ActorUserID: f.ownerID,
			Name: fmt.Sprintf("Producto %03d", i), Price: 10, InitialStock: 5, StockMinimum: 1,
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	// (a) Un page_size por encima del cap ya NO colapsa a 50: devuelve
	// exactamente el cap (200), que en este catálogo alcanza para los 60.
	big, err := f.listUC().Execute(context.Background(), prodApp.ListProductsInput{
		GymID: f.gymID, ActiveFilter: "active", Page: 1, PageSize: 500,
	})
	if err != nil {
		t.Fatalf("list big page: %v", err)
	}
	if big.Total != nProducts {
		t.Errorf("total = %d, want %d", big.Total, nProducts)
	}
	if big.PageSize != prodRepo.MaxPageSize {
		t.Errorf("page_size reportado = %d, want cap %d (no el default 50)", big.PageSize, prodRepo.MaxPageSize)
	}
	if len(big.Items) != nProducts {
		t.Fatalf("items = %d, want %d — el clamp seguía ocultando activos", len(big.Items), nProducts)
	}

	// (b) Con un page_size chico, paginar hasta agotar junta los 60 sin
	// duplicados (espeja fetchAllActiveProducts del desktop).
	seen := map[string]bool{}
	pageSize := 25
	for page := 1; ; page++ {
		res, err := f.listUC().Execute(context.Background(), prodApp.ListProductsInput{
			GymID: f.gymID, ActiveFilter: "active", Page: page, PageSize: pageSize,
		})
		if err != nil {
			t.Fatalf("list page %d: %v", page, err)
		}
		for _, p := range res.Items {
			if seen[p.ID.String()] {
				t.Errorf("producto duplicado entre páginas: %s", p.ID)
			}
			seen[p.ID.String()] = true
		}
		if len(seen) >= res.Total || len(res.Items) < pageSize {
			break
		}
		if page > 10 {
			t.Fatal("paginación no terminó — posible loop")
		}
	}
	if len(seen) != nProducts {
		t.Errorf("paginando junté %d únicos, want %d", len(seen), nProducts)
	}
}

// ---------------------------------------------------------------------------
// ProductService.DecrementForSale — DA-25.2 enforcement at the seam.
// ---------------------------------------------------------------------------

func TestProductService_DecrementForSale_RejectsOversell(t *testing.T) {
	f := setupProducts(t)
	createdOut, _ := f.createProductUC().Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Name: "Limited", Price: 10, InitialStock: 3,
	})
	// Wrap in a Command tx so the service has a real transaction.
	err := f.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := f.productSvc.DecrementForSale(context.Background(), tx, prodApp.DecrementForSaleInput{
			GymID: f.gymID, ProductID: createdOut.ProductID, Quantity: 5,
			OperatorID: f.ownerID, SaleItemID: uuid.New(),
		}, time.Now().UTC())
		return err
	})
	if err == nil || !errors.Is(err, prodErrors.ErrInsufficientStock) {
		// Custom errors wrap; double-check via .Error() substring as fallback.
		if err == nil || (err.Error() == "" || !contains(err.Error(), "stock insuficiente")) {
			t.Errorf("expected insufficient stock, got: %v", err)
		}
	}
	// Stock untouched on rollback.
	var stock int
	_ = f.db.Get(&stock, "SELECT stock FROM products WHERE id=?", createdOut.ProductID.String())
	if stock != 3 {
		t.Errorf("stock mutated despite failure: %d", stock)
	}
	// No stock_movement of type 'sale' exists.
	var nMv int
	_ = f.db.Get(&nMv, "SELECT COUNT(*) FROM stock_movements WHERE product_id=? AND movement_type='sale'", createdOut.ProductID.String())
	if nMv != 0 {
		t.Errorf("expected 0 sale movements after failed tx, got %d", nMv)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
