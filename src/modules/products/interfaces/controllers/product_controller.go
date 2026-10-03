// Package controllers exposes the products BC over HTTP. Routes are
// registered under /api/v1; auth + audit context come from the shared
// middleware.
package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	purchaseDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
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
	RegisterPurchase     *prodApp.RegisterInventoryPurchase
	MissingCosts         *prodApp.ListMissingPurchaseCosts
	CompleteCost         *prodApp.CompleteLegacyPurchaseCost
	CreateRemotePurchase *prodApp.CreateRemoteInventoryPurchase
	ReceivePurchase      *prodApp.ReceiveInventoryPurchase
	Create               *prodApp.CreateProduct
	Update               *prodApp.UpdateProduct
	Deactivate           *prodApp.DeactivateProduct
	Reactivate           *prodApp.ReactivateProduct
	List                 *prodApp.ListProducts
	Adjust               *prodApp.AdjustStock
	ListPurchases        *prodApp.ListInventoryPurchases
	PayPurchase          *prodApp.PayInventoryPurchase
	ReopenPurchase       *prodApp.ReopenInventoryPurchase
	CorrectPurchase      *prodApp.CorrectInventoryPurchase
	Tokens               auth.TokenService
}

func (ctrl *ProductController) WithInventoryPurchases(list *prodApp.ListInventoryPurchases,
	pay *prodApp.PayInventoryPurchase, reopen *prodApp.ReopenInventoryPurchase) *ProductController {
	ctrl.ListPurchases = list
	ctrl.PayPurchase = pay
	ctrl.ReopenPurchase = reopen
	return ctrl
}

func (ctrl *ProductController) WithInventoryPurchaseCorrections(correct *prodApp.CorrectInventoryPurchase) *ProductController {
	ctrl.CorrectPurchase = correct
	return ctrl
}

func (ctrl *ProductController) WithRemotePurchases(create *prodApp.CreateRemoteInventoryPurchase, receive *prodApp.ReceiveInventoryPurchase) *ProductController {
	ctrl.CreateRemotePurchase = create
	ctrl.ReceivePurchase = receive
	return ctrl
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
		if ctrl.RegisterPurchase != nil {
			api.POST("/inventory-purchase-registrations", ctrl.handleRegisterPurchase)
		}
		api.POST("/products", ctrl.handleCreate)
		api.PATCH("/products/:id", ctrl.handleUpdate)
		api.DELETE("/products/:id", ctrl.handleDeactivate)
		api.POST("/products/:id/reactivate", ctrl.handleReactivate)
		api.GET("/products", ctrl.handleList)
		api.POST("/products/:id/adjust-stock", ctrl.handleAdjust)
		if ctrl.ReceivePurchase != nil {
			api.GET("/inventory-purchase-deliveries", ctrl.handlePurchaseDeliveries)
			api.POST("/inventory-purchases/:id/receive", ctrl.handleReceivePurchase)
		}
	}
	owner := api.Group("")
	owner.Use(middleware.RequireOwner())
	{
		owner.GET("/inventory-purchases", ctrl.handleListInventoryPurchases)
		if ctrl.MissingCosts != nil && ctrl.CompleteCost != nil {
			owner.GET("/inventory-purchase-missing-costs", ctrl.handleMissingPurchaseCosts)
			owner.POST("/inventory-purchase-missing-costs/:id/complete", ctrl.handleCompletePurchaseCost)
		}
		if ctrl.CreateRemotePurchase != nil {
			owner.POST("/inventory-purchases", ctrl.handleCreateRemotePurchase)
		}
		owner.POST("/inventory-purchases/:id/pay", ctrl.handlePayInventoryPurchase)
		owner.POST("/inventory-purchases/:id/reopen", ctrl.handleReopenInventoryPurchase)
		owner.POST("/inventory-purchases/:id/correct", ctrl.handleCorrectInventoryPurchase)
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
	// Product creation captures existing stock. Real purchases use
	// /adjust-stock with their payment/source fields.
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
	// A purchase is explicit. An omitted/false flag is stock capture only.
	IsPurchase     *bool   `json:"is_purchase,omitempty"`
	PurchaseStatus string  `json:"purchase_status,omitempty" validate:"omitempty,oneof=paid unpaid"`
	PaidOn         string  `json:"paid_on,omitempty"`
	PaymentMethod  string  `json:"payment_method,omitempty" validate:"omitempty,oneof=cash transfer card"`
	PaidFrom       string  `json:"paid_from,omitempty" validate:"omitempty,oneof=cash_drawer cash_register gym_fund external"`
	CashDrawerID   *string `json:"cash_drawer_id,omitempty"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
	Reason         *string `json:"reason,omitempty"`
}

type payInventoryPurchaseReq struct {
	Version        int     `json:"version" validate:"required,gt=0"`
	PaidOn         string  `json:"paid_on" validate:"required"`
	PaymentMethod  string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	PaidFrom       string  `json:"paid_from" validate:"required,oneof=cash_drawer cash_register gym_fund external"`
	CashDrawerID   *string `json:"cash_drawer_id,omitempty"`
	CashMovementID *string `json:"cash_movement_id,omitempty"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
}

