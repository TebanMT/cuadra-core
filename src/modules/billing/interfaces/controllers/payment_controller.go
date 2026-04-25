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

// PaymentController bundles UC-018 ... UC-022.
type PaymentController struct {
	Register     *billingApp.RegisterMembershipPayment
	Settle       *billingApp.SettlePendingBalance
	Receipt      *billingApp.GenerateReceipt
	SendReceipt  *billingApp.SendReceipt
	ListByMember *billingApp.ListMemberPayments
	Refund       *billingApp.RefundPayment
	Tokens       auth.TokenService
}

func NewPaymentController(
	register *billingApp.RegisterMembershipPayment,
	settle *billingApp.SettlePendingBalance,
	receipt *billingApp.GenerateReceipt,
	send *billingApp.SendReceipt,
	listByMember *billingApp.ListMemberPayments,
	refund *billingApp.RefundPayment,
	tokens auth.TokenService,
) *PaymentController {
	return &PaymentController{
		Register: register, Settle: settle, Receipt: receipt, SendReceipt: send,
		ListByMember: listByMember, Refund: refund, Tokens: tokens,
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
		api.POST("/payments/:id/refund", middleware.RequireOwner(), ctrl.handleRefund)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type registerPaymentReq struct {
	MemberID         string  `json:"member_id" validate:"required,uuid"`
	MembershipTypeID string  `json:"membership_type_id" validate:"required,uuid"`
	Method           string  `json:"method" validate:"required,oneof=cash transfer card"`
	PaymentDate      string  `json:"payment_date,omitempty"` // YYYY-MM-DD
	Notes            *string `json:"notes,omitempty"`
	Discount         float64 `json:"discount,omitempty"`
	DiscountReason   *string `json:"discount_reason,omitempty"`
	PaidNow          float64 `json:"paid_now,omitempty"`
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
}

type settleReq struct {
	Amount      float64 `json:"amount" validate:"required,gt=0"`
	Method      string  `json:"method" validate:"required,oneof=cash transfer card"`
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
	Method           string  `json:"method" validate:"required,oneof=cash transfer card"`
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
	MemberID        *uuid.UUID `json:"member_id,omitempty"`
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

type listPaymentsResp struct {
	Items    []paymentResp `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
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
	paymentDate := time.Now().UTC()
	if req.PaymentDate != "" {
		t, err := time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		paymentDate = t
	}
	out, err := ctrl.Register.Execute(c.Request.Context(), billingApp.RegisterMembershipPaymentInput{
		GymID:            gymID,
		ActorUserID:      userID,
		MemberID:         memberID,
		MembershipTypeID: typeID,
		Method:           req.Method,
		PaymentDate:      paymentDate,
		Notes:            req.Notes,
		Discount:         req.Discount,
		DiscountReason:   req.DiscountReason,
		PaidNow:          req.PaidNow,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, registerPaymentResp{
		PaymentID:       out.PaymentID,
		Folio:           out.Folio,
		Subtotal:        out.Subtotal,
		Discount:        out.Discount,
		Total:           out.Total,
		Paid:            out.Paid,
		BalancePending:  out.BalancePending,
		NewMembershipID: out.NewMembershipID,
		NewExpiry:       out.NewExpiry.Format("2006-01-02"),
		EnrollmentChrg:  out.EnrollmentChrg,
		MaintenanceChrg: out.MaintenanceChrg,
	})
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
	paymentDate := time.Now().UTC()
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
	utils.JsonResponse(c, http.StatusOK, listPaymentsResp{
		Items: items, Total: out.Total, Page: out.Page, PageSize: out.PageSize,
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
	paymentDate := time.Now().UTC()
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

func toPaymentResp(p *paymentDomain.Payment) paymentResp {
	return paymentResp{
		ID:              p.ID,
		Folio:           p.Folio,
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
