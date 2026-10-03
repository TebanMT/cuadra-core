// Package controllers exposes the billing BC over HTTP. Routes are registered
// under /api/v1; auth + audit context come from the shared middleware.
package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var (
	errBadID               = errors.New("id inválido")
	errBadAuth             = errors.New("autenticación requerida")
	errIdempotencyRequired = errors.New("la operación requiere idempotency_key")
)

// PaymentController bundles UC-018 ... UC-022 + UC-025/UC-026/UC-027.
type PaymentController struct {
	Register       *billingApp.RegisterMembershipPayment
	Settle         *billingApp.SettlePendingBalance
	Receipt        *billingApp.GenerateReceipt
	SendReceipt    *billingApp.SendReceipt
	ListByMember   *billingApp.ListMemberPayments
	ListByGym      *billingApp.ListGymPayments
	Refund         *billingApp.RefundPayment
	RegisterSale   *billingApp.RegisterSale
	OtherIncome    *billingApp.RegisterOtherIncome
	RefundSale     *billingApp.RefundSale
	CorrectSale    *billingApp.CorrectSale
	CorrectPayment *billingApp.CorrectPayment
	CashClose      *reportsApp.CashClose
	Tokens         auth.TokenService
}

func (ctrl *PaymentController) WithOtherIncome(uc *billingApp.RegisterOtherIncome) *PaymentController {
	ctrl.OtherIncome = uc
	return ctrl
}

// WithSaleCorrections enables the product-sale correction/detail endpoints.
// Keeping it as an option preserves construction compatibility for narrow
// controller tests while both production binaries wire the complete use case.
func (ctrl *PaymentController) WithSaleCorrections(uc *billingApp.CorrectSale) *PaymentController {
	ctrl.CorrectSale = uc
	return ctrl
}