type reopenInventoryPurchaseReq struct {
	Version          int    `json:"version" validate:"required,gt=0"`
	CorrectionReason string `json:"correction_reason" validate:"required,min=3,max=200"`
}

type correctInventoryPurchaseReq struct {
	Version          int      `json:"version" validate:"required,gt=0"`
	Quantity         int      `json:"quantity,omitempty"`
	UnitCost         *float64 `json:"unit_cost,omitempty"`
	Annul            bool     `json:"annul,omitempty"`
	CorrectionReason string   `json:"correction_reason" validate:"required,min=3,max=200"`
	IdempotencyKey   string   `json:"idempotency_key,omitempty"`
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
	NewStock       int        `json:"new_stock"`
	Delta          int        `json:"delta"`
	MovementID     uuid.UUID  `json:"movement_id"`
	PurchaseID     *uuid.UUID `json:"purchase_id,omitempty"`
	CashMovementID *uuid.UUID `json:"cash_movement_id,omitempty"`
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
	// El costo frente al operador también es sensible (plan Reports-improve
	// transversal §2): costo unitario y agregados de margen sólo viajan al
	// dueño. Stock, precios de venta y low_stock son operación — van para
	// ambos roles.
	role, _ := middleware.GetRole(c)
	includeCosts := role == "owner"
	items := make([]productResp, 0, len(out.Items))
	for _, p := range out.Items {
		item := toProductResp(p)
		if cost, ok := out.UnitCosts[p.ID]; ok && includeCosts {
			c := cost
			item.AvgUnitCost = &c
		}
		items = append(items, item)
	}
	totals := listProductTotals{
		TotalValue:    out.Aggregates.TotalValue,
		LowCount:      out.Aggregates.LowCount,
		OutCount:      out.Aggregates.OutCount,
		ProductsTotal: out.Aggregates.ProductsTotal,
	}
	if includeCosts {
		totals.PotentialProfit = out.Aggregates.PotentialProfit
		totals.CostValue = out.Aggregates.CostValue
		totals.MarginPct = marginPct(out.Aggregates)
		totals.ProductsWithCost = out.Aggregates.ProductsWithCost
	}
	utils.JsonResponse(c, http.StatusOK, listProductsResp{
		Items: items, Total: out.Total, Page: out.Page, PageSize: out.PageSize,
		Totals: totals,
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
	role, _ := middleware.GetRole(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req adjustStockReq
	if !bindJSON(c, &req) {
		return
	}
	// Fail closed at the HTTP boundary as well as in the use case: operators
	// can receive inventory as an explicit unpaid purchase, but cannot claim a
	// payment from Caja/Fondo/Externo with a forged request.
	if req.IsPurchase != nil && *req.IsPurchase && role != "owner" &&
		strings.TrimSpace(req.PurchaseStatus) != purchaseDomain.StatusUnpaid {
		utils.ErrorResponse(c, http.StatusForbidden, prodErrors.ErrPurchaseOwnerRequired)
		return
	}
	var paidOn *time.Time
	if strings.TrimSpace(req.PaidOn) != "" {
		day, err := time.Parse("2006-01-02", strings.TrimSpace(req.PaidOn))
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		paidOn = &day
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		key = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	}
	var cashDrawerID *uuid.UUID
	if req.CashDrawerID != nil && strings.TrimSpace(*req.CashDrawerID) != "" {
		parsed, parseErr := uuid.Parse(strings.TrimSpace(*req.CashDrawerID))
		if parseErr != nil || parsed == uuid.Nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("cash_drawer_id inválido"))
			return
		}
		cashDrawerID = &parsed
	}
	out, err := ctrl.Adjust.Execute(c.Request.Context(), prodApp.AdjustStockInput{
		GymID:          gymID,
		ActorUserID:    userID,
		ActorRole:      role,
		ProductID:      id,
		MovementType:   req.MovementType,
		Quantity:       req.Quantity,
		Cost:           req.Cost,
		IsPurchase:     req.IsPurchase,
		PurchaseStatus: req.PurchaseStatus,
		PaidOn:         paidOn,
		PaymentMethod:  req.PaymentMethod,
		PaidFrom:       req.PaidFrom,
		CashDrawerID:   cashDrawerID,
		IdempotencyKey: key,
		Reason:         req.Reason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, adjustStockResp{
		NewStock: out.NewStock, Delta: out.Delta, MovementID: out.MovementID,
		PurchaseID: out.PurchaseID, CashMovementID: out.CashMovementID,
	})
}

func (ctrl *ProductController) handleListInventoryPurchases(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("page inválida"))
		return
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if err != nil || pageSize < 1 || pageSize > prodRepo.MaxPageSize {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("page_size debe estar entre 1 y 200"))
		return
	}
	input := prodApp.ListInventoryPurchasesInput{
		GymID: gymID, Status: c.DefaultQuery("status", "unpaid"), Page: page, PageSize: pageSize,
	}
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		from, parseErr := time.Parse("2006-01-02", raw)
		if parseErr != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("from inválida (YYYY-MM-DD)"))
			return
		}
		input.From = &from
	}
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		to, parseErr := time.Parse("2006-01-02", raw)
		if parseErr != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("to inválida (YYYY-MM-DD)"))
			return
		}
		input.To = &to
	}
	input.ReceiptStatus = c.Query("receipt_status")
	result, err := ctrl.ListPurchases.ExecuteList(c.Request.Context(), input)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]gin.H, len(result.Items))
	for i := range result.Items {
		items[i] = inventoryPurchaseToWire(result.Items[i])
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": items, "total": result.Total, "page": result.Page, "page_size": result.PageSize})
}

