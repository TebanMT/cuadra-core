// Package controllers exposes the products BC over HTTP. Routes are
// registered under /api/v1; auth + audit context come from the shared
// middleware.
package controllers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var (
	errBadID   = errors.New("id inválido")
	errBadAuth = errors.New("autenticación requerida")
)

// ProductController bundles UC-023 + UC-024.
type ProductController struct {
	Create     *prodApp.CreateProduct
	Update     *prodApp.UpdateProduct
	Deactivate *prodApp.DeactivateProduct
	Reactivate *prodApp.ReactivateProduct
	List       *prodApp.ListProducts
	Adjust     *prodApp.AdjustStock
	Tokens     auth.TokenService
}

func NewProductController(
	create *prodApp.CreateProduct,
	update *prodApp.UpdateProduct,
	deactivate *prodApp.DeactivateProduct,
	reactivate *prodApp.ReactivateProduct,
	list *prodApp.ListProducts,
	adjust *prodApp.AdjustStock,
	tokens auth.TokenService,
) *ProductController {
	return &ProductController{
		Create: create, Update: update, Deactivate: deactivate,
		Reactivate: reactivate, List: list, Adjust: adjust, Tokens: tokens,
	}
}

func (ctrl *ProductController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		api.POST("/products", ctrl.handleCreate)
		api.PATCH("/products/:id", ctrl.handleUpdate)
		api.DELETE("/products/:id", ctrl.handleDeactivate)
		api.POST("/products/:id/reactivate", ctrl.handleReactivate)
		api.GET("/products", ctrl.handleList)
		api.POST("/products/:id/adjust-stock", ctrl.handleAdjust)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type createProductReq struct {
	Name          string   `json:"name" validate:"required,min=2,max=100"`
	Price         float64  `json:"price" validate:"required,gt=0"`
	InitialStock  int      `json:"initial_stock"`
	StockMinimum  int      `json:"stock_minimum"`
	Category      *string  `json:"category,omitempty"`
	ImageURL      *string  `json:"image_url,omitempty"`
	InitialCost   *float64 `json:"initial_cost,omitempty"`
	InitialReason *string  `json:"initial_reason,omitempty"`
	// initial_is_purchase=false → captura de inventario preexistente (no es
	// egreso). Ausente/true = compra (compat con FEs viejos).
	InitialIsPurchase *bool `json:"initial_is_purchase,omitempty"`
}

type updateProductReq struct {
	Name         string  `json:"name" validate:"required,min=2,max=100"`
	Price        float64 `json:"price" validate:"required,gt=0"`
	StockMinimum int     `json:"stock_minimum"`
	Category     *string `json:"category,omitempty"`
	ImageURL     *string `json:"image_url,omitempty"`
}

type adjustStockReq struct {
	MovementType string   `json:"movement_type" validate:"required,oneof=restock shrinkage count_correction"`
	Quantity     int      `json:"quantity" validate:"required,min=0"`
	Cost         *float64 `json:"cost,omitempty"`
	// is_purchase=false (sólo restock) → inventario que ya existía, no es
	// egreso. Ausente/true = compra.
	IsPurchase *bool   `json:"is_purchase,omitempty"`
	Reason     *string `json:"reason,omitempty"`
}

type productResp struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Price        float64   `json:"price"`
	Stock        int       `json:"stock"`
	StockMinimum int       `json:"stock_minimum"`
	Category     *string   `json:"category,omitempty"`
	ImageURL     *string   `json:"image_url,omitempty"`
	Active       bool      `json:"active"`
	LowStock     bool      `json:"low_stock"`
	// AvgUnitCost — costo unitario promedio ponderado (pesos), o ausente
	// cuando el producto no tiene costo capturado. La ficha lo usa para
	// "Costo prom · Precio · Margen". Nullable a propósito: el costo es
	// opcional al crear/resurtir.
	AvgUnitCost *float64 `json:"avg_unit_cost,omitempty"`
}

type listProductsResp struct {
	Items    []productResp     `json:"items"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Totals   listProductTotals `json:"totals"`
}

// listProductTotals — stats globales sobre el filtro completo (no la
// página). El FE alimenta las StatCards con esto en lugar de calcular
// localmente sobre `items` (que solo trae la página visible).
type listProductTotals struct {
	TotalValue float64 `json:"total_value"`
	LowCount   int     `json:"low_count"`
	OutCount   int     `json:"out_count"`
	// Ganancia potencial sobre el stock (Standard). Todos los montos en
	// pesos, solo activos con costo capturado. MarginPct es la ganancia
	// como % del valor de venta de ESOS mismos productos; null cuando
	// ninguno tiene costo (el FE oculta el chip). ProductsWithCost/Total
	// es la cobertura honesta para el hint "X de Y con costo".
	PotentialProfit  float64  `json:"potential_profit"`
	CostValue        float64  `json:"cost_value"`
	MarginPct        *float64 `json:"margin_pct,omitempty"`
	ProductsTotal    int      `json:"products_total"`
	ProductsWithCost int      `json:"products_with_cost"`
}

type adjustStockResp struct {
	NewStock   int       `json:"new_stock"`
	Delta      int       `json:"delta"`
	MovementID uuid.UUID `json:"movement_id"`
}

type createProductResp struct {
	ProductID uuid.UUID `json:"product_id"`
	Name      string    `json:"name"`
	Stock     int       `json:"stock"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *ProductController) handleCreate(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req createProductReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.Create.Execute(c.Request.Context(), prodApp.CreateProductInput{
		GymID:             gymID,
		ActorUserID:       userID,
		Name:              req.Name,
		Price:             req.Price,
		InitialStock:      req.InitialStock,
		StockMinimum:      req.StockMinimum,
		Category:          req.Category,
		ImageURL:          req.ImageURL,
		InitialCost:       req.InitialCost,
		InitialReason:     req.InitialReason,
		InitialIsPurchase: req.InitialIsPurchase,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, createProductResp{
		ProductID: out.ProductID, Name: out.Name, Stock: out.Stock,
	})
}

func (ctrl *ProductController) handleUpdate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req updateProductReq
	if !bindJSON(c, &req) {
		return
	}
	if err := ctrl.Update.Execute(c.Request.Context(), prodApp.UpdateProductInput{
		GymID:        gymID,
		ActorUserID:  userID,
		ProductID:    id,
		Name:         req.Name,
		Price:        req.Price,
		StockMinimum: req.StockMinimum,
		Category:     req.Category,
		ImageURL:     req.ImageURL,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"id": id})
}