func (ctrl *PaymentController) WithPaymentCorrections(uc *billingApp.CorrectPayment) *PaymentController {
	ctrl.CorrectPayment = uc
	return ctrl
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
		api.GET("/payments/:id/refund-preview", middleware.RequireOwner(), ctrl.handleRefundPreview)
		api.POST("/payments/:id/refund", middleware.RequireOwner(), ctrl.handleRefund)
		api.POST("/sales", ctrl.handleRegisterSale)
		api.POST("/payments/other", middleware.RequireOwner(), ctrl.handleOtherIncome)
		api.POST("/sales/:id/refund", middleware.RequireOwner(), ctrl.handleRefundSale)
		if ctrl.CorrectSale != nil {
			api.GET("/sales/:id", ctrl.handleSaleDetail)
			api.POST("/sales/:id/corrections", ctrl.handleCorrectSale)
			api.POST("/sale-corrections/:id/settle", ctrl.handleSettleSaleCorrection)
		}
		if ctrl.CorrectPayment != nil {
			api.POST("/payments/:id/corrections", middleware.RequireOwner(), ctrl.handleCorrectPayment)
			api.GET("/payments/:id/corrections", middleware.RequireOwner(), ctrl.handlePaymentCorrectionHistory)
		}
		// Cash close (cierre de caja) es STANDARD desde ago-2026 (decisión
		// de producto: el corte diario es operación básica del gym, no
		// admin avanzado — dogfooding del gym piloto). El desglose fino de
		// gastos también es Standard; el rol operador puede ocultar el detalle
		// administrativo, pero el cálculo físico del cajón siempre es correcto.
		api.GET("/cash-close", ctrl.handleCashCloseReport)
		api.POST("/cash-close", ctrl.handleCashClose)
		api.POST("/cash-sessions/open", ctrl.handleCashOpen)
		api.POST("/cash-close/reopen", middleware.RequireOwner(), ctrl.handleCashReopen)
		api.POST("/cash-sessions/:id/reopen", middleware.RequireOwner(), ctrl.handleCashReopen)
		api.POST("/cash-sessions/:id/reconcile", ctrl.handleCashReconcile)
		api.POST("/cash-sessions/:id/withdraw", ctrl.handleCashWithdraw)
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
	CashDrawerID     *string `json:"cash_drawer_id,omitempty"`
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
	IdempotencyKey    string             `json:"idempotency_key,omitempty"`
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
	Amount         float64 `json:"amount" validate:"required,gt=0"`
	Method         string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	CashDrawerID   *string `json:"cash_drawer_id,omitempty"`
	PaymentDate    string  `json:"payment_date,omitempty"`
	Notes          *string `json:"notes,omitempty"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
}

type otherIncomeReq struct {
	CashDestination string  `json:"cash_destination,omitempty"`
	Amount          float64 `json:"amount" validate:"required,gt=0"`
	Method          string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	CashDrawerID    *string `json:"cash_drawer_id,omitempty"`
	Description     string  `json:"description" validate:"required,min=3,max=2000"`
	PaymentDate     string  `json:"payment_date,omitempty"`
	IdempotencyKey  string  `json:"idempotency_key,omitempty"`
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
	Reason           string  `json:"reason" validate:"required,min=3,max=200"`
	Method           string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	CashDrawerID     *string `json:"cash_drawer_id,omitempty"`
	Amount           float64 `json:"amount,omitempty"`
	PaymentDate      string  `json:"payment_date,omitempty"`
	RevertMembership bool    `json:"revert_membership,omitempty"`
	IdempotencyKey   string  `json:"idempotency_key,omitempty"`
}

type refundResp struct {
	RefundID         uuid.UUID `json:"refund_id"`
	RefundFolio      string    `json:"folio"`
	Amount           float64   `json:"amount"`
	BalanceCancelled float64   `json:"balance_cancelled,omitempty"`
	Reverted         bool      `json:"reverted_membership"`
}

type refundPreviewResp struct {
	SelectedPaymentID       uuid.UUID `json:"selected_payment_id"`
	RootPaymentID           uuid.UUID `json:"root_payment_id"`
	SelectedRefundable      float64   `json:"selected_refundable"`
	AggregateCollected      float64   `json:"aggregate_collected"`
	AggregateRefunded       float64   `json:"aggregate_refunded"`
	AggregateRefundable     float64   `json:"aggregate_refundable"`
	BalancePending          float64   `json:"balance_pending"`
	RevertMembershipTotal   float64   `json:"revert_membership_total"`
	MembershipRevertAllowed bool      `json:"membership_revert_allowed"`
	MembershipRevertReason  string    `json:"membership_revert_block_reason,omitempty"`
}

type paymentResp struct {
	ID         uuid.UUID  `json:"id"`
	Version    int        `json:"version"`
	SaleID     *uuid.UUID `json:"sale_id,omitempty"`
	Folio      string     `json:"folio"`
	Reference  string     `json:"reference"`
	MemberID   *uuid.UUID `json:"member_id,omitempty"`
	MemberName string     `json:"member_name,omitempty"`
	// SaleSummary — productos de la venta ("Agua 1L ×2 · Proteína") para
	// concept='product' y sus refunds. El FE lo usa como título cuando no
	// hay socio (venta walk-in) y como detalle cuando sí lo hay.
	SaleSummary      string     `json:"sale_summary,omitempty"`
	Amount           float64    `json:"amount"`
	RecognizedAmount float64    `json:"recognized_amount"`
	PaymentMethod    string     `json:"payment_method"`
	CashDrawerID     *uuid.UUID `json:"cash_drawer_id,omitempty"`
	CashDestination  string     `json:"cash_destination"`
	Concept          string     `json:"concept"`
	ParentPaymentID  *uuid.UUID `json:"parent_payment_id,omitempty"`
	DiscountAmount   float64    `json:"discount_amount"`
	DiscountReason   *string    `json:"discount_reason,omitempty"`
	BalancePending   float64    `json:"balance_pending"`
	PaymentDate      string     `json:"payment_date"`
	Notes            *string    `json:"notes,omitempty"`
	OperatorID       uuid.UUID  `json:"operator_id"`
	CreatedAt        time.Time  `json:"created_at"`
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
	drawerID, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	commandKey := strings.TrimSpace(firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")))
	if commandKey == "" {
		utils.ErrorResponse(c, http.StatusBadRequest, errIdempotencyRequired)
		return
	}
	out, err := ctrl.Register.Execute(c.Request.Context(), billingApp.RegisterMembershipPaymentInput{
		GymID:             gymID,
		ActorUserID:       userID,
		MemberID:          memberID,
		MembershipTypeID:  typeID,
		Method:            req.Method,
		CashDrawerID:      drawerID,
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
		IdempotencyKey:    commandKey,
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
	drawerID, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	commandKey := strings.TrimSpace(firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")))
	if commandKey == "" {
		utils.ErrorResponse(c, http.StatusBadRequest, errIdempotencyRequired)
		return
	}
	out, err := ctrl.Settle.Execute(c.Request.Context(), billingApp.SettlePendingBalanceInput{
		GymID:           gymID,
		ActorUserID:     userID,
		ParentPaymentID: id,
		Amount:          req.Amount,
		Method:          req.Method,
		CashDrawerID:    drawerID,
		PaymentDate:     paymentDate,
		Notes:           req.Notes,
		IdempotencyKey:  commandKey,
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

func (ctrl *PaymentController) handleOtherIncome(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	var req otherIncomeReq
	if !bindJSON(c, &req) {
		return
	}
	var paymentDate time.Time
	if req.PaymentDate != "" {
		var err error
		paymentDate, err = time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
	}
	drawerID, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	out, err := ctrl.OtherIncome.Execute(c.Request.Context(), billingApp.RegisterOtherIncomeInput{
		GymID: gymID, ActorUserID: userID, Amount: req.Amount, Method: req.Method, CashDestination: req.CashDestination,
		CashDrawerID: drawerID,
		Description:  req.Description, PaymentDate: paymentDate,
		IdempotencyKey: firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")),
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, gin.H{"payment_id": out.PaymentID, "folio": out.Folio, "amount": out.Amount})
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
		row := toPaymentResp(p)
		if saleID, exists := out.SaleIDs[p.ID]; exists {
			row.SaleID = &saleID
		}
		items = append(items, row)
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
		if s, ok := out.SaleSummaries[p.ID]; ok {
			row.SaleSummary = s
		}
		if saleID, ok := out.SaleIDs[p.ID]; ok {
			row.SaleID = &saleID
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
	drawerID, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	commandKey := strings.TrimSpace(firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")))
	if commandKey == "" {
		utils.ErrorResponse(c, http.StatusBadRequest, errIdempotencyRequired)
		return
	}
	out, err := ctrl.Refund.Execute(c.Request.Context(), billingApp.RefundPaymentInput{
		GymID:            gymID,
		ActorUserID:      userID,
		ParentPaymentID:  id,
		Reason:           req.Reason,
		Method:           req.Method,
		CashDrawerID:     drawerID,
		Amount:           req.Amount,
		PaymentDate:      paymentDate,
		RevertMembership: req.RevertMembership,
		IdempotencyKey:   commandKey,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, refundResp{
		RefundID:         out.RefundID,
		RefundFolio:      out.RefundFolio,
		Amount:           out.Amount,
		BalanceCancelled: out.BalanceCancelled,
		Reverted:         out.Reverted,
	})
}

func (ctrl *PaymentController) handleRefundPreview(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.Refund.Preview(c.Request.Context(), gymID, id)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, refundPreviewResp{
		SelectedPaymentID: out.SelectedPaymentID, RootPaymentID: out.RootPaymentID,
		SelectedRefundable: out.SelectedRefundable,
		AggregateCollected: out.AggregateCollected, AggregateRefunded: out.AggregateRefunded,
		AggregateRefundable: out.AggregateRefundable, BalancePending: out.BalancePending,
		RevertMembershipTotal:   out.RevertMembershipTotal,
		MembershipRevertAllowed: out.MembershipRevertAllowed,
		MembershipRevertReason:  out.MembershipRevertReason,
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

func parseOptionalCashDrawerID(raw *string) (*uuid.UUID, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(*raw)
	if err != nil || id == uuid.Nil {
		return nil, errBadID
	}
	return &id, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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
	ExpectedTotal  *float64           `json:"expected_total,omitempty"`
	Method         string             `json:"payment_method" validate:"required,oneof=cash transfer card"`
	CashDrawerID   *string            `json:"cash_drawer_id,omitempty"`
	MemberID       *string            `json:"member_id,omitempty"`
	Discount       float64            `json:"discount,omitempty"`
	Paid           *float64           `json:"paid,omitempty"`
	PaymentDate    string             `json:"payment_date,omitempty"`
	Notes          *string            `json:"notes,omitempty"`
	Items          []saleLineReq      `json:"line_items" validate:"required,min=1,dive"`
	Promotion      *promotionApplyReq `json:"promotion,omitempty"`
	IdempotencyKey string             `json:"idempotency_key,omitempty"`
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
	Reason         string              `json:"reason" validate:"required,min=3,max=200"`
	Method         string              `json:"method,omitempty" validate:"omitempty,oneof=cash transfer card"`
	CashDrawerID   *string             `json:"cash_drawer_id,omitempty"`
	Amount         float64             `json:"amount,omitempty"`
	PaymentDate    string              `json:"payment_date,omitempty"`
	IdempotencyKey string              `json:"idempotency_key,omitempty"`
	Items          []refundSaleLineReq `json:"line_items,omitempty" validate:"omitempty,dive"`
}

type refundSaleLineReq struct {
	SaleItemID  string  `json:"sale_item_id" validate:"required,uuid"`
	Quantity    int     `json:"quantity" validate:"required,min=1"`
	Amount      float64 `json:"amount,omitempty"`
	Disposition string  `json:"disposition" validate:"required,oneof=returned_to_stock damaged not_returned"`
}

type refundSaleResp struct {
	RefundID         uuid.UUID `json:"refund_id"`
	Amount           float64   `json:"amount"`
	BalanceCancelled float64   `json:"balance_cancelled"`
}

type correctSaleLineReq struct {
	SaleItemID *string `json:"sale_item_id,omitempty"`
	ProductID  string  `json:"product_id" validate:"required,uuid"`
	Quantity   int     `json:"quantity" validate:"required,min=1"`
}

type correctPaymentReq struct {
	CashDestination string   `json:"cash_destination,omitempty"`
	ExpectedVersion int      `json:"expected_version" validate:"required,min=1"`
	Reason          string   `json:"reason" validate:"required,min=3,max=200"`
	Annul           bool     `json:"annul,omitempty"`
	Amount          *float64 `json:"amount,omitempty" validate:"omitempty,gt=0"`
	PaymentMethod   string   `json:"payment_method,omitempty" validate:"omitempty,oneof=cash transfer card"`
	CashDrawerID    *string  `json:"cash_drawer_id,omitempty"`
	PaymentDate     string   `json:"payment_date,omitempty"`
	IdempotencyKey  string   `json:"idempotency_key,omitempty" validate:"omitempty,max=120"`
}

type correctSaleReq struct {
	ExpectedVersion        int                  `json:"expected_version" validate:"min=0"`
	Annul                  bool                 `json:"annul"`
	Reason                 string               `json:"reason" validate:"required,min=3,max=200"`
	Lines                  []correctSaleLineReq `json:"lines" validate:"dive"`
	MoneyResolution        string               `json:"money_resolution" validate:"required,oneof=record_only refund_excess refund_pending"`
	IncreaseResolution     string               `json:"increase_resolution,omitempty" validate:"omitempty,oneof=pending already_collected collect_now"`
	RefundMethod           string               `json:"refund_method,omitempty" validate:"omitempty,oneof=cash transfer card"`
	CashDrawerID           *string              `json:"cash_drawer_id,omitempty"`
	CollectionMethod       string               `json:"collection_method,omitempty" validate:"omitempty,oneof=cash transfer card"`
	CollectionCashDrawerID *string              `json:"collection_cash_drawer_id,omitempty"`
	CollectionDate         string               `json:"collection_date,omitempty"`
	IdempotencyKey         string               `json:"idempotency_key,omitempty"`
}

type settleSaleCorrectionReq struct {
	Method         string  `json:"payment_method" validate:"required,oneof=cash transfer card"`
	CashDrawerID   *string `json:"cash_drawer_id,omitempty"`
	PaymentDate    string  `json:"payment_date,omitempty"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
}

