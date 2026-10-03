// Package controllers exposes the expenses BC over HTTP. Routes are
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

	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	recurring "github.com/cuadra/cuadra-core/src/modules/expenses/domain/recurring"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var (
	errBadID             = errors.New("id inválido")
	errBadAuth           = errors.New("autenticación requerida")
	errBadDate           = errors.New("fecha inválida (YYYY-MM-DD)")
	errBadVersion        = errors.New("versión requerida")
	errBadIdempotencyKey = errors.New("idempotency_key es requerido")
	errOwnerCashHistory  = errors.New("sólo el propietario puede consultar el historial de caja")
)

type ExpenseController struct {
	Create            *expApp.CreateExpense
	Update            *expApp.UpdateExpense
	Delete            *expApp.DeleteExpense
	List              *expApp.ListExpenses
	Get               *expApp.GetExpense
	CreateCash        *expApp.CreateCashMovement
	UpdateCash        *expApp.UpdateCashMovement
	DeleteCash        *expApp.DeleteCashMovement
	ListCash          *expApp.ListCashMovementsByDate
	ClassifyCash      *expApp.ClassifyCashMovement
	UnclassifyCash    *expApp.UnclassifyCashMovement
	CreateTemplate    *expApp.CreateRecurringExpenseTemplate
	UpdateTemplate    *expApp.UpdateRecurringExpenseTemplate
	SetTemplateActive *expApp.DeactivateRecurringExpenseTemplate
	ListTemplates     *expApp.ListRecurringExpenseTemplates
	Materialize       *expApp.MaterializeExpenseOccurrences
	ListOccurrences   *expApp.ListExpenseOccurrences
	PayOccurrence     *expApp.MarkExpenseOccurrencePaid
	SkipOccurrence    *expApp.SkipExpenseOccurrence
	ReopenOccurrence  *expApp.ReopenExpenseOccurrence
	Tokens            auth.TokenService
	// PlanGate (opcional) gatea únicamente automatización recurrente. El
	// registro básico de gastos y caja forma parte de Standard.
	PlanGate gin.HandlerFunc
}

func (ctrl *ExpenseController) WithPlusOperations(get *expApp.GetExpense, cc *expApp.CreateCashMovement, uc *expApp.UpdateCashMovement, dc *expApp.DeleteCashMovement, lc *expApp.ListCashMovementsByDate, classify *expApp.ClassifyCashMovement, ct *expApp.CreateRecurringExpenseTemplate, ut *expApp.UpdateRecurringExpenseTemplate, active *expApp.DeactivateRecurringExpenseTemplate, lt *expApp.ListRecurringExpenseTemplates, mat *expApp.MaterializeExpenseOccurrences, lo *expApp.ListExpenseOccurrences, pay *expApp.MarkExpenseOccurrencePaid, skip *expApp.SkipExpenseOccurrence) *ExpenseController {
	ctrl.Get = get
	ctrl.CreateCash = cc
	ctrl.UpdateCash = uc
	ctrl.DeleteCash = dc
	ctrl.ListCash = lc
	ctrl.ClassifyCash = classify
	ctrl.CreateTemplate = ct
	ctrl.UpdateTemplate = ut
	ctrl.SetTemplateActive = active
	ctrl.ListTemplates = lt
	ctrl.Materialize = mat
	ctrl.ListOccurrences = lo
	ctrl.PayOccurrence = pay
	ctrl.SkipOccurrence = skip
	return ctrl
}