func (ctrl *ProductController) handlePayInventoryPurchase(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req payInventoryPurchaseReq
	if !bindJSON(c, &req) {
		return
	}
	paidOn, err := time.Parse("2006-01-02", strings.TrimSpace(req.PaidOn))
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	drawerID, err := parseOptionalUUID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	cashMovementID, err := parseOptionalUUIDField(req.CashMovementID, "cash_movement_id")
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		key = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	}
	view, err := ctrl.PayPurchase.Execute(c.Request.Context(), prodApp.PayInventoryPurchaseInput{
		GymID: gymID, ActorUserID: userID, ActorRole: role, PurchaseID: id,
		ExpectedVersion: req.Version, PaidOn: paidOn, PaymentMethod: req.PaymentMethod,
		PaidFrom: req.PaidFrom, CashDrawerID: drawerID, CashMovementID: cashMovementID, IdempotencyKey: key,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, inventoryPurchaseToWire(*view))
}

func (ctrl *ProductController) handleReopenInventoryPurchase(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req reopenInventoryPurchaseReq
	if !bindJSON(c, &req) {
		return
	}
	view, err := ctrl.ReopenPurchase.Execute(c.Request.Context(), prodApp.ReopenInventoryPurchaseInput{
		GymID: gymID, ActorUserID: userID, ActorRole: role, PurchaseID: id,
		ExpectedVersion: req.Version, CorrectionReason: req.CorrectionReason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, inventoryPurchaseToWire(*view))
}

func (ctrl *ProductController) handleCorrectInventoryPurchase(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req correctInventoryPurchaseReq
	if !bindJSON(c, &req) {
		return
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		key = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	}
	out, err := ctrl.CorrectPurchase.Execute(c.Request.Context(), prodApp.CorrectInventoryPurchaseInput{
		GymID: gymID, ActorUserID: userID, ActorRole: role, PurchaseID: id,
		ExpectedVersion: req.Version, Quantity: req.Quantity, UnitCost: req.UnitCost, Annul: req.Annul,
		CorrectionReason: req.CorrectionReason, IdempotencyKey: key,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, out)
}

func inventoryPurchaseToWire(view prodApp.InventoryPurchaseView) gin.H {
	p := view.Purchase
	recordedOn := view.RecordedOn
	if recordedOn == "" {
		recordedOn = p.CreatedAt.Format("2006-01-02")
	}
	var movement any
	if p.StockMovementID != uuid.Nil {
		movement = p.StockMovementID
	}
	receiptStatus := "received"
	var receivedAt any
	var receivedQuantity any
	if p.HasSeparateReceipt() {
		receiptStatus = "pending"
		if view.Receipt != nil {
			receiptStatus = "received"
			receivedAt = view.Receipt.CreatedAt
			receivedQuantity = view.Receipt.Quantity
		}
	} else {
		receivedAt = p.CreatedAt
		receivedQuantity = p.Quantity
	}
	return gin.H{
		"id": p.ID, "version": p.Version, "stock_movement_id": movement, "remote": p.IsCloudManaged(), "separate_receipt": p.HasSeparateReceipt(), "receipt_status": receiptStatus, "received_at": receivedAt, "received_quantity": receivedQuantity,
		"product_id": p.ProductID, "product_name": view.ProductName, "quantity": p.Quantity,
		"unit_cost": p.UnitCost, "total_amount": p.TotalAmount, "status": p.Status,
		"paid_on": formatPurchaseDate(p.PaidOn), "payment_method": p.PaymentMethod, "paid_from": p.PaidFrom,
		"cash_movement_id": p.CashMovementID, "cash_drawer_id": view.CashDrawerID,
		"recorded_on": recordedOn, "created_by": p.CreatedBy, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
	}
}

func formatPurchaseDate(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.Format("2006-01-02")
}

func parseOptionalUUID(raw *string) (*uuid.UUID, error) {
	return parseOptionalUUIDField(raw, "cash_drawer_id")
}

func parseOptionalUUIDField(raw *string, field string) (*uuid.UUID, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	id, err := uuid.Parse(strings.TrimSpace(*raw))
	if err != nil || id == uuid.Nil {
		return nil, errors.New(field + " inválido")
	}
	return &id, nil
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

func (ctrl *ProductController) handleCreateRemotePurchase(c *gin.Context) {
	var req struct {
		ID            string  `json:"id" validate:"required,uuid"`
		ProductID     string  `json:"product_id" validate:"required,uuid"`
		Quantity      int     `json:"quantity" validate:"required,gt=0"`
		UnitCost      float64 `json:"unit_cost" validate:"required,gt=0"`
		PaidOn        string  `json:"paid_on" validate:"required"`
		PaymentMethod string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
		PaidFrom      string  `json:"paid_from" validate:"required,oneof=gym_fund external"`
	}
	if !bindJSON(c, &req) {
		return
	}
	day, err := time.Parse("2006-01-02", req.PaidOn)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	gymID, _ := middleware.GetGymID(c)
	actor, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	view, err := ctrl.CreateRemotePurchase.Execute(c.Request.Context(), prodApp.CreateRemoteInventoryPurchaseInput{ID: uuid.MustParse(req.ID), GymID: gymID, ActorUserID: actor, ActorRole: role, ProductID: uuid.MustParse(req.ProductID), Quantity: req.Quantity, UnitCost: req.UnitCost, PaidOn: day, PaymentMethod: req.PaymentMethod, PaidFrom: req.PaidFrom})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, inventoryPurchaseToWire(*view))
}
func (ctrl *ProductController) handleReceivePurchase(c *gin.Context) {
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Quantity int `json:"quantity" validate:"required,gt=0"`
	}
	if !bindJSON(c, &req) {
		return
	}
	gymID, _ := middleware.GetGymID(c)
	actor, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	receipt, err := ctrl.ReceivePurchase.Execute(c.Request.Context(), prodApp.ReceiveInventoryPurchaseInput{GymID: gymID, PurchaseID: id, ActorUserID: actor, ActorRole: role, Quantity: req.Quantity})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"id": receipt.ID, "quantity": receipt.Quantity, "received_at": receipt.CreatedAt})
}
func (ctrl *ProductController) handlePurchaseDeliveries(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	result, err := ctrl.ListPurchases.ExecuteList(c.Request.Context(), prodApp.ListInventoryPurchasesInput{GymID: gymID, Status: "all", ReceiptStatus: "pending", Page: page, PageSize: 50})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	// Reception needs the merchandise details, not access to the owner's payments.
	items := make([]gin.H, 0, len(result.Items))
	for _, v := range result.Items {
		items = append(items, gin.H{"id": v.Purchase.ID, "product_id": v.Purchase.ProductID, "product_name": v.ProductName, "quantity": v.Purchase.Quantity, "created_at": v.Purchase.CreatedAt})
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": items, "total": result.Total, "page": result.Page, "page_size": result.PageSize})
}

func (ctrl *ProductController) WithLegacyPurchaseCosts(list *prodApp.ListMissingPurchaseCosts, complete *prodApp.CompleteLegacyPurchaseCost) *ProductController {
	ctrl.MissingCosts, ctrl.CompleteCost = list, complete
	return ctrl
}
func (ctrl *ProductController) handleMissingPurchaseCosts(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	from, err := time.Parse("2006-01-02", c.Query("from"))
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	to, err := time.Parse("2006-01-02", c.Query("to"))
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	out, err := ctrl.MissingCosts.Execute(c.Request.Context(), prodApp.MissingPurchaseCostsInput{GymID: gymID, From: from, To: to, Page: page})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, out)
}
func (ctrl *ProductController) handleCompletePurchaseCost(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Version  int     `json:"version"`
		UnitCost float64 `json:"unit_cost"`
		Reason   string  `json:"reason"`
	}
	if err = c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	err = ctrl.CompleteCost.Execute(c.Request.Context(), prodApp.CompleteLegacyPurchaseCostInput{GymID: gymID, ActorUserID: userID, ActorRole: role, MovementID: id, ExpectedVersion: req.Version, UnitCost: req.UnitCost, Reason: req.Reason})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"saved": true})
}

