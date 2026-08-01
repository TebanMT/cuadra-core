// Package controllers exposes the billing BC over HTTP. Routes are registered
// under /api/v1; auth + audit context come from the shared middleware.
package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var (
	errBadID   = errors.New("id inválido")
	errBadAuth = errors.New("autenticación requerida")
)

// PaymentController bundles UC-018 ... UC-022 + UC-025/UC-026/UC-027.
type PaymentController struct {
	Register     *billingApp.RegisterMembershipPayment
	Settle       *billingApp.SettlePendingBalance
	Receipt      *billingApp.GenerateReceipt
	SendReceipt  *billingApp.SendReceipt
	ListByMember *billingApp.ListMemberPayments
	ListByGym    *billingApp.ListGymPayments
	Refund       *billingApp.RefundPayment
	RegisterSale *billingApp.RegisterSale
	RefundSale   *billingApp.RefundSale
	CashClose    *reportsApp.CashClose
	Tokens       auth.TokenService
	// PlanGate (opcional) restringe cash-close a Plus. Los cobros normales,
	// ventas, comprobantes y refunds permanecen en Standard.
	PlanGate gin.HandlerFunc
}

func NewPaymentController(
	register *billingApp.RegisterMembershipPayment,
	settle *billingApp.SettlePendingBalance,
	receipt *billingApp.GenerateReceipt,
	send *billingApp.SendReceipt,
	listByMember *billingApp.ListMemberPayments,
	listByGym *billingApp.ListGymPayments,
	refund *billingApp.RefundPayment,
	registerSale *billingApp.RegisterSale,
	refundSale *billingApp.RefundSale,
	cashClose *reportsApp.CashClose,
	tokens auth.TokenService,
) *PaymentController {
	return &PaymentController{
		Register: register, Settle: settle, Receipt: receipt, SendReceipt: send,
		ListByMember: listByMember, ListByGym: listByGym, Refund: refund,
		RegisterSale: registerSale, RefundSale: refundSale, CashClose: cashClose,
		Tokens: tokens,
	}
}