func (ctrl *ExpenseController) WithCorrectionOperations(unclassify *expApp.UnclassifyCashMovement, reopen *expApp.ReopenExpenseOccurrence) *ExpenseController {
	ctrl.UnclassifyCash = unclassify
	ctrl.ReopenOccurrence = reopen
	return ctrl
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
	// Physical drawer movements are Standard and operational.
	api.POST("/cash-movements", ctrl.handleCreateCash)
	api.GET("/cash-movements", ctrl.handleListCash)

	admin := api.Group("")
	admin.Use(middleware.RequireOwner())
	{
		admin.POST("/expenses", ctrl.handleCreate)
		admin.GET("/expenses", ctrl.handleList)
		admin.GET("/expenses/:id", ctrl.handleGet)
		admin.PATCH("/expenses/:id", ctrl.handleUpdate)
		admin.DELETE("/expenses/:id", ctrl.handleDelete)
		admin.PATCH("/cash-movements/:id", ctrl.handleUpdateCash)
		admin.DELETE("/cash-movements/:id", ctrl.handleDeleteCash)
		admin.GET("/cash-movements/unclassified", ctrl.handleListUnclassified)
		admin.POST("/cash-movements/:id/classify", ctrl.handleClassifyCash)
		admin.POST("/cash-movements/:id/unclassify", ctrl.handleUnclassifyCash)
	}
	plus := api.Group("")
	plus.Use(middleware.RequireOwner())
	if ctrl.PlanGate != nil {
		plus.Use(ctrl.PlanGate)
	}
	plus.GET("/expense-templates", ctrl.handleListTemplates)
	plus.POST("/expense-templates", ctrl.handleCreateTemplate)
	plus.PATCH("/expense-templates/:id", ctrl.handleUpdateTemplate)
	plus.DELETE("/expense-templates/:id", ctrl.handleDeactivateTemplate)
	plus.POST("/expense-templates/:id/reactivate", ctrl.handleReactivateTemplate)
	plus.GET("/expense-occurrences", ctrl.handleListOccurrences)
	plus.POST("/expense-occurrences/materialize", ctrl.handleMaterialize)
	plus.POST("/expense-occurrences/:id/pay", ctrl.handlePayOccurrence)
	plus.POST("/expense-occurrences/:id/skip", ctrl.handleSkipOccurrence)
	plus.POST("/expense-occurrences/:id/reopen", ctrl.handleReopenOccurrence)
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type createExpenseReq struct {
	ExpenseDate    string  `json:"paid_on" validate:"required"`
	Amount         float64 `json:"amount" validate:"required,gt=0"`
	Category       string  `json:"category" validate:"required,oneof=renta servicios nomina mantenimiento marketing insumos_no_inventariables impuestos_y_permisos otros"`
	Description    *string `json:"description,omitempty"`
	PaymentMethod  string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	PaidFrom       string  `json:"paid_from" validate:"required,oneof=cash_drawer cash_register gym_fund external"`
	PayeeName      *string `json:"payee_name,omitempty"`
	Reference      *string `json:"reference,omitempty"`
	Classification string  `json:"classification" validate:"omitempty,oneof=fixed variable"`
	CashDrawerID   *string `json:"cash_drawer_id,omitempty"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
}

type updateExpenseReq struct {
	ExpenseDate      string  `json:"paid_on" validate:"required"`
	Amount           float64 `json:"amount" validate:"required,gt=0"`
	Category         string  `json:"category" validate:"required,oneof=renta servicios nomina mantenimiento marketing insumos_no_inventariables impuestos_y_permisos otros"`
	Description      *string `json:"description,omitempty"`
	PaymentMethod    string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	PaidFrom         string  `json:"paid_from" validate:"required,oneof=cash_drawer cash_register gym_fund external"`
	Version          int     `json:"version" validate:"required,gt=0"`
	PayeeName        *string `json:"payee_name,omitempty"`
	Reference        *string `json:"reference,omitempty"`
	Classification   string  `json:"classification" validate:"omitempty,oneof=fixed variable"`
	CorrectionReason string  `json:"correction_reason" validate:"required,min=3,max=200"`
	CashDrawerID     *string `json:"cash_drawer_id,omitempty"`
}

type expenseResp struct {
	ID                    uuid.UUID  `json:"id"`
	ExpenseDate           string     `json:"expense_date"`
	PaidOn                string     `json:"paid_on"`
	Amount                float64    `json:"amount"`
	Category              string     `json:"category"`
	PayeeName             *string    `json:"payee_name,omitempty"`
	Description           *string    `json:"description,omitempty"`
	PaymentMethod         string     `json:"payment_method"`
	PaidFrom              string     `json:"paid_from"`
	Reference             *string    `json:"reference,omitempty"`
	Classification        string     `json:"classification"`
	Source                string     `json:"source"`
	RecurringOccurrenceID *uuid.UUID `json:"recurring_occurrence_id,omitempty"`
	CashMovementID        *uuid.UUID `json:"cash_movement_id,omitempty"`
	CashDrawerID          *uuid.UUID `json:"cash_drawer_id,omitempty"`
	Version               int        `json:"version"`
	CreatedBy             uuid.UUID  `json:"created_by"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
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
	FixedTotal       float64 `json:"fixed_total"`
	VariableTotal    float64 `json:"variable_total"`
	DominantCategory string  `json:"dominant_category"`
	DominantCatTotal float64 `json:"dominant_category_total"`
}

type createExpenseResp struct {
	ExpenseID    uuid.UUID  `json:"expense_id"`
	Amount       float64    `json:"amount"`
	Category     string     `json:"category"`
	CashDrawerID *uuid.UUID `json:"cash_drawer_id,omitempty"`
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
	drawerID, err := optionalUUID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	key := requestIdempotencyKey(c, req.IdempotencyKey)
	if key == "" || len(key) > 120 {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadIdempotencyKey)
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
		PaidFrom:      req.PaidFrom,
		PayeeName:     req.PayeeName, Reference: req.Reference, Classification: req.Classification,
		CashDrawerID: drawerID, IdempotencyKey: key,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, createExpenseResp{
		ExpenseID: out.ExpenseID, Amount: out.Amount, Category: out.Category, CashDrawerID: out.CashDrawerID,
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
	drawerID, err := optionalUUID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	if err := ctrl.Update.Execute(c.Request.Context(), expApp.UpdateExpenseInput{
		GymID:           gymID,
		ActorUserID:     userID,
		ExpenseID:       id,
		ExpenseDate:     date,
		Amount:          req.Amount,
		Category:        req.Category,
		Description:     req.Description,
		PaymentMethod:   req.PaymentMethod,
		PaidFrom:        req.PaidFrom,
		ExpectedVersion: req.Version, PayeeName: req.PayeeName, Reference: req.Reference, Classification: req.Classification,
		CorrectionReason: req.CorrectionReason,
		CashDrawerID:     drawerID,
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
	version, err := strconv.Atoi(c.Query("version"))
	if err != nil || version <= 0 {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadVersion)
		return
	}
	if err := ctrl.Delete.Execute(c.Request.Context(), expApp.DeleteExpenseInput{
		GymID: gymID, ActorUserID: userID, ExpenseID: id,
		ExpectedVersion: version, CorrectionReason: c.Query("correction_reason"),
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
		GymID:          gymID,
		Category:       c.Query("category"),
		PaymentMethod:  c.Query("payment_method"),
		Source:         c.Query("source"),
		Classification: c.Query("classification"),
		Search:         c.Query("q"),
		Sort:           c.Query("sort"),
		Direction:      c.Query("dir"),
		Page:           page,
		PageSize:       pageSize,
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
			FixedTotal:       out.Aggregates.FixedTotal,
			VariableTotal:    out.Aggregates.VariableTotal,
			DominantCategory: out.Aggregates.DominantCategory,
			DominantCatTotal: out.Aggregates.DominantCatTotal,
		},
	})
}

func (ctrl *ExpenseController) handleGet(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	e, err := ctrl.Get.Execute(c.Request.Context(), gym, id)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toExpenseResp(e))
}

type cashReq struct {
	MovementOn       string  `json:"movement_on" validate:"required"`
	Amount           float64 `json:"amount" validate:"required,gt=0"`
	MovementType     string  `json:"movement_type" validate:"required,oneof=cash_in cash_out"`
	Reason           string  `json:"reason" validate:"required,max=200"`
	Version          int     `json:"version"`
	CashDrawerID     *string `json:"cash_drawer_id,omitempty"`
	DrawerID         *string `json:"drawer_id,omitempty"` // backward-compatible alias
	IdempotencyKey   string  `json:"idempotency_key,omitempty"`
	CorrectionReason string  `json:"correction_reason,omitempty" validate:"omitempty,min=3,max=200"`
	Purpose          string  `json:"purpose,omitempty" validate:"omitempty,oneof=expense non_operating"`
	Category         string  `json:"category,omitempty"`
}

func cashInput(c *gin.Context, r cashReq) (expApp.CashMovementInput, error) {
	gym, _ := middleware.GetGymID(c)
	user, _ := middleware.GetUserID(c)
	d, err := time.Parse("2006-01-02", r.MovementOn)
	if err != nil {
		return expApp.CashMovementInput{}, err
	}
	var drawerID *uuid.UUID
	rawDrawerID := r.CashDrawerID
	if rawDrawerID == nil {
		rawDrawerID = r.DrawerID
	}
	if rawDrawerID != nil && *rawDrawerID != "" {
		parsed, parseErr := uuid.Parse(*rawDrawerID)
		if parseErr != nil || parsed == uuid.Nil {
			return expApp.CashMovementInput{}, errBadID
		}
		drawerID = &parsed
	}
	return expApp.CashMovementInput{GymID: gym, ActorUserID: user, MovementOn: d, Amount: r.Amount, MovementType: r.MovementType, Reason: r.Reason, CashDrawerID: drawerID, IdempotencyKey: r.IdempotencyKey, CorrectionReason: r.CorrectionReason, ExpectedVersion: r.Version}, nil
}
func (ctrl *ExpenseController) handleCreateCash(c *gin.Context) {
	var req cashReq
	if !bindJSON(c, &req) {
		return
	}
	req.IdempotencyKey = requestIdempotencyKey(c, req.IdempotencyKey)
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 120 {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadIdempotencyKey)
		return
	}
	in, err := cashInput(c, req)
	if err != nil {
		utils.ErrorResponse(c, 400, err)
		return
	}
	in.Purpose, in.Category = req.Purpose, req.Category
	m, err := ctrl.CreateCash.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, 201, cashToWire(m))
}
func (ctrl *ExpenseController) handleUpdateCash(c *gin.Context) {
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req cashReq
	if !bindJSON(c, &req) {
		return
	}
	in, err := cashInput(c, req)
	if err != nil {
		utils.ErrorResponse(c, 400, err)
		return
	}
	if in.ExpectedVersion <= 0 {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadVersion)
		return
	}
	m, err := ctrl.UpdateCash.Execute(c.Request.Context(), id, in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, 200, cashToWire(m))
}
func (ctrl *ExpenseController) handleDeleteCash(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	user, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	version, err := strconv.Atoi(c.Query("version"))
	if err != nil || version <= 0 {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadVersion)
		return
	}
	if err := ctrl.DeleteCash.Execute(c.Request.Context(), gym, user, id, expApp.DeleteCashMovementOptions{
		ExpectedVersion: version, CorrectionReason: c.Query("correction_reason"),
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(204)
}
func (ctrl *ExpenseController) handleListCash(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	role, _ := middleware.GetRole(c)
	rawDate := strings.TrimSpace(c.Query("date"))
	if role != "owner" && rawDate == "" {
		utils.ErrorResponse(c, http.StatusForbidden, errOwnerCashHistory)
		return
	}
	var exactDate *time.Time
	if rawDate != "" {
		d, err := time.Parse("2006-01-02", rawDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadDate)
			return
		}
		exactDate = &d
	}
	// Exact date without history filters is the operational compatibility
	// path: a daily close must receive every movement, never a truncated page.
	legacyDaily := exactDate != nil && c.Query("from") == "" && c.Query("to") == "" &&
		c.Query("status") == "" && c.Query("page") == "" && c.Query("page_size") == ""
	if legacyDaily {
		rows, err := ctrl.ListCash.Execute(c.Request.Context(), gym, exactDate, false)
		if err != nil {
			utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
			return
		}
		out := make([]gin.H, len(rows))
		for i, x := range rows {
			out[i] = cashToWire(x)
		}
		utils.JsonResponse(c, http.StatusOK, gin.H{"items": out, "total": len(out), "page": 1, "page_size": len(out)})
		return
	}
	page, pageSize := 1, 50
	var err error
	if raw := c.Query("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("page inválida"))
			return
		}
	}
	if raw := c.Query("page_size"); raw != "" {
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 200 {
			utils.ErrorResponse(c, http.StatusBadRequest, errors.New("page_size debe estar entre 1 y 200"))
			return
		}
	}
	input := expApp.ListCashMovementsInput{GymID: gym, Status: c.Query("status"), Page: page, PageSize: pageSize}
	if exactDate != nil {
		input.From, input.To = exactDate, exactDate
	}
	for raw, target := range map[string]**time.Time{"from": &input.From, "to": &input.To} {
		if value := strings.TrimSpace(c.Query(raw)); value != "" {
			parsed, parseErr := time.Parse("2006-01-02", value)
			if parseErr != nil {
				utils.ErrorResponse(c, http.StatusBadRequest, errBadDate)
				return
			}
			*target = &parsed
		}
	}
	result, err := ctrl.ListCash.ExecuteList(c.Request.Context(), input)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	out := make([]gin.H, len(result.Items))
	for i, x := range result.Items {
		out[i] = cashToWire(x)
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": out, "total": result.Total, "page": result.Page, "page_size": result.PageSize})
}
func (ctrl *ExpenseController) handleListUnclassified(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	result, err := ctrl.ListCash.ExecuteList(c.Request.Context(), expApp.ListCashMovementsInput{
		GymID: gym, Status: "pending", Page: page, PageSize: pageSize,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	out := make([]gin.H, len(result.Items))
	for i, x := range result.Items {
		out[i] = cashToWire(x)
	}
	utils.JsonResponse(c, 200, gin.H{"items": out, "total": result.Total, "page": result.Page, "page_size": result.PageSize})
}
func cashToWire(m *cashDomain.CashMovement) gin.H {
	return gin.H{"id": m.ID, "version": m.Version, "movement_on": m.MovementOn.Format("2006-01-02"), "amount": m.Amount, "movement_type": m.MovementType, "reason": m.Reason, "cash_drawer_id": m.CashDrawerID, "operator_id": m.OperatorID, "expense_id": m.ExpenseID, "classification_status": m.ClassificationStatus, "created_at": m.CreatedAt, "updated_at": m.UpdatedAt}
}

type classifyReq struct {
	NonOperating   bool    `json:"non_operating"`
	PaidOn         string  `json:"paid_on"`
	Category       string  `json:"category"`
	PayeeName      *string `json:"payee_name"`
	Description    *string `json:"description"`
	Reference      *string `json:"reference"`
	Classification string  `json:"classification"`
}

func (ctrl *ExpenseController) handleClassifyCash(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	user, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req classifyReq
	if !bindJSON(c, &req) {
		return
	}
	var paid time.Time
	var err error
	if req.PaidOn != "" {
		paid, err = time.Parse("2006-01-02", req.PaidOn)
		if err != nil {
			utils.ErrorResponse(c, 400, errBadDate)
			return
		}
	}
	e, err := ctrl.ClassifyCash.Execute(c.Request.Context(), expApp.ClassifyCashMovementInput{GymID: gym, ActorUserID: user, MovementID: id, AsNonOperating: req.NonOperating, PaidOn: paid, Category: req.Category, PayeeName: req.PayeeName, Description: req.Description, Reference: req.Reference, Classification: req.Classification})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	if e == nil {
		utils.JsonResponse(c, 200, gin.H{"classification_status": "non_operating"})
		return
	}
	utils.JsonResponse(c, 201, toExpenseResp(e))
}

type correctionReq struct {
	Version          int    `json:"version" validate:"required,gt=0"`
	CorrectionReason string `json:"correction_reason" validate:"required,min=3,max=200"`
}

func (ctrl *ExpenseController) handleUnclassifyCash(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	user, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req correctionReq
	if !bindJSON(c, &req) {
		return
	}
	m, err := ctrl.UnclassifyCash.Execute(c.Request.Context(), expApp.UnclassifyCashMovementInput{
		GymID: gym, ActorUserID: user, ActorRole: role, MovementID: id,
		ExpectedVersion: req.Version, CorrectionReason: req.CorrectionReason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, cashToWire(m))
}

type templateReq struct {
	Name           string  `json:"name" validate:"required,max=120"`
	PayeeName      *string `json:"payee_name"`
	Category       string  `json:"category" validate:"required"`
	ExpectedAmount float64 `json:"expected_amount" validate:"required,gt=0"`
	PaymentMethod  string  `json:"usual_payment_method" validate:"required,oneof=cash transfer card"`
	Classification string  `json:"classification" validate:"required,oneof=fixed variable"`
	Frequency      string  `json:"frequency" validate:"required"`
	StartsOn       string  `json:"starts_on" validate:"required"`
	EndsOn         *string `json:"ends_on"`
	Version        int     `json:"version"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
}

func templateInput(c *gin.Context, r templateReq) (expApp.RecurringTemplateInput, error) {
	gym, _ := middleware.GetGymID(c)
	user, _ := middleware.GetUserID(c)
	s, err := time.Parse("2006-01-02", r.StartsOn)
	if err != nil {
		return expApp.RecurringTemplateInput{}, err
	}
	var end *time.Time
	if r.EndsOn != nil && *r.EndsOn != "" {
		x, e := time.Parse("2006-01-02", *r.EndsOn)
		if e != nil {
			return expApp.RecurringTemplateInput{}, e
		}
		end = &x
	}
	return expApp.RecurringTemplateInput{GymID: gym, ActorUserID: user, Name: r.Name, PayeeName: r.PayeeName, Category: r.Category, ExpectedAmount: r.ExpectedAmount, PaymentMethod: r.PaymentMethod, Classification: r.Classification, Frequency: r.Frequency, StartsOn: s, EndsOn: end, ExpectedVersion: r.Version, IdempotencyKey: r.IdempotencyKey}, nil
}
func (ctrl *ExpenseController) handleCreateTemplate(c *gin.Context) {
	var req templateReq
	if !bindJSON(c, &req) {
		return
	}
	in, err := templateInput(c, req)
	if err != nil {
		utils.ErrorResponse(c, 400, errBadDate)
		return
	}
	in.IdempotencyKey = requestIdempotencyKey(c, in.IdempotencyKey)
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 120 {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadIdempotencyKey)
		return
	}
	t, err := ctrl.CreateTemplate.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, 201, templateToWire(t))
}
func (ctrl *ExpenseController) handleUpdateTemplate(c *gin.Context) {
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req templateReq
	if !bindJSON(c, &req) {
		return
	}
	if req.Version < 1 {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadVersion)
		return
	}
	in, err := templateInput(c, req)
	if err != nil {
		utils.ErrorResponse(c, 400, errBadDate)
		return
	}
	t, err := ctrl.UpdateTemplate.Execute(c.Request.Context(), id, in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, 200, templateToWire(t))
}
func (ctrl *ExpenseController) handleListTemplates(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	rows, err := ctrl.ListTemplates.Execute(c.Request.Context(), gym, c.Query("include_inactive") == "true")
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	out := make([]gin.H, len(rows))
	for i, x := range rows {
		out[i] = templateToWire(x)
	}
	utils.JsonResponse(c, 200, gin.H{"items": out})
}
func (ctrl *ExpenseController) setTemplateActive(c *gin.Context, active bool) {
	gym, _ := middleware.GetGymID(c)
	user, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	version, err := strconv.Atoi(c.Query("version"))
	if err != nil || version < 1 {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadVersion)
		return
	}
	t, err := ctrl.SetTemplateActive.Execute(c.Request.Context(), gym, user, id, active, version)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, templateToWire(t))
}
func (ctrl *ExpenseController) handleDeactivateTemplate(c *gin.Context) {
	ctrl.setTemplateActive(c, false)
}
func (ctrl *ExpenseController) handleReactivateTemplate(c *gin.Context) {
	ctrl.setTemplateActive(c, true)
}
func templateToWire(t *recurring.Template) gin.H {
	return gin.H{"id": t.ID, "version": t.Version, "name": t.Name, "payee_name": t.PayeeName, "category": t.Category, "expected_amount": t.ExpectedAmount, "usual_payment_method": t.PaymentMethod, "classification": t.Classification, "frequency": t.Frequency, "starts_on": t.StartsOn.Format("2006-01-02"), "ends_on": formatDatePtr(t.EndsOn), "next_due_on": t.NextDueOn.Format("2006-01-02"), "active": t.Active}
}

func (ctrl *ExpenseController) handleMaterialize(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	var req struct {
		To string `json:"to" validate:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	to, err := time.Parse("2006-01-02", req.To)
	if err != nil {
		utils.ErrorResponse(c, 400, errBadDate)
		return
	}
	n, err := ctrl.Materialize.Execute(c.Request.Context(), gym, to)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, 200, gin.H{"created": n})
}
func (ctrl *ExpenseController) handleListOccurrences(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	var from, to time.Time
	for key, target := range map[string]*time.Time{"from": &from, "to": &to} {
		if value := c.Query(key); value != "" {
			parsed, err := time.Parse("2006-01-02", value)
			if err != nil {
				utils.ErrorResponse(c, 400, errBadDate)
				return
			}
			*target = parsed
		}
	}
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("page debe ser mayor a cero"))
		return
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if err != nil || pageSize < 1 || pageSize > 200 {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("page_size debe estar entre 1 y 200"))
		return
	}
	result, err := ctrl.ListOccurrences.ExecutePage(c.Request.Context(), expRepo.OccurrenceQuery{GymID: gym, From: from, To: to, Status: c.Query("status")}, page, pageSize)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	out := make([]gin.H, len(result.Items))
	for i, x := range result.Items {
		out[i] = occurrenceToWire(x)
	}
	utils.JsonResponse(c, 200, gin.H{"items": out, "total": result.Total, "page": result.Page, "page_size": result.PageSize})
}