func (ctrl *ProductController) WithPurchaseRegistration(uc *prodApp.RegisterInventoryPurchase) *ProductController {
	ctrl.RegisterPurchase = uc
	return ctrl
}
func (ctrl *ProductController) handleRegisterPurchase(c *gin.Context) {
	var req struct {
		ID            string                      `json:"id" validate:"required,uuid"`
		Items         []prodApp.PurchaseLineInput `json:"items"`
		Received      bool                        `json:"received"`
		Paid          bool                        `json:"paid"`
		PaidOn        string                      `json:"paid_on"`
		PaymentMethod string                      `json:"payment_method"`
		PaidFrom      string                      `json:"paid_from"`
		CashDrawerID  *string                     `json:"cash_drawer_id"`
	}
	if !bindJSON(c, &req) {
		return
	}
	var day time.Time
	var err error
	var drawer *uuid.UUID
	if req.PaidOn != "" {
		day, err = time.Parse("2006-01-02", req.PaidOn)
		if err != nil {
			utils.ErrorResponse(c, 400, err)
			return
		}
	}
	if req.CashDrawerID != nil {
		v, e := uuid.Parse(*req.CashDrawerID)
		if e != nil {
			utils.ErrorResponse(c, 400, e)
			return
		}
		drawer = &v
	}
	gymID, _ := middleware.GetGymID(c)
	actor, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	views, err := ctrl.RegisterPurchase.Execute(c.Request.Context(), prodApp.RegisterInventoryPurchaseInput{ID: uuid.MustParse(req.ID), GymID: gymID, ActorUserID: actor, ActorRole: role, Items: req.Items, Received: req.Received, Paid: req.Paid, PaidOn: day, PaymentMethod: req.PaymentMethod, PaidFrom: req.PaidFrom, CashDrawerID: drawer})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]gin.H, 0, len(views))
	for _, v := range views {
		items = append(items, inventoryPurchaseToWire(v))
	}
	utils.JsonResponse(c, 201, gin.H{"items": items})
}
