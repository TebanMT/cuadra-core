// Package controllers exposes the notifications BC over HTTP. Routes are
// registered under /api/v1; auth + audit context come from the shared
// middleware. UC-037 connect, UC-038 templates, UC-041 broadcasts and the
// debug listing all live here.
package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var (
	errBadID   = errors.New("id inválido")
	errBadAuth = errors.New("autenticación requerida")
)

// Controller bundles the use cases under a single registration surface.
type Controller struct {
	Connect          *notiApp.ConnectWhatsApp
	Status           *notiApp.GetWhatsAppStatus
	ListTemplates    *notiApp.ListTemplates
	UpdateTemplate   *notiApp.UpdateTemplate
	Broadcast        *notiApp.Broadcast
	List             *notiApp.ListNotifications
	Tokens           auth.TokenService
}

func NewController(
	connect *notiApp.ConnectWhatsApp,
	status *notiApp.GetWhatsAppStatus,
	listTemplates *notiApp.ListTemplates,
	updateTemplate *notiApp.UpdateTemplate,
	broadcast *notiApp.Broadcast,
	list *notiApp.ListNotifications,
	tokens auth.TokenService,
) *Controller {
	return &Controller{
		Connect:        connect,
		Status:         status,
		ListTemplates:  listTemplates,
		UpdateTemplate: updateTemplate,
		Broadcast:      broadcast,
		List:           list,
		Tokens:         tokens,
	}
}

func (ctrl *Controller) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		api.POST("/gyms/me/whatsapp/connect", middleware.RequireOwner(), ctrl.handleConnect)
		api.GET("/gyms/me/whatsapp/status", ctrl.handleStatus)
		api.GET("/notification-templates", ctrl.handleListTemplates)
		api.PATCH("/notification-templates/:key", middleware.RequireOwner(), ctrl.handleUpdateTemplate)
		api.POST("/broadcasts", middleware.RequireOwner(), ctrl.handleBroadcast)
		api.GET("/notifications", ctrl.handleList)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type connectReq struct {
	Phone string `json:"phone" validate:"required,min=8,max=20"`
}

type connectResp struct {
	GymID       uuid.UUID `json:"gym_id"`
	Phone       string    `json:"phone"`
	SenderID    string    `json:"sender_id"`
	ConnectedAt time.Time `json:"connected_at"`
}

type statusResp struct {
	Connected   bool       `json:"connected"`
	Phone       *string    `json:"phone,omitempty"`
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
}

type templateResp struct {
	Key       string   `json:"key"`
	Channel   string   `json:"channel"`
	Category  string   `json:"category"`
	Variables []string `json:"variables"`
	Default   string   `json:"default_body"`
	Body      string   `json:"body"`
	Enabled   bool     `json:"enabled"`
	Custom    bool     `json:"custom"`
}

