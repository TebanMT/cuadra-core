// Package controllers exposes the expenses BC over HTTP. Routes are
// registered under /api/v1; auth + audit context come from the shared
// middleware.
package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var (
	errBadID   = errors.New("id inválido")
	errBadAuth = errors.New("autenticación requerida")
	errBadDate = errors.New("fecha inválida (YYYY-MM-DD)")
)

type ExpenseController struct {
	Create *expApp.CreateExpense
	Update *expApp.UpdateExpense
	Delete *expApp.DeleteExpense
	List   *expApp.ListExpenses
	Tokens auth.TokenService
}

func NewExpenseController(
	create *expApp.CreateExpense,
	update *expApp.UpdateExpense,
	del *expApp.DeleteExpense,
	list *expApp.ListExpenses,
	tokens auth.TokenService,
) *ExpenseController {
	return &ExpenseController{Create: create, Update: update, Delete: del, List: list, Tokens: tokens}
}

func (ctrl *ExpenseController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		api.POST("/expenses", ctrl.handleCreate)
		api.GET("/expenses", ctrl.handleList)
		api.PATCH("/expenses/:id", ctrl.handleUpdate)
		api.DELETE("/expenses/:id", ctrl.handleDelete)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type createExpenseReq struct {
	ExpenseDate   string  `json:"expense_date" validate:"required"`
	Amount        float64 `json:"amount" validate:"required,gt=0"`
	Category      string  `json:"category" validate:"required,oneof=renta servicios mantenimiento sueldos marketing mercaderia_externa otros"`
	Description   *string `json:"description,omitempty"`
	PaymentMethod string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
}

type updateExpenseReq struct {
	ExpenseDate   string  `json:"expense_date" validate:"required"`
	Amount        float64 `json:"amount" validate:"required,gt=0"`
	Category      string  `json:"category" validate:"required,oneof=renta servicios mantenimiento sueldos marketing mercaderia_externa otros"`
	Description   *string `json:"description,omitempty"`
	PaymentMethod string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
}

type expenseResp struct {
	ID            uuid.UUID `json:"id"`
	ExpenseDate   string    `json:"expense_date"`
	Amount        float64   `json:"amount"`
	Category      string    `json:"category"`
	Description   *string   `json:"description,omitempty"`
	PaymentMethod string    `json:"payment_method"`
	CreatedBy     uuid.UUID `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type listExpensesResp struct {
	Items    []expenseResp     `json:"items"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Totals   listExpenseTotals `json:"totals"`
}

type listExpenseTotals struct {
	Total            float64 `json:"total"`
	CashTotal        float64 `json:"cash_total"`
	NonCashTotal     float64 `json:"non_cash_total"`
	DominantCategory string  `json:"dominant_category"`
	DominantCatTotal float64 `json:"dominant_category_total"`
}

type createExpenseResp struct {
	ExpenseID uuid.UUID `json:"expense_id"`
	Amount    float64   `json:"amount"`
	Category  string    `json:"category"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *ExpenseController) handleCreate(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req createExpenseReq
	if !bindJSON(c, &req) {
		return
	}
	date, err := time.Parse("2006-01-02", req.ExpenseDate)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadDate)
		return
	}
	out, err := ctrl.Create.Execute(c.Request.Context(), expApp.CreateExpenseInput{
		GymID:         gymID,
		ActorUserID:   userID,
		ExpenseDate:   date,
		Amount:        req.Amount,
		Category:      req.Category,
		Description:   req.Description,
		PaymentMethod: req.PaymentMethod,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, createExpenseResp{
		ExpenseID: out.ExpenseID, Amount: out.Amount, Category: out.Category,
	})
}

func (ctrl *ExpenseController) handleUpdate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req updateExpenseReq
	if !bindJSON(c, &req) {
		return
	}
	date, err := time.Parse("2006-01-02", req.ExpenseDate)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadDate)
		return
	}
	if err := ctrl.Update.Execute(c.Request.Context(), expApp.UpdateExpenseInput{
		GymID:         gymID,
		ActorUserID:   userID,
		ExpenseID:     id,
		ExpenseDate:   date,
		Amount:        req.Amount,
		Category:      req.Category,
		Description:   req.Description,
		PaymentMethod: req.PaymentMethod,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"id": id})
}

func (ctrl *ExpenseController) handleDelete(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	if err := ctrl.Delete.Execute(c.Request.Context(), expApp.DeleteExpenseInput{
		GymID: gymID, ActorUserID: userID, ExpenseID: id,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (ctrl *ExpenseController) handleList(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	in := expApp.ListExpensesInput{
		GymID:         gymID,
		Category:      c.Query("category"),
		PaymentMethod: c.Query("payment_method"),
		Search:        c.Query("q"),
		Sort:          c.Query("sort"),
		Direction:     c.Query("dir"),
		Page:          page,
		PageSize:      pageSize,
	}
	if v := c.Query("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadDate)
			return
		}
		in.From = &t
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadDate)
			return
		}
		in.To = &t
	}
	out, err := ctrl.List.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]expenseResp, 0, len(out.Items))
	for _, e := range out.Items {
		items = append(items, toExpenseResp(e))
	}
	utils.JsonResponse(c, http.StatusOK, listExpensesResp{
		Items: items, Total: out.Total, Page: out.Page, PageSize: out.PageSize,
		Totals: listExpenseTotals{
			Total:            out.Aggregates.Total,
			CashTotal:        out.Aggregates.CashTotal,
			NonCashTotal:     out.Aggregates.NonCashTotal,
			DominantCategory: out.Aggregates.DominantCategory,
			DominantCatTotal: out.Aggregates.DominantCatTotal,
		},
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

func toExpenseResp(e *expenseDomain.Expense) expenseResp {
	return expenseResp{
		ID:            e.ID,
		ExpenseDate:   e.ExpenseDate.Format("2006-01-02"),
		Amount:        e.Amount,
		Category:      e.Category,
		Description:   e.Description,
		PaymentMethod: e.PaymentMethod,
		CreatedBy:     e.CreatedBy,
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
	}
}