type payReq struct {
	CashMovementID    *string `json:"cash_movement_id,omitempty"`
	ExistingExpenseID *string `json:"expense_id,omitempty"`
	PaidOn            string  `json:"paid_on" validate:"required"`
	Amount            float64 `json:"amount" validate:"required,gt=0"`
	PaymentMethod     string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	PaidFrom          string  `json:"paid_from" validate:"required,oneof=cash_drawer cash_register gym_fund external"`
	PayeeName         *string `json:"payee_name"`
	Description       *string `json:"description"`
	Reference         *string `json:"reference"`
	CashDrawerID      *string `json:"cash_drawer_id,omitempty"`
	IdempotencyKey    string  `json:"idempotency_key,omitempty"`
}

func (ctrl *ExpenseController) handlePayOccurrence(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	user, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req payReq
	if !bindJSON(c, &req) {
		return
	}
	day, err := time.Parse("2006-01-02", req.PaidOn)
	if err != nil {
		utils.ErrorResponse(c, 400, errBadDate)
		return
	}
	drawerID, err := optionalUUID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	key := requestIdempotencyKey(c, req.IdempotencyKey)
	if key == "" || len(key) > 120 {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadIdempotencyKey)
		return
	}
	movementID, err := optionalUUID(req.CashMovementID)
	if err != nil {
		utils.ErrorResponse(c, 400, errBadID)
		return
	}
	existingExpenseID, err := optionalUUID(req.ExistingExpenseID)
	if err != nil {
		utils.ErrorResponse(c, 400, errBadID)
		return
	}
	e, err := ctrl.PayOccurrence.Execute(c.Request.Context(), expApp.MarkOccurrencePaidInput{CashMovementID: movementID, ExistingExpenseID: existingExpenseID, GymID: gym, ActorUserID: user, OccurrenceID: id, PaidOn: day, Amount: req.Amount, PaymentMethod: req.PaymentMethod, PaidFrom: req.PaidFrom, PayeeName: req.PayeeName, Description: req.Description, Reference: req.Reference, CashDrawerID: drawerID, IdempotencyKey: key})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, 200, toExpenseResp(e))
}
func (ctrl *ExpenseController) handleSkipOccurrence(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	user, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason" validate:"required,max=200"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := ctrl.SkipOccurrence.Execute(c.Request.Context(), gym, user, id, req.Reason); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(204)
}