func (ctrl *PaymentController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		api.POST("/payments/membership", ctrl.handleRegister)
		api.POST("/payments/:id/settle", ctrl.handleSettle)
		api.GET("/payments/:id/receipt.pdf", ctrl.handleReceipt)
		api.POST("/payments/:id/send-receipt", ctrl.handleSendReceipt)
		api.GET("/members/:id/payments", ctrl.handleListByMember)
		api.GET("/payments", ctrl.handleListByGym)
		api.POST("/payments/:id/refund", middleware.RequireOwner(), ctrl.handleRefund)
		api.POST("/sales", ctrl.handleRegisterSale)
		api.POST("/sales/:id/refund", middleware.RequireOwner(), ctrl.handleRefundSale)
		// Cash close (cierre de caja) es Plus. Standard cierra a mano /
		// con libreta; el módulo de cash close + reportes es admin avanzado.
		plus := api.Group("")
		if ctrl.PlanGate != nil {
			plus.Use(ctrl.PlanGate)
		}
		plus.GET("/cash-close", ctrl.handleCashCloseReport)
		plus.POST("/cash-close", ctrl.handleCashClose)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// promotionApplyReq es el sub-objeto opcional para aplicar una promo al
// cobro. PromotionID y Code son excluyentes — operador eligió de la lista
// vigente o tecleó un código (case-insensitive).
type promotionApplyReq struct {
	PromotionID        *string  `json:"promotion_id,omitempty"`
	Code               *string  `json:"code,omitempty"`
	CompanionMemberIDs []string `json:"companion_member_ids,omitempty"`
	Notes              *string  `json:"notes,omitempty"`
}

type registerPaymentReq struct {
	MemberID         string  `json:"member_id" validate:"required,uuid"`
	MembershipTypeID string  `json:"membership_type_id" validate:"required,uuid"`
	Method           string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	PaymentDate      string  `json:"payment_date,omitempty"` // YYYY-MM-DD
	Notes            *string `json:"notes,omitempty"`
	Discount         float64 `json:"discount_amount,omitempty"`
	DiscountReason   *string `json:"discount_reason,omitempty"`
	PaidNow          float64 `json:"partial_amount,omitempty"`
	// Operator overrides — nil/cero = auto-decisión basada en el plan
	// y el estado del socio.
	ChargeEnrollment  *bool              `json:"charge_enrollment,omitempty"`
	ChargeMaintenance *bool              `json:"charge_maintenance,omitempty"`
	EnrollmentAmount  float64            `json:"enrollment_amount,omitempty"`
	MaintenanceAmount float64            `json:"maintenance_amount,omitempty"`
	Promotion         *promotionApplyReq `json:"promotion,omitempty"`
}

type registerPaymentResp struct {
	PaymentID       uuid.UUID `json:"payment_id"`
	Folio           string    `json:"folio"`
	Subtotal        float64   `json:"subtotal"`
	Discount        float64   `json:"discount"`
	Total           float64   `json:"total"`
	Paid            float64   `json:"paid"`
	BalancePending  float64   `json:"balance_pending"`
	NewMembershipID uuid.UUID `json:"new_membership_id"`
	NewExpiry       string    `json:"new_expiry"`
	EnrollmentChrg  bool      `json:"enrollment_charged"`
	MaintenanceChrg bool      `json:"maintenance_charged"`
	// Datos de la promoción aplicada (omitidos cuando no hubo).
	PromotionAppliedID *uuid.UUID  `json:"promotion_applied_id,omitempty"`
	PromotionName      string      `json:"promotion_name,omitempty"`
	PromotionKind      string      `json:"promotion_kind,omitempty"`
	PromotionExtraDays int         `json:"promotion_extra_days,omitempty"`
	PromotionGiftedIDs []uuid.UUID `json:"promotion_gifted_membership_ids,omitempty"`
}

type settleReq struct {
	Amount      float64 `json:"amount" validate:"required,gt=0"`
	Method      string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	PaymentDate string  `json:"payment_date,omitempty"`
	Notes       *string `json:"notes,omitempty"`
}

type settleResp struct {
	SettlementID      uuid.UUID `json:"settlement_id"`
	SettlementFolio   string    `json:"folio"`
	NewBalancePending float64   `json:"new_balance_pending"`
}

type sendReceiptReq struct {
	Channel   string `json:"channel" validate:"required"`
	Recipient string `json:"recipient,omitempty"`
}

type refundReq struct {
	Reason           string  `json:"reason" validate:"required,min=3,max=500"`
	Method           string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	Amount           float64 `json:"amount,omitempty"`
	PaymentDate      string  `json:"payment_date,omitempty"`
	RevertMembership bool    `json:"revert_membership,omitempty"`
}

type refundResp struct {
	RefundID    uuid.UUID `json:"refund_id"`
	RefundFolio string    `json:"folio"`
	Amount      float64   `json:"amount"`
	Reverted    bool      `json:"reverted_membership"`
}

type paymentResp struct {
	ID              uuid.UUID  `json:"id"`
	Folio           string     `json:"folio"`
	Reference       string     `json:"reference"`
	MemberID        *uuid.UUID `json:"member_id,omitempty"`
	MemberName      string     `json:"member_name,omitempty"`
	Amount          float64    `json:"amount"`
	PaymentMethod   string     `json:"payment_method"`
	Concept         string     `json:"concept"`
	ParentPaymentID *uuid.UUID `json:"parent_payment_id,omitempty"`
	DiscountAmount  float64    `json:"discount_amount"`
	DiscountReason  *string    `json:"discount_reason,omitempty"`
	BalancePending  float64    `json:"balance_pending"`
	PaymentDate     string     `json:"payment_date"`
	Notes           *string    `json:"notes,omitempty"`
	OperatorID      uuid.UUID  `json:"operator_id"`
	CreatedAt       time.Time  `json:"created_at"`
}

// listMemberPaymentsResp agrega el rollup `total_pending` (cuánto debe el
// socio en TOTAL, sin paginación ni filtros) — espejo del razonamiento de
// total_paid en listGymPaymentsResp. El FE ya tipaba este campo
// (PaymentHistoryResponse.total_pending) pero ningún handler lo emitía: el
// banner de deuda del perfil nunca prendía.
type listMemberPaymentsResp struct {
	Items        []paymentResp `json:"items"`
	Total        int           `json:"total"`
	Page         int           `json:"page"`
	PageSize     int           `json:"page_size"`
	TotalPending float64       `json:"total_pending"`
}

// listGymPaymentsResp adds a `total_paid` rollup to the listPaymentsResp
// shape so the cobranza screen can render the day/week/month total in the
// header without the FE having to sum locally (would be wrong with
// pagination cutting the page).
type listGymPaymentsResp struct {
	Items    []paymentResp `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	// total_paid es NETO del set filtrado completo (refunds restan);
	// refund_total trae la magnitud devuelta por separado. Los por-método
	// son netos del método.
	TotalPaid     float64 `json:"total_paid"`
	RefundTotal   float64 `json:"refund_total"`
	CashTotal     float64 `json:"cash_total"`
	TransferTotal float64 `json:"transfer_total"`
	CardTotal     float64 `json:"card_total"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *PaymentController) handleRegister(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req registerPaymentReq
	if !bindJSON(c, &req) {
		return
	}
	memberID, err := uuid.Parse(req.MemberID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	typeID, err := uuid.Parse(req.MembershipTypeID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	// Sin fecha en el request → zero time: el use case ancla el default al
	// día LOCAL del gym (gymLocalPaymentDate). El pre-fill UTC anterior
	// dejaba ese fallback muerto y fechaba refunds/ventas nocturnas en el
	// día siguiente.
	var paymentDate time.Time
	if req.PaymentDate != "" {
		t, err := time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		paymentDate = t
	}
	promo, err := parsePromotion(req.Promotion)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	out, err := ctrl.Register.Execute(c.Request.Context(), billingApp.RegisterMembershipPaymentInput{
		GymID:             gymID,
		ActorUserID:       userID,
		MemberID:          memberID,
		MembershipTypeID:  typeID,
		Method:            req.Method,
		PaymentDate:       paymentDate,
		Notes:             req.Notes,
		Discount:          req.Discount,
		DiscountReason:    req.DiscountReason,
		PaidNow:           req.PaidNow,
		ChargeEnrollment:  req.ChargeEnrollment,
		ChargeMaintenance: req.ChargeMaintenance,
		EnrollmentAmount:  req.EnrollmentAmount,
		MaintenanceAmount: req.MaintenanceAmount,
		Promotion:         promo,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, registerPaymentResp{
		PaymentID:          out.PaymentID,
		Folio:              out.Folio,
		Subtotal:           out.Subtotal,
		Discount:           out.Discount,
		Total:              out.Total,
		Paid:               out.Paid,
		BalancePending:     out.BalancePending,
		NewMembershipID:    out.NewMembershipID,
		NewExpiry:          out.NewExpiry.Format("2006-01-02"),
		EnrollmentChrg:     out.EnrollmentChrg,
		MaintenanceChrg:    out.MaintenanceChrg,
		PromotionAppliedID: out.PromotionAppliedID,
		PromotionName:      out.PromotionName,
		PromotionKind:      out.PromotionKind,
		PromotionExtraDays: out.PromotionExtraDays,
		PromotionGiftedIDs: out.PromotionGiftedIDs,
	})
}

// parsePromotion convierte el sub-DTO HTTP a billingApp.PromotionApply,
// parseando UUIDs. Devuelve nil cuando el operador no envió el campo.
func parsePromotion(req *promotionApplyReq) (*billingApp.PromotionApply, error) {
	if req == nil {
		return nil, nil
	}
	out := &billingApp.PromotionApply{Code: req.Code, Notes: req.Notes}
	if req.PromotionID != nil && *req.PromotionID != "" {
		id, err := uuid.Parse(*req.PromotionID)
		if err != nil {
			return nil, errBadID
		}
		out.PromotionID = &id
	}
	for _, s := range req.CompanionMemberIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, errBadID
		}
		out.CompanionMemberIDs = append(out.CompanionMemberIDs, id)
	}
	return out, nil
}

func (ctrl *PaymentController) handleSettle(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req settleReq
	if !bindJSON(c, &req) {
		return
	}
	// Sin fecha en el request → zero time: el use case ancla el default al
	// día LOCAL del gym (gymLocalPaymentDate). El pre-fill UTC anterior
	// dejaba ese fallback muerto y fechaba refunds/ventas nocturnas en el
	// día siguiente.
	var paymentDate time.Time
	if req.PaymentDate != "" {
		t, err := time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		paymentDate = t
	}
	out, err := ctrl.Settle.Execute(c.Request.Context(), billingApp.SettlePendingBalanceInput{
		GymID:           gymID,
		ActorUserID:     userID,
		ParentPaymentID: id,
		Amount:          req.Amount,
		Method:          req.Method,
		PaymentDate:     paymentDate,
		Notes:           req.Notes,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, settleResp{
		SettlementID:      out.SettlementID,
		SettlementFolio:   out.SettlementFolio,
		NewBalancePending: out.NewBalancePending,
	})
}

func (ctrl *PaymentController) handleReceipt(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.Receipt.Execute(c.Request.Context(), billingApp.GenerateReceiptInput{
		GymID: gymID, PaymentID: id,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Header("Content-Type", out.ContentType)
	c.Header("Content-Disposition", `inline; filename="`+out.SuggestedFilename+`"`)
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write(out.PDF)
}

func (ctrl *PaymentController) handleSendReceipt(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req sendReceiptReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.SendReceipt.Execute(c.Request.Context(), billingApp.SendReceiptInput{
		GymID: gymID, PaymentID: id, Channel: req.Channel, Recipient: req.Recipient,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusAccepted, gin.H{
		"status": out.Status,
		"note":   out.Note,
	})
}

func (ctrl *PaymentController) handleListByMember(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	memberID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "25"))
	var from, to *time.Time
	if v := c.Query("from"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			from = &t
		}
	}
	if v := c.Query("to"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			to = &t
		}
	}
	out, err := ctrl.ListByMember.Execute(c.Request.Context(), billingApp.ListMemberPaymentsInput{
		GymID:         gymID,
		MemberID:      memberID,
		ConceptFilter: c.Query("concept"),
		From:          from,
		To:            to,
		Page:          page,
		PageSize:      pageSize,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]paymentResp, 0, len(out.Items))
	for _, p := range out.Items {
		items = append(items, toPaymentResp(p))
	}
	utils.JsonResponse(c, http.StatusOK, listMemberPaymentsResp{
		Items: items, Total: out.Total, Page: out.Page, PageSize: out.PageSize,
		TotalPending: out.TotalPending,
	})
}

// handleListByGym backs GET /api/v1/payments — gym-wide cobranza screen.
// Defaults to "today" when neither from nor to is provided so the operator
// arriving at the screen sees what's been collected so far.
func (ctrl *PaymentController) handleListByGym(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	var from, to time.Time
	if v := c.Query("from"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			from = t
		}
	}
	if v := c.Query("to"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			to = t
		}
	}
	// Sin from/to → zero times: el use case resuelve "hoy" en el día LOCAL
	// del gym. El default UTC de antes pedía el día de mañana desde las
	// 6 PM de CDMX y la pantalla salía vacía.
	out, err := ctrl.ListByGym.Execute(c.Request.Context(), billingApp.ListGymPaymentsInput{
		GymID:         gymID,
		ConceptFilter: c.Query("concept"),
		MethodFilter:  c.Query("method"),
		From:          from,
		To:            to,
		Page:          page,
		PageSize:      pageSize,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]paymentResp, 0, len(out.Items))
	for _, p := range out.Items {
		row := toPaymentResp(p)
		if p.MemberID != nil {
			if name, ok := out.MemberNames[*p.MemberID]; ok {
				row.MemberName = name
			}
		}
		items = append(items, row)
	}
	utils.JsonResponse(c, http.StatusOK, listGymPaymentsResp{
		Items: items, Total: out.Total, Page: out.Page, PageSize: out.PageSize,
		TotalPaid:     out.TotalPaid,
		RefundTotal:   out.RefundTotal,
		CashTotal:     out.CashTotal,
		TransferTotal: out.TransferTotal,
		CardTotal:     out.CardTotal,
	})
}