type saleDetailPaymentResp struct {
	Amount           float64 `json:"amount"`
	RecognizedAmount float64 `json:"recognized_amount"`
	PaymentMethod    string  `json:"payment_method"`
	PaymentDate      string  `json:"payment_date"`
}

type saleDetailLineResp struct {
	SaleItemID         uuid.UUID `json:"sale_item_id"`
	ProductID          uuid.UUID `json:"product_id"`
	ProductName        string    `json:"product_name"`
	UnitPrice          float64   `json:"unit_price"`
	Quantity           int       `json:"quantity"`
	LineTotal          float64   `json:"line_total"`
	RefundableQuantity int       `json:"refundable_quantity"`
	RefundedQuantity   int       `json:"refunded_quantity"`
}

type pendingRefundResp struct {
	CorrectionID uuid.UUID `json:"correction_id"`
	AmountDue    float64   `json:"amount_due"`
	CreatedAt    time.Time `json:"created_at"`
	Reason       string    `json:"reason"`
}

type saleDetailResp struct {
	ID               uuid.UUID             `json:"id"`
	Version          int                   `json:"version"`
	PaymentID        uuid.UUID             `json:"payment_id"`
	MemberID         *uuid.UUID            `json:"member_id"`
	Folio            string                `json:"folio"`
	Collected        float64               `json:"collected"`
	Refunded         float64               `json:"refunded"`
	Refundable       float64               `json:"refundable"`
	BalancePending   float64               `json:"balance_pending"`
	Payment          saleDetailPaymentResp `json:"payment"`
	Subtotal         float64               `json:"subtotal"`
	Discount         float64               `json:"discount"`
	Total            float64               `json:"total"`
	Lines            []saleDetailLineResp  `json:"lines"`
	PendingRefundDue float64               `json:"pending_refund_due"`
	PendingRefunds   []pendingRefundResp   `json:"pending_refunds"`
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

// cashCloseExpenseResp — one expense paid from today's register. Fund and
// external expenses are intentionally absent from the cash close.
type cashCloseExpenseResp struct {
	ID            string  `json:"id"`
	Category      string  `json:"category"`
	Description   *string `json:"description,omitempty"`
	Amount        float64 `json:"amount"`
	PaymentMethod string  `json:"payment_method"`
}

type cashCloseClosedResp struct {
	ClosedAt              string  `json:"closed_at"`
	CalculatedCash        float64 `json:"calculated_cash"`
	CurrentCalculatedCash float64 `json:"current_calculated_cash"`
	CountedCash           float64 `json:"counted_cash"`
	Diff                  float64 `json:"diff"`
	IsOutdated            bool    `json:"is_outdated"`
	Reason                *string `json:"reason,omitempty"`
	ClosedByName          *string `json:"closed_by_name,omitempty"`
}

type cashLedgerEntryResp struct {
	ID           uuid.UUID `json:"id"`
	RecordedAt   string    `json:"recorded_at"`
	Amount       float64   `json:"amount"`
	Concept      string    `json:"concept"`
	Reason       string    `json:"reason"`
	OperatorName string    `json:"operator_name"`
}

type cashCloseReportResp struct {
	Timezone             string                      `json:"timezone"`
	Entries              []cashLedgerEntryResp       `json:"entries"`
	SuggestedOpeningCash *float64                    `json:"suggested_opening_cash"`
	Date                 string                      `json:"date"`
	ByMethod             map[string]float64          `json:"by_method"`
	ByConcept            map[string]conceptTotalResp `json:"by_concept"`
	Operators            []operatorTotalResp         `json:"operators"`
	Total                float64                     `json:"total"`
	// RefundsTotal / RefundByMethod van en MAGNITUD POSITIVA (el dominio los
	// guarda negativos). El FE los muestra con un "−" explícito y resta los
	// refunds en efectivo del cajón.
	RefundsTotal     float64                `json:"refunds_total"`
	RefundsCount     int                    `json:"refunds_count"`
	RefundByMethod   map[string]float64     `json:"refund_by_method"`
	Expenses         []cashCloseExpenseResp `json:"expenses,omitempty"`
	ExpensesTotal    *float64               `json:"expenses_total,omitempty"`
	ExpensesByMethod map[string]float64     `json:"expenses_by_method,omitempty"`
	// Deprecated: compatibility alias with mixed semantics. New clients use
	// session.activity_cash / expected_cash for physical reconciliation.
	NetTotal              *float64             `json:"net_total,omitempty"`
	CashMovements         []cashMovementResp   `json:"cash_movements"`
	CashInTotal           float64              `json:"cash_in_total"`
	CashOutTotal          float64              `json:"cash_out_total"`
	Closed                *cashCloseClosedResp `json:"closed,omitempty"`
	Session               *cashSessionResp     `json:"session,omitempty"`
	Sessions              []*cashSessionResp   `json:"sessions"`
	CashDrawerID          uuid.UUID            `json:"cash_drawer_id"`
	Drawers               []cashDrawerResp     `json:"drawers"`
	UncoveredCashActivity float64              `json:"uncovered_cash_activity"`
	RequiresNewSession    bool                 `json:"requires_new_session"`
}

type cashSessionResp struct {
	CurrentExpectedCash     *float64  `json:"current_expected_cash,omitempty"`
	FinishedAt              *string   `json:"finished_at"`
	ClosedByName            *string   `json:"closed_by_name"`
	DiscrepancyReason       *string   `json:"discrepancy_reason"`
	ID                      uuid.UUID `json:"id"`
	DrawerID                uuid.UUID `json:"drawer_id"`
	DrawerCode              string    `json:"drawer_code"`
	OperationalDate         string    `json:"operational_date"`
	Sequence                int       `json:"sequence"`
	Status                  string    `json:"status"`
	OpeningCash             float64   `json:"opening_cash"`
	OpeningCashKnown        bool      `json:"opening_cash_known"`
	ActivityCash            float64   `json:"activity_cash"`
	ExpectedCash            float64   `json:"expected_cash"`
	CountedCash             *float64  `json:"counted_cash"`
	Difference              *float64  `json:"difference"`
	CashLeft                *float64  `json:"cash_left"`
	WithdrawnCash           *float64  `json:"withdrawn_cash"`
	WithdrawalDestination   *string   `json:"withdrawal_destination"`
	OpenedAt                string    `json:"opened_at"`
	ClosedAt                *string   `json:"closed_at"`
	ReconciledAt            *string   `json:"reconciled_at"`
	StaleAt                 *string   `json:"stale_at"`
	WithdrawnAt             *string   `json:"withdrawn_at"`
	IsStale                 bool      `json:"is_stale"`
	AdjustedAfterWithdrawal bool      `json:"adjusted_after_withdrawal"`
	IntegrityNote           *string   `json:"integrity_note"`
	CorrectionReason        *string   `json:"correction_reason,omitempty"`
	UncoveredCashActivity   float64   `json:"uncovered_cash_activity"`
	RequiresNewSession      bool      `json:"requires_new_session"`
}
type cashMovementResp struct {
	ID                   uuid.UUID `json:"id"`
	CashDrawerID         uuid.UUID `json:"cash_drawer_id"`
	MovementType         string    `json:"movement_type"`
	Reason               string    `json:"reason"`
	Amount               float64   `json:"amount"`
	OperatorID           uuid.UUID `json:"operator_id"`
	ClassificationStatus string    `json:"classification_status"`
}

type cashDrawerResp struct {
	ID           uuid.UUID `json:"id"`
	Code         string    `json:"code"`
	Name         string    `json:"name"`
	Active       bool      `json:"active"`
	IsMain       bool      `json:"is_main"`
	ActivityCash float64   `json:"activity_cash"`
	HasActivity  bool      `json:"has_activity"`
	SessionCount int       `json:"session_count"`
}

type cashCloseReq struct {
	ExpectedCash      *float64 `json:"expected_cash"`
	SessionID         string   `json:"session_id"`
	Finish            bool     `json:"finish"`
	Date              string   `json:"date" validate:"required"`
	CashDrawerID      *string  `json:"cash_drawer_id,omitempty"`
	DrawerID          *string  `json:"drawer_id,omitempty"` // backward-compatible alias
	OpeningCash       *float64 `json:"opening_cash,omitempty"`
	CountedCash       *float64 `json:"counted_cash,omitempty"`
	DiscrepancyReason *string  `json:"discrepancy_reason,omitempty"`
	CorrectionReason  *string  `json:"correction_reason,omitempty"`
	CashLeft          *float64 `json:"cash_left,omitempty"`
	Withdraw          bool     `json:"withdraw,omitempty"`
}

type cashCloseResp struct {
	CashCloseID    uuid.UUID        `json:"cash_close_id"`
	CalculatedCash float64          `json:"calculated_cash"`
	CountedCash    *float64         `json:"counted_cash,omitempty"`
	Discrepancy    *float64         `json:"discrepancy,omitempty"`
	Session        *cashSessionResp `json:"session,omitempty"`
}

type cashReopenReq struct {
	Date         string  `json:"date,omitempty"`
	CashDrawerID *string `json:"cash_drawer_id,omitempty"`
	DrawerID     *string `json:"drawer_id,omitempty"` // backward-compatible alias
	Reason       string  `json:"reason" validate:"required,min=3,max=200"`
}

type cashReconcileReq struct {
	Finish            bool     `json:"finish"`
	CountedCash       *float64 `json:"counted_cash"`
	DiscrepancyReason *string  `json:"discrepancy_reason,omitempty"`
	CorrectionReason  *string  `json:"correction_reason,omitempty"`
}

type cashWithdrawReq struct {
	CashLeft    float64 `json:"cash_left"`
	Destination string  `json:"destination,omitempty"`
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
	drawerID, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	commandKey := strings.TrimSpace(firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")))
	if commandKey == "" {
		utils.ErrorResponse(c, http.StatusBadRequest, errIdempotencyRequired)
		return
	}
	out, err := ctrl.RegisterSale.Execute(c.Request.Context(), billingApp.RegisterSaleInput{
		ExpectedTotal:  req.ExpectedTotal,
		GymID:          gymID,
		ActorUserID:    userID,
		Method:         req.Method,
		CashDrawerID:   drawerID,
		MemberID:       memberID,
		Discount:       req.Discount,
		Paid:           req.Paid,
		PaymentDate:    paymentDate,
		Notes:          req.Notes,
		Items:          items,
		Promotion:      promo,
		IdempotencyKey: commandKey,
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
	var paymentDate time.Time
	if req.PaymentDate != "" {
		parsed, err := time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		paymentDate = parsed
	}
	drawerID, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	items := make([]refundDomain.ItemInput, 0, len(req.Items))
	for _, line := range req.Items {
		lineID, err := uuid.Parse(line.SaleItemID)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
			return
		}
		items = append(items, refundDomain.ItemInput{
			SaleItemID: lineID, Quantity: line.Quantity, Amount: line.Amount, Disposition: line.Disposition,
		})
	}
	out, err := ctrl.RefundSale.Execute(c.Request.Context(), billingApp.RefundSaleInput{
		GymID:          gymID,
		ActorUserID:    userID,
		SaleID:         id,
		Reason:         req.Reason,
		Method:         req.Method,
		CashDrawerID:   drawerID,
		Amount:         req.Amount,
		PaymentDate:    paymentDate,
		IdempotencyKey: firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")),
		Items:          items,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, refundSaleResp{
		RefundID: out.RefundID, Amount: out.Amount, BalanceCancelled: out.BalanceCancelled,
	})
}

func (ctrl *PaymentController) handleSaleDetail(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	saleID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.CorrectSale.Detail(c.Request.Context(), gymID, saleID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, saleDetailToResp(out))
}

func saleDetailToResp(out *billingApp.SaleDetailOutput) saleDetailResp {
	lines := make([]saleDetailLineResp, 0, len(out.Lines))
	for _, line := range out.Lines {
		lines = append(lines, saleDetailLineResp{
			SaleItemID: line.SaleItemID, ProductID: line.ProductID, ProductName: line.ProductName,
			UnitPrice: line.UnitPrice, Quantity: line.Quantity, LineTotal: line.LineTotal,
			RefundableQuantity: line.RefundableQuantity, RefundedQuantity: line.RefundedQuantity,
		})
	}
	pending := make([]pendingRefundResp, 0, len(out.PendingRefunds))
	for _, item := range out.PendingRefunds {
		pending = append(pending, pendingRefundResp{CorrectionID: item.CorrectionID, AmountDue: item.AmountDue,
			CreatedAt: item.CreatedAt, Reason: item.Reason})
	}
	return saleDetailResp{
		ID: out.ID, Version: out.Version, PaymentID: out.PaymentID, MemberID: out.MemberID, Folio: out.Folio,
		Collected: out.Collected, Refunded: out.Refunded, Refundable: out.Refundable,
		BalancePending: out.BalancePending,
		Payment: saleDetailPaymentResp{Amount: out.Payment.Amount, RecognizedAmount: out.Payment.RecognizedAmount,
			PaymentMethod: out.Payment.PaymentMethod, PaymentDate: out.Payment.PaymentDate.Format("2006-01-02")},
		Subtotal: out.Subtotal, Discount: out.Discount, Total: out.Total, Lines: lines,
		PendingRefundDue: out.PendingRefundDue, PendingRefunds: pending,
	}
}

func (ctrl *PaymentController) handleCorrectPayment(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	paymentID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req correctPaymentReq
	if !bindJSON(c, &req) {
		return
	}
	drawerID, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	var paymentDate time.Time
	if req.PaymentDate != "" {
		paymentDate, err = time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
	}
	key := strings.TrimSpace(firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")))
	if key == "" {
		utils.ErrorResponse(c, http.StatusBadRequest, errIdempotencyRequired)
		return
	}
	out, err := ctrl.CorrectPayment.Execute(c.Request.Context(), billingApp.CorrectPaymentInput{
		GymID: gymID, ActorUserID: actorID, ActorRole: role, PaymentID: paymentID,
		ExpectedVersion: req.ExpectedVersion, Reason: req.Reason, Annul: req.Annul, Amount: req.Amount,
		PaymentMethod: req.PaymentMethod, CashDrawerID: drawerID, CashDestination: req.CashDestination, PaymentDate: paymentDate, IdempotencyKey: key,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, out)
}

func (ctrl *PaymentController) handlePaymentCorrectionHistory(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	paymentID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	items, err := ctrl.CorrectPayment.History(c.Request.Context(), gymID, paymentID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (ctrl *PaymentController) handleCorrectSale(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	saleID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req correctSaleReq
	if !bindJSON(c, &req) {
		return
	}
	lines := make([]billingApp.CorrectSaleLineInput, 0, len(req.Lines))
	for _, line := range req.Lines {
		productID, err := uuid.Parse(line.ProductID)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
			return
		}
		var saleItemID *uuid.UUID
		if line.SaleItemID != nil && *line.SaleItemID != "" {
			parsed, err := uuid.Parse(*line.SaleItemID)
			if err != nil {
				utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
				return
			}
			saleItemID = &parsed
		}
		lines = append(lines, billingApp.CorrectSaleLineInput{SaleItemID: saleItemID, ProductID: productID, Quantity: line.Quantity})
	}
	drawerID, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	collectionDrawerID, err := parseOptionalCashDrawerID(req.CollectionCashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	var collectionDate time.Time
	if req.CollectionDate != "" {
		collectionDate, err = time.Parse("2006-01-02", req.CollectionDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
	}
	out, err := ctrl.CorrectSale.Execute(c.Request.Context(), billingApp.CorrectSaleInput{
		GymID: gymID, ActorUserID: actorID, ActorRole: role, SaleID: saleID,
		ExpectedVersion: req.ExpectedVersion, Annul: req.Annul, Reason: req.Reason, Lines: lines,
		MoneyResolution: req.MoneyResolution, RefundMethod: req.RefundMethod,
		RefundCashDrawerID: drawerID,
		IncreaseResolution: req.IncreaseResolution, CollectionMethod: req.CollectionMethod,
		CollectionCashDrawerID: collectionDrawerID, CollectionDate: collectionDate,
		IdempotencyKey: firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")),
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	var saleResponse any
	if !out.Annulled {
		detail, detailErr := ctrl.CorrectSale.Detail(c.Request.Context(), gymID, saleID)
		if detailErr != nil {
			utils.ErrorResponse(c, utils.DomainErrorToHttpCode(detailErr), detailErr)
			return
		}
		saleResponse = saleDetailToResp(detail)
	}
	moneyStatus := "settled"
	if out.MoneyEffect.PendingRefundDue > 0 {
		moneyStatus = "refund_pending"
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"correction_id":     out.CorrectionID,
		"correction_type":   out.CorrectionType,
		"annulled":          out.Annulled,
		"idempotency_key":   firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")),
		"sale_version":      out.SaleVersion,
		"sale":              saleResponse,
		"inventory_effects": out.InventoryEffects,
		"money_effect": gin.H{
			"previous_total": out.MoneyEffect.PreviousTotal, "corrected_total": out.MoneyEffect.CorrectedTotal,
			"physical_collected": out.MoneyEffect.PhysicalCollected, "recognized_income": out.MoneyEffect.RecognizedIncome,
			"refunded_now": out.MoneyEffect.RefundedNow, "pending_refund_due": out.MoneyEffect.PendingRefundDue,
			"status":         out.MoneyEffect.Status,
			"old_sale_total": out.MoneyEffect.PreviousTotal, "new_sale_total": out.MoneyEffect.CorrectedTotal,
			"collected": out.MoneyEffect.PhysicalCollected, "refund_due": out.MoneyEffect.PendingRefundDue,
			"money_status": moneyStatus, "refund_id": out.MoneyEffect.RefundID,
			"refund_amount": out.MoneyEffect.RefundedNow, "refund_method": out.MoneyEffect.RefundMethod,
			"additional_collected_now":        out.MoneyEffect.AdditionalCollectedNow,
			"additional_pending":              out.MoneyEffect.AdditionalPending,
			"additional_historical_collected": out.MoneyEffect.AdditionalHistoricalCollected,
			"settlement_id":                   out.MoneyEffect.SettlementID,
		},
	})
}

func (ctrl *PaymentController) handleSettleSaleCorrection(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	correctionID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req settleSaleCorrectionReq
	if !bindJSON(c, &req) {
		return
	}
	var paymentDate time.Time
	if req.PaymentDate != "" {
		parsed, err := time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		paymentDate = parsed
	}
	drawerID, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	out, err := ctrl.CorrectSale.SettlePending(c.Request.Context(), billingApp.SettlePendingRefundInput{
		GymID: gymID, ActorUserID: actorID, ActorRole: role, CorrectionID: correctionID,
		Method: req.Method, CashDrawerID: drawerID, PaymentDate: paymentDate,
		IdempotencyKey: firstNonEmpty(req.IdempotencyKey, c.GetHeader("Idempotency-Key")),
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, gin.H{"refund_id": out.RefundID, "amount": out.Amount,
		"payment_method": out.PaymentMethod, "refunded_on": out.RefundedOn.Format("2006-01-02"),
		"pending_amount": out.PendingAmount})
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
	var drawerID *uuid.UUID
	rawDrawerID := firstNonEmpty(c.Query("cash_drawer_id"), c.Query("drawer_id"))
	if rawDrawerID != "" {
		parsed, err := uuid.Parse(rawDrawerID)
		if err != nil || parsed == uuid.Nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
			return
		}
		drawerID = &parsed
	}
	role, _ := middleware.GetRole(c)
	out, err := ctrl.CashClose.Report(c.Request.Context(), reportsApp.CashCloseReportInput{
		GymID: gymID, Date: date, DrawerID: drawerID, HideAdministrative: role != "owner",
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
	moves := make([]cashMovementResp, len(out.CashMovements))
	for i, m := range out.CashMovements {
		moves[i] = cashMovementResp{ID: m.ID, CashDrawerID: m.CashDrawerID, MovementType: m.MovementType,
			Reason: m.Reason, Amount: m.Amount, OperatorID: m.OperatorID, ClassificationStatus: m.ClassificationStatus}
	}
	drawers := make([]cashDrawerResp, len(out.Drawers))
	for i, drawer := range out.Drawers {
		drawers[i] = cashDrawerResp{ID: drawer.ID, Code: drawer.Code, Name: drawer.Name,
			Active: drawer.Active, IsMain: drawer.IsMain, ActivityCash: drawer.ActivityCash,
			HasActivity: drawer.HasActivity, SessionCount: drawer.SessionCount}
	}
	resp := cashCloseReportResp{
		// El use case puede resolver la fecha por defecto en el día local del
		// gym; responder con la variable del handler emitía 0001-01-01 cuando
		// el caller omitía ?date=.
		Date:           out.Date.Format("2006-01-02"),
		ByMethod:       out.Totals.ByMethod,
		ByConcept:      concepts,
		Operators:      ops,
		Total:          out.Totals.GrandTotal,
		RefundsTotal:   -out.Totals.RefundTotal,
		RefundsCount:   out.Totals.RefundCount,
		RefundByMethod: refundByMethod,
		CashMovements:  moves, CashInTotal: out.CashInTotal, CashOutTotal: out.CashOutTotal,
		CashDrawerID: out.SelectedDrawerID, Drawers: drawers,
	}
	if out.AdministrativeIncluded {
		resp.Expenses = gastos
		resp.ExpensesTotal = &out.ExpensesTotal
		resp.ExpensesByMethod = out.ExpensesByMethod
		resp.NetTotal = &out.NetTotal
	}
	if out.Closed != nil {
		resp.Closed = &cashCloseClosedResp{
			ClosedAt:              out.Closed.ClosedAt.Format(time.RFC3339),
			CalculatedCash:        out.Closed.CalculatedCash,
			CurrentCalculatedCash: out.Closed.CurrentCalculatedCash,
			CountedCash:           out.Closed.CountedCash,
			Diff:                  out.Closed.Diff,
			IsOutdated:            out.Closed.IsOutdated,
			Reason:                out.Closed.Reason,
			ClosedByName:          out.Closed.ClosedByName,
		}
	}
	resp.Session = cashSessionToResp(out.Session)
	resp.Sessions = make([]*cashSessionResp, 0, len(out.Sessions))
	for _, session := range out.Sessions {
		resp.Sessions = append(resp.Sessions, cashSessionToResp(session))
	}
	resp.Timezone = out.Timezone
	resp.SuggestedOpeningCash = out.SuggestedOpeningCash
	resp.Entries = make([]cashLedgerEntryResp, 0, len(out.Entries))
	for _, e := range out.Entries {
		resp.Entries = append(resp.Entries, cashLedgerEntryResp{ID: e.ID, RecordedAt: e.RecordedAt.Format(time.RFC3339Nano), Amount: e.Amount, Concept: e.Concept, Reason: e.Reason, OperatorName: e.OperatorName})
	}
	resp.UncoveredCashActivity = out.UncoveredCashActivity
	resp.RequiresNewSession = out.RequiresNewSession
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
	var drawerID *uuid.UUID
	rawDrawerID := req.CashDrawerID
	if rawDrawerID == nil {
		rawDrawerID = req.DrawerID
	}
	if rawDrawerID != nil && *rawDrawerID != "" {
		parsed, err := uuid.Parse(*rawDrawerID)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
			return
		}
		drawerID = &parsed
	}
	var sessionID uuid.UUID
	if req.SessionID != "" {
		sessionID, err = uuid.Parse(req.SessionID)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
			return
		}
	}
	out, err := ctrl.CashClose.Close(c.Request.Context(), reportsApp.CashCloseInput{
		GymID:       gymID,
		ActorUserID: userID,
		Date:        date,
		DrawerID:    drawerID,
		OpeningCash: req.OpeningCash,
		SessionID:   sessionID, Finish: req.Finish, ExpectedCash: req.ExpectedCash,
		CountedCash:       req.CountedCash,
		DiscrepancyReason: req.DiscrepancyReason,
		CorrectionReason:  req.CorrectionReason,
		CashLeft:          req.CashLeft,
		Withdraw:          req.Withdraw,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := cashCloseResp{
		CashCloseID:    out.CashCloseID,
		CalculatedCash: out.CalculatedCash,
		CountedCash:    out.CountedCash,
		Session:        cashSessionToResp(out.Session),
	}
	if out.Discrepancy != nil {
		resp.Discrepancy = out.Discrepancy
	}
	utils.JsonResponse(c, http.StatusCreated, resp)
}

func (ctrl *PaymentController) handleCashReopen(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	var req cashReopenReq
	if !bindJSON(c, &req) {
		return
	}
	var sessionID uuid.UUID
	if rawSessionID := c.Param("id"); rawSessionID != "" {
		parsed, err := uuid.Parse(rawSessionID)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
			return
		}
		sessionID = parsed
	}
	var date time.Time
	if req.Date != "" {
		parsed, err := time.Parse("2006-01-02", req.Date)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		date = parsed
	} else if sessionID == uuid.Nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("date es obligatorio para la reapertura por fecha"))
		return
	}
	rawDrawerID := req.CashDrawerID
	if rawDrawerID == nil {
		rawDrawerID = req.DrawerID
	}
	drawerID, err := parseOptionalCashDrawerID(rawDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	reason := req.Reason
	if err := ctrl.CashClose.Reopen(c.Request.Context(), reportsApp.CashReopenInput{
		GymID: gymID, ActorUserID: userID, SessionID: sessionID,
		Date: date, DrawerID: drawerID, Reason: &reason,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (ctrl *PaymentController) handleCashReconcile(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	sessionID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req cashReconcileReq
	if !bindJSON(c, &req) {
		return
	}
	if req.CountedCash == nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("counted_cash es obligatorio"))
		return
	}
	out, err := ctrl.CashClose.Reconcile(c.Request.Context(), reportsApp.CashReconcileInput{
		GymID: gymID, ActorUserID: userID, ActorRole: role, SessionID: sessionID,
		CountedCash: *req.CountedCash, Finish: req.Finish, DiscrepancyReason: req.DiscrepancyReason,
		CorrectionReason: req.CorrectionReason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, cashSessionToResp(out))
}

func (ctrl *PaymentController) handleCashWithdraw(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	sessionID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req cashWithdrawReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.CashClose.Withdraw(c.Request.Context(), reportsApp.CashWithdrawInput{
		GymID: gymID, ActorUserID: userID, SessionID: sessionID,
		CashLeft: req.CashLeft, Destination: req.Destination,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, cashSessionToResp(out))
}

func cashSessionToResp(in *reportsApp.CashSessionView) *cashSessionResp {
	if in == nil {
		return nil
	}
	out := &cashSessionResp{
		ID: in.ID, DrawerID: in.DrawerID, DrawerCode: in.DrawerCode,
		OperationalDate: in.OperationalDate.Format("2006-01-02"), Sequence: in.Sequence,
		Status: in.Status, OpeningCash: in.OpeningCash, OpeningCashKnown: in.OpeningCashKnown,
		ActivityCash: in.ActivityCash, ExpectedCash: in.ExpectedCash,
		CountedCash: in.CountedCash, Difference: in.Difference, CashLeft: in.CashLeft,
		WithdrawnCash: in.WithdrawnCash, WithdrawalDestination: in.WithdrawalDestination,
		OpenedAt: in.OpenedAt.Format(time.RFC3339Nano), IsStale: in.IsStale,
		AdjustedAfterWithdrawal: in.AdjustedAfterWithdrawal, IntegrityNote: in.IntegrityNote,
		CorrectionReason:      in.CorrectionReason,
		UncoveredCashActivity: in.UncoveredCashActivity, RequiresNewSession: in.RequiresNewSession,
	}
	out.CurrentExpectedCash = in.CurrentExpectedCash
	out.FinishedAt = formatOptionalTime(in.FinishedAt)
	out.ClosedByName = in.ClosedByName
	out.DiscrepancyReason = in.DiscrepancyReason
	out.ClosedAt = formatOptionalTime(in.ClosedAt)
	out.ReconciledAt = formatOptionalTime(in.ReconciledAt)
	out.StaleAt = formatOptionalTime(in.StaleAt)
	out.WithdrawnAt = formatOptionalTime(in.WithdrawnAt)
	return out
}

func formatOptionalTime(in *time.Time) *string {
	if in == nil {
		return nil
	}
	v := in.UTC().Format(time.RFC3339Nano)
	return &v
}

func toPaymentResp(p *paymentDomain.Payment) paymentResp {
	drawerID := p.CashDrawerID
	if p.EffectiveCashDestination() == "gym_fund" {
		drawerID = nil
	}
	if p.PaymentMethod == paymentDomain.MethodCash && p.EffectiveCashDestination() != "gym_fund" && (drawerID == nil || *drawerID == uuid.Nil) {
		mainDrawerID := p.GymID
		drawerID = &mainDrawerID
	}
	return paymentResp{
		ID:               p.ID,
		Version:          p.Version,
		Folio:            p.Folio,
		Reference:        p.Folio, // FE alias — comprobantes use this as folio.
		MemberID:         p.MemberID,
		Amount:           p.Amount,
		RecognizedAmount: p.RecognizedAmount,
		PaymentMethod:    p.PaymentMethod,
		CashDrawerID:     drawerID,
		CashDestination:  p.EffectiveCashDestination(),
		Concept:          p.Concept,
		ParentPaymentID:  p.ParentPaymentID,
		DiscountAmount:   p.DiscountAmount,
		DiscountReason:   p.DiscountReason,
		BalancePending:   p.BalancePending,
		PaymentDate:      p.PaymentDate.Format("2006-01-02"),
		Notes:            p.Notes,
		OperatorID:       p.OperatorID,
		CreatedAt:        p.CreatedAt,
	}
}

func (ctrl *PaymentController) handleCashOpen(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	var req struct {
		Date         string   `json:"date" validate:"required"`
		CashDrawerID *string  `json:"cash_drawer_id"`
		OpeningCash  *float64 `json:"opening_cash"`
		Sequence     int      `json:"sequence" validate:"min=1"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if req.OpeningCash == nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("indica el efectivo inicial"))
		return
	}
	date, err := time.Parse("2006-01-02", req.Date)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	drawer, err := parseOptionalCashDrawerID(req.CashDrawerID)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return
	}
	out, err := ctrl.CashClose.Open(c.Request.Context(), reportsApp.CashOpenInput{GymID: gymID, ActorUserID: actorID, Date: date, DrawerID: drawer, OpeningCash: *req.OpeningCash, Sequence: req.Sequence})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, cashSessionToResp(out))
}