type updateTemplateReq struct {
	Body    string `json:"body" validate:"required,min=1,max=1024"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type broadcastReq struct {
	Filter    string `json:"filter,omitempty"`
	Message   string `json:"message" validate:"required,min=1,max=600"`
	Confirmed bool   `json:"confirmed"`
}

type broadcastResp struct {
	Preview     bool      `json:"preview"`
	AudienceN   int       `json:"audience_count"`
	EnqueuedN   int       `json:"enqueued_count"`
	BroadcastID uuid.UUID `json:"broadcast_id"`
}

type notificationResp struct {
	ID                uuid.UUID  `json:"id"`
	Channel           string     `json:"channel"`
	TemplateKey       string     `json:"template_key"`
	RecipientType     string     `json:"recipient_type"`
	RecipientAddress  string     `json:"recipient_address"`
	Status            string     `json:"status"`
	RetryCount        int        `json:"retry_count"`
	ScheduledFor      time.Time  `json:"scheduled_for"`
	SentAt            *time.Time `json:"sent_at,omitempty"`
	FailedAt          *time.Time `json:"failed_at,omitempty"`
	ProviderMessageID *string    `json:"provider_message_id,omitempty"`
	ErrorMessage      *string    `json:"error_message,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

type listNotificationsResp struct {
	Items    []notificationResp `json:"items"`
	Total    int                `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *Controller) handleConnect(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req connectReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.Connect.Execute(c.Request.Context(), notiApp.ConnectWhatsAppInput{
		GymID:       gymID,
		ActorUserID: userID,
		Phone:       req.Phone,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, connectResp{
		GymID:       out.GymID,
		Phone:       out.Phone,
		SenderID:    out.SenderID,
		ConnectedAt: out.ConnectedAt,
	})
}

func (ctrl *Controller) handleStatus(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	out, err := ctrl.Status.Execute(c.Request.Context(), notiApp.GetWhatsAppStatusInput{GymID: gymID})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, statusResp{
		Connected: out.Connected, Phone: out.Phone, ConnectedAt: out.ConnectedAt,
	})
}

func (ctrl *Controller) handleListTemplates(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	rows, err := ctrl.ListTemplates.Execute(c.Request.Context(), gymID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	out := make([]templateResp, 0, len(rows))
	for _, r := range rows {
		out = append(out, templateResp{
			Key: r.Key, Channel: r.Channel, Category: r.Category, Variables: r.Variables,
			Default: r.Default, Body: r.Body, Enabled: r.Enabled, Custom: r.Custom,
		})
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": out})
}

func (ctrl *Controller) handleUpdateTemplate(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	key := c.Param("key")
	var req updateTemplateReq
	if !bindJSON(c, &req) {
		return
	}
	saved, err := ctrl.UpdateTemplate.Execute(c.Request.Context(), notiApp.UpdateTemplateInput{
		GymID:       gymID,
		ActorUserID: userID,
		Key:         key,
		Body:        req.Body,
		Enabled:     req.Enabled,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"key":     saved.TemplateKey,
		"body":    saved.Body,
		"enabled": saved.Enabled,
		"version": saved.Version,
	})
}

func (ctrl *Controller) handleBroadcast(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req broadcastReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.Broadcast.Execute(c.Request.Context(), notiApp.BroadcastInput{
		GymID:       gymID,
		ActorUserID: userID,
		Filter:      notiApp.BroadcastFilter(req.Filter),
		Message:     req.Message,
		Confirmed:   req.Confirmed,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	status := http.StatusOK
	if !out.Preview {
		status = http.StatusCreated
	}
	utils.JsonResponse(c, status, broadcastResp{
		Preview:     out.Preview,
		AudienceN:   out.AudienceN,
		EnqueuedN:   out.EnqueuedN,
		BroadcastID: out.BroadcastID,
	})
}

func (ctrl *Controller) handleList(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	out, err := ctrl.List.Execute(c.Request.Context(), notiApp.ListNotificationsInput{
		GymID:    gymID,
		Status:   c.Query("status"),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]notificationResp, 0, len(out.Items))
	for _, n := range out.Items {
		items = append(items, toNotificationResp(n))
	}
	utils.JsonResponse(c, http.StatusOK, listNotificationsResp{
		Items: items, Total: out.Total, Page: out.Page, PageSize: out.PageSize,
	})
}

func toNotificationResp(n *notiDomain.Notification) notificationResp {
	return notificationResp{
		ID:                n.ID,
		Channel:           n.Channel,
		TemplateKey:       n.TemplateKey,
		RecipientType:     n.RecipientType,
		RecipientAddress:  n.RecipientAddress,
		Status:            n.Status,
		RetryCount:        n.RetryCount,
		ScheduledFor:      n.ScheduledFor,
		SentAt:            n.SentAt,
		FailedAt:          n.FailedAt,
		ProviderMessageID: n.ProviderMessageID,
		ErrorMessage:      n.ErrorMessage,
		CreatedAt:         n.CreatedAt,
	}
}

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

// guard against unused
var _ = errBadID