func (ctrl *ProductController) handleDeactivate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	if err := ctrl.Deactivate.Execute(c.Request.Context(), prodApp.DeactivateProductInput{
		GymID: gymID, ActorUserID: userID, ProductID: id,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (ctrl *ProductController) handleReactivate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	if err := ctrl.Reactivate.Execute(c.Request.Context(), prodApp.ReactivateProductInput{
		GymID: gymID, ActorUserID: userID, ProductID: id,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (ctrl *ProductController) handleList(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	// FE manda `q` para búsqueda y `status` para el filtro Activos /
	// Inactivos / Todos (ver useProducts.ts). Antes leíamos "search"
	// e "include_inactive" — ambos parámetros viajaban con nombres
	// distintos y el backend los ignoraba silenciosamente.
	out, err := ctrl.List.Execute(c.Request.Context(), prodApp.ListProductsInput{
		GymID:        gymID,
		Search:       c.Query("q"),
		Category:     c.Query("category"),
		ActiveFilter: c.Query("status"),
		LowStockOnly: c.Query("low_stock") == "true",
		Sort:         c.Query("sort"),
		Direction:    c.Query("dir"),
		Page:         page,
		PageSize:     pageSize,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]productResp, 0, len(out.Items))
	for _, p := range out.Items {
		item := toProductResp(p)
		if cost, ok := out.UnitCosts[p.ID]; ok {
			c := cost
			item.AvgUnitCost = &c
		}
		items = append(items, item)
	}
	utils.JsonResponse(c, http.StatusOK, listProductsResp{
		Items: items, Total: out.Total, Page: out.Page, PageSize: out.PageSize,
		Totals: listProductTotals{
			TotalValue:       out.Aggregates.TotalValue,
			LowCount:         out.Aggregates.LowCount,
			OutCount:         out.Aggregates.OutCount,
			PotentialProfit:  out.Aggregates.PotentialProfit,
			CostValue:        out.Aggregates.CostValue,
			MarginPct:        marginPct(out.Aggregates),
			ProductsTotal:    out.Aggregates.ProductsTotal,
			ProductsWithCost: out.Aggregates.ProductsWithCost,
		},
	})
}

// marginPct — ganancia potencial como % del valor de venta de los
// productos CON costo capturado (mismo subconjunto). nil cuando el
// denominador es 0 (ningún activo con costo) para que el FE no muestre un
// chip sin sentido — mismo patrón que delta_pct en los KPIs del dashboard.
func marginPct(a prodRepo.ProductAggregates) *float64 {
	if a.SaleValueWithCost == 0 {
		return nil
	}
	pct := a.PotentialProfit / a.SaleValueWithCost * 100
	return &pct
}

func (ctrl *ProductController) handleAdjust(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req adjustStockReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.Adjust.Execute(c.Request.Context(), prodApp.AdjustStockInput{
		GymID:        gymID,
		ActorUserID:  userID,
		ProductID:    id,
		MovementType: req.MovementType,
		Quantity:     req.Quantity,
		Cost:         req.Cost,
		IsPurchase:   req.IsPurchase,
		Reason:       req.Reason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, adjustStockResp{
		NewStock: out.NewStock, Delta: out.Delta, MovementID: out.MovementID,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func bindJSON[T any](c *gin.Context, dst *T) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return false
	}
	if err := utils.ValidateRequest(*dst); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return false
	}
	return true
}

func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	raw := c.Param(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return uuid.Nil, false
	}
	return id, true
}

func toProductResp(p *productDomain.Product) productResp {
	return productResp{
		ID:           p.ID,
		Name:         p.Name,
		Price:        p.Price,
		Stock:        p.Stock,
		StockMinimum: p.StockMinimum,
		Category:     p.Category,
		ImageURL:     p.ImageURL,
		Active:       p.Active,
		LowStock:     p.IsLowStock(),
	}
}

// Compile-time keep — assert the movement-type constant is reachable so the
// import isn't pruned in builds where only the controller surface is used.
var _ = stockMovementDomain.TypeRestock