func (ctrl *PaymentController) handleRefund(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req refundReq
	if !bindJSON(c, &req) {
		return
	}
	// Sin fecha en el request → zero time: el use case ancla el default al
	// día LOCAL del gym (gymLocalPaymentDate). El pre-fill UTC anterior
	// dejaba ese fallback muerto y fechaba refunds/ventas nocturnas en el
	// día siguiente.
	var paymentDate time.Time
	if req.PaymentDate != "" {
		t, err := time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		paymentDate = t
	}
	out, err := ctrl.Refund.Execute(c.Request.Context(), billingApp.RefundPaymentInput{
		GymID:            gymID,
		ActorUserID:      userID,
		ParentPaymentID:  id,
		Reason:           req.Reason,
		Method:           req.Method,
		Amount:           req.Amount,
		PaymentDate:      paymentDate,
		RevertMembership: req.RevertMembership,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, refundResp{
		RefundID:    out.RefundID,
		RefundFolio: out.RefundFolio,
		Amount:      out.Amount,
		Reverted:    out.Reverted,
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

// ---------------------------------------------------------------------------
// UC-025/UC-026/UC-027 — Sales + cash close
// ---------------------------------------------------------------------------

type saleLineReq struct {
	ProductID string `json:"product_id" validate:"required,uuid"`
	Quantity  int    `json:"quantity" validate:"required,min=1"`
}

// registerSaleReq — body de POST /api/v1/sales. Las JSON keys siguen la
// convención del resto del módulo billing (payment_method, line_items)
// — antes eran `method`/`items` y divergían del FE, lo que dejaba el
// endpoint roto en producción (binding fallaba, FE recibía 400).
//
// `paid` (opcional) habilita el caso "fiado": cuando paid < total, el
// faltante se persiste como balance_pending en el Payment para que se
// liquide después vía POST /api/v1/payments/:id/settle (mismo flujo que
// los abonos a mensualidades). Si se omite, el cobro es completo.
// Requiere `member_id` — no se fía a un walk-in.
type registerSaleReq struct {
	Method      string             `json:"payment_method" validate:"required,oneof=cash transfer card"`
	MemberID    *string            `json:"member_id,omitempty"`
	Discount    float64            `json:"discount,omitempty"`
	Paid        *float64           `json:"paid,omitempty"`
	PaymentDate string             `json:"payment_date,omitempty"`
	Notes       *string            `json:"notes,omitempty"`
	Items       []saleLineReq      `json:"line_items" validate:"required,min=1,dive"`
	Promotion   *promotionApplyReq `json:"promotion,omitempty"`
}

type saleItemResp struct {
	ProductID   uuid.UUID `json:"product_id"`
	ProductName string    `json:"product_name"`
	UnitPrice   float64   `json:"unit_price"`
	Quantity    int       `json:"quantity"`
	LineTotal   float64   `json:"line_total"`
	StockAfter  int       `json:"stock_after"`
}

type registerSaleResp struct {
	SaleID             uuid.UUID      `json:"sale_id"`
	PaymentID          uuid.UUID      `json:"payment_id"`
	Folio              string         `json:"folio"`
	Subtotal           float64        `json:"subtotal"`
	Discount           float64        `json:"discount"`
	Total              float64        `json:"total"`
	Paid               float64        `json:"paid"`
	BalancePending     float64        `json:"balance_pending"`
	Items              []saleItemResp `json:"items"`
	PromotionAppliedID *uuid.UUID     `json:"promotion_applied_id,omitempty"`
	PromotionName      string         `json:"promotion_name,omitempty"`
	PromotionKind      string         `json:"promotion_kind,omitempty"`
}

type refundSaleReq struct {
	Reason string `json:"reason" validate:"required,min=3,max=500"`
	Method string `json:"method" validate:"required,oneof=cash transfer card"`
}

type refundSaleResp struct {
	RefundID uuid.UUID `json:"refund_id"`
	Amount   float64   `json:"amount"`
}

type operatorTotalResp struct {
	OperatorID    uuid.UUID `json:"operator_id"`
	OperatorName  string    `json:"operator_name"`
	Total         float64   `json:"total"`
	PaymentsCount int       `json:"payments_count"`
	SalesCount    int       `json:"sales_count"`
}

// conceptTotalResp matches the FE's `{total, count}` per concept. Same shape
// the dashboard already uses for KPI-like breakdowns so the FE has a single
// pattern.
type conceptTotalResp struct {
	Total float64 `json:"total"`
	Count int     `json:"count"`
}

// cashCloseExpenseResp — one row of the "Gastos del día" section inside
// the cash close. Mirrors the expenses entity at the wire edge so the FE
// can render description / category / amount / method.
type cashCloseExpenseResp struct {
	ID            string  `json:"id"`
	Category      string  `json:"category"`
	Description   *string `json:"description,omitempty"`
	Amount        float64 `json:"amount"`
	PaymentMethod string  `json:"payment_method"`
}

type cashCloseClosedResp struct {
	ClosedAt     string  `json:"closed_at"`
	CountedCash  float64 `json:"counted_cash"`
	Diff         float64 `json:"diff"`
	Reason       *string `json:"reason,omitempty"`
	ClosedByName *string `json:"closed_by_name,omitempty"`
}

type cashCloseReportResp struct {
	Date      string                      `json:"date"`
	ByMethod  map[string]float64          `json:"by_method"`
	ByConcept map[string]conceptTotalResp `json:"by_concept"`
	Operators []operatorTotalResp         `json:"operators"`
	Total     float64                     `json:"total"`
	// RefundsTotal / RefundByMethod van en MAGNITUD POSITIVA (el dominio los
	// guarda negativos). El FE los muestra con un "−" explícito y resta los
	// refunds en efectivo del cajón.
	RefundsTotal     float64                `json:"refunds_total"`
	RefundsCount     int                    `json:"refunds_count"`
	RefundByMethod   map[string]float64     `json:"refund_by_method"`
	Expenses         []cashCloseExpenseResp `json:"expenses"`
	ExpensesTotal    float64                `json:"expenses_total"`
	ExpensesByMethod map[string]float64     `json:"expenses_by_method"`
	NetTotal         float64                `json:"net_total"`
	Closed           *cashCloseClosedResp   `json:"closed,omitempty"`
}

type cashCloseReq struct {
	Date              string   `json:"date" validate:"required"`
	CountedCash       *float64 `json:"counted_cash,omitempty"`
	DiscrepancyReason *string  `json:"discrepancy_reason,omitempty"`
}

type cashCloseResp struct {
	CashCloseID    uuid.UUID `json:"cash_close_id"`
	CalculatedCash float64   `json:"calculated_cash"`
	CountedCash    *float64  `json:"counted_cash,omitempty"`
	Discrepancy    *float64  `json:"discrepancy,omitempty"`
}

func (ctrl *PaymentController) handleRegisterSale(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	var req registerSaleReq
	if !bindJSON(c, &req) {
		return
	}
	items := make([]billingApp.SaleLineInput, len(req.Items))
	for i, it := range req.Items {
		pid, err := uuid.Parse(it.ProductID)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
			return
		}
		items[i] = billingApp.SaleLineInput{ProductID: pid, Quantity: it.Quantity}
	}
	var memberID *uuid.UUID
	if req.MemberID != nil && *req.MemberID != "" {
		mid, err := uuid.Parse(*req.MemberID)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
			return
		}
		memberID = &mid
	}
	// Sin fecha en el request → zero time: el use case ancla el default al
	// día LOCAL del gym (gymLocalPaymentDate). El pre-fill UTC anterior
	// dejaba ese fallback muerto y fechaba refunds/ventas nocturnas en el
	// día siguiente.
	var paymentDate time.Time
	if req.PaymentDate != "" {
		t, err := time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		paymentDate = t
	}
	promo, err := parsePromotion(req.Promotion)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	out, err := ctrl.RegisterSale.Execute(c.Request.Context(), billingApp.RegisterSaleInput{
		GymID:       gymID,
		ActorUserID: userID,
		Method:      req.Method,
		MemberID:    memberID,
		Discount:    req.Discount,
		Paid:        req.Paid,
		PaymentDate: paymentDate,
		Notes:       req.Notes,
		Items:       items,
		Promotion:   promo,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	respItems := make([]saleItemResp, len(out.Items))
	for i, it := range out.Items {
		respItems[i] = saleItemResp{
			ProductID: it.ProductID, ProductName: it.ProductName,
			UnitPrice: it.UnitPrice, Quantity: it.Quantity,
			LineTotal: it.LineTotal, StockAfter: it.StockAfter,
		}
	}
	utils.JsonResponse(c, http.StatusCreated, registerSaleResp{
		SaleID: out.SaleID, PaymentID: out.PaymentID, Folio: out.Folio,
		Subtotal: out.Subtotal, Discount: out.Discount, Total: out.Total,
		Paid: out.Paid, BalancePending: out.BalancePending,
		Items:              respItems,
		PromotionAppliedID: out.PromotionAppliedID,
		PromotionName:      out.PromotionName,
		PromotionKind:      out.PromotionKind,
	})
}

func (ctrl *PaymentController) handleRefundSale(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req refundSaleReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.RefundSale.Execute(c.Request.Context(), billingApp.RefundSaleInput{
		GymID:       gymID,
		ActorUserID: userID,
		SaleID:      id,
		Reason:      req.Reason,
		Method:      req.Method,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, refundSaleResp{
		RefundID: out.RefundID, Amount: out.Amount,
	})
}

func (ctrl *PaymentController) handleCashCloseReport(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	// Sin ?date= el use case resuelve "hoy" en el día LOCAL del gym (el
	// default UTC de antes devolvía la caja de mañana desde las 6 PM).
	var date time.Time
	if dateStr := c.Query("date"); dateStr != "" {
		var err error
		date, err = time.Parse("2006-01-02", dateStr)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
	}
	out, err := ctrl.CashClose.Report(c.Request.Context(), reportsApp.CashCloseReportInput{
		GymID: gymID, Date: date,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	ops := make([]operatorTotalResp, len(out.Totals.ByOperator))
	for i, o := range out.Totals.ByOperator {
		ops[i] = operatorTotalResp{
			OperatorID:    o.OperatorID,
			OperatorName:  o.OperatorName,
			Total:         o.Total,
			PaymentsCount: o.PaymentsN,
			SalesCount:    o.SalesN,
		}
	}
	concepts := make(map[string]conceptTotalResp, len(out.Totals.ByConcept))
	for k, v := range out.Totals.ByConcept {
		concepts[k] = conceptTotalResp{Total: v.Total, Count: v.Count}
	}
	gastos := make([]cashCloseExpenseResp, 0, len(out.Expenses))
	for _, e := range out.Expenses {
		gastos = append(gastos, cashCloseExpenseResp{
			ID:            e.ID.String(),
			Category:      e.Category,
			Description:   e.Description,
			Amount:        e.Amount,
			PaymentMethod: e.PaymentMethod,
		})
	}
	// Refunds a magnitud positiva para el wire (el dominio los guarda negativos).
	refundByMethod := make(map[string]float64, len(out.Totals.RefundByMethod))
	for k, v := range out.Totals.RefundByMethod {
		refundByMethod[k] = -v
	}
	resp := cashCloseReportResp{
		Date:             date.Format("2006-01-02"),
		ByMethod:         out.Totals.ByMethod,
		ByConcept:        concepts,
		Operators:        ops,
		Total:            out.Totals.GrandTotal,
		RefundsTotal:     -out.Totals.RefundTotal,
		RefundsCount:     out.Totals.RefundCount,
		RefundByMethod:   refundByMethod,
		Expenses:         gastos,
		ExpensesTotal:    out.ExpensesTotal,
		ExpensesByMethod: out.ExpensesByMethod,
		NetTotal:         out.NetTotal,
	}
	if out.Closed != nil {
		resp.Closed = &cashCloseClosedResp{
			ClosedAt:     out.Closed.ClosedAt.Format(time.RFC3339),
			CountedCash:  out.Closed.CountedCash,
			Diff:         out.Closed.Diff,
			Reason:       out.Closed.Reason,
			ClosedByName: out.Closed.ClosedByName,
		}
	}
	utils.JsonResponse(c, http.StatusOK, resp)
}

func (ctrl *PaymentController) handleCashClose(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	var req cashCloseReq
	if !bindJSON(c, &req) {
		return
	}
	date, err := time.Parse("2006-01-02", req.Date)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	out, err := ctrl.CashClose.Close(c.Request.Context(), reportsApp.CashCloseInput{
		GymID:             gymID,
		ActorUserID:       userID,
		Date:              date,
		CountedCash:       req.CountedCash,
		DiscrepancyReason: req.DiscrepancyReason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := cashCloseResp{
		CashCloseID:    out.CashCloseID,
		CalculatedCash: out.CalculatedCash,
		CountedCash:    out.CountedCash,
	}
	if out.Discrepancy != nil {
		resp.Discrepancy = out.Discrepancy
	}
	utils.JsonResponse(c, http.StatusCreated, resp)
}

func toPaymentResp(p *paymentDomain.Payment) paymentResp {
	return paymentResp{
		ID:              p.ID,
		Folio:           p.Folio,
		Reference:       p.Folio, // FE alias — comprobantes use this as folio.
		MemberID:        p.MemberID,
		Amount:          p.Amount,
		PaymentMethod:   p.PaymentMethod,
		Concept:         p.Concept,
		ParentPaymentID: p.ParentPaymentID,
		DiscountAmount:  p.DiscountAmount,
		DiscountReason:  p.DiscountReason,
		BalancePending:  p.BalancePending,
		PaymentDate:     p.PaymentDate.Format("2006-01-02"),
		Notes:           p.Notes,
		OperatorID:      p.OperatorID,
		CreatedAt:       p.CreatedAt,
	}
}