func (ctrl *ExpenseController) handleReopenOccurrence(c *gin.Context) {
	gym, _ := middleware.GetGymID(c)
	user, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req correctionReq
	if !bindJSON(c, &req) {
		return
	}
	o, err := ctrl.ReopenOccurrence.Execute(c.Request.Context(), expApp.ReopenExpenseOccurrenceInput{
		GymID: gym, ActorUserID: user, ActorRole: role, OccurrenceID: id,
		ExpectedVersion: req.Version, CorrectionReason: req.CorrectionReason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, occurrenceToWire(o))
}
func occurrenceToWire(o *recurring.Occurrence) gin.H {
	return gin.H{"name": o.Name, "paid_amount": o.PaidAmount, "paid_on": formatDatePtr(o.PaidOn), "id": o.ID, "version": o.Version, "template_id": o.TemplateID, "due_on": o.DueOn.Format("2006-01-02"), "expected_amount": o.ExpectedAmount, "category": o.Category, "payee_name": o.PayeeName, "payment_method": o.PaymentMethod, "classification": o.Classification, "status": o.Status, "expense_id": o.ExpenseID, "resolved_by": o.ResolvedBy, "resolved_at": o.ResolvedAt, "skip_reason": o.SkipReason}
}
func formatDatePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format("2006-01-02")
}

func optionalUUID(raw *string) (*uuid.UUID, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	id, err := uuid.Parse(strings.TrimSpace(*raw))
	if err != nil || id == uuid.Nil {
		return nil, errBadID
	}
	return &id, nil
}

func requestIdempotencyKey(c *gin.Context, body string) string {
	key := strings.TrimSpace(body)
	if key == "" {
		key = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	}
	return key
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
		PaidOn:        e.PaidOn.Format("2006-01-02"),
		Amount:        e.Amount,
		Category:      e.Category,
		PayeeName:     e.PayeeName,
		Description:   e.Description,
		PaymentMethod: e.PaymentMethod,
		PaidFrom:      expenseDomain.PaidFromWire(e.PaidFrom),
		Reference:     e.Reference, Classification: e.Classification, Source: e.Source, RecurringOccurrenceID: e.RecurringOccurrenceID, CashMovementID: e.CashMovementID, CashDrawerID: e.CashDrawerID, Version: e.Version,
		CreatedBy: e.CreatedBy,
		CreatedAt: e.CreatedAt,
		UpdatedAt: e.UpdatedAt,
	}
}
