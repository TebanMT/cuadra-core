// Package controllers exposes the notifications BC over HTTP. Routes are
// registered under /api/v1; auth + audit context come from the shared
// middleware. UC-037 connect, UC-038 templates, UC-041 broadcasts and the
// debug listing all live here.
package controllers

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notifications "github.com/cuadra/cuadra-core/src/modules/notifications/domain"
	alertDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/alertconfig"
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
//
// The OTP store backs the 2-step WhatsApp connect flow (UC-037). Lives in
// memory because the OTP TTL is 10 min — losing it on restart only means
// the owner re-runs /start. Production should swap for a Redis/SQL impl.
type Controller struct {
	Connect          *notiApp.ConnectWhatsApp
	Disconnect       *notiApp.DisconnectWhatsApp
	Status           *notiApp.GetWhatsAppStatus
	ListTemplates    *notiApp.ListTemplates
	UpdateTemplate   *notiApp.UpdateTemplate
	Broadcast        *notiApp.Broadcast
	List             *notiApp.ListNotifications
	ListOwnerAlerts  *notiApp.ListOwnerAlerts
	UpdateOwnerAlert *notiApp.UpdateOwnerAlert
	WhatsApp         notifications.WhatsAppProvider
	OTPStore         *whatsappOTPStore
	Tokens           auth.TokenService
	// PlanGate (opcional) wrappea las rutas exclusivas de Plus dentro del
	// módulo de notifications. Hoy solo broadcast (envío masivo a socios)
	// es Plus; los recordatorios automáticos + comprobantes son Standard.
	PlanGate gin.HandlerFunc
}

func NewController(
	connect *notiApp.ConnectWhatsApp,
	disconnect *notiApp.DisconnectWhatsApp,
	status *notiApp.GetWhatsAppStatus,
	listTemplates *notiApp.ListTemplates,
	updateTemplate *notiApp.UpdateTemplate,
	broadcast *notiApp.Broadcast,
	list *notiApp.ListNotifications,
	listOwnerAlerts *notiApp.ListOwnerAlerts,
	updateOwnerAlert *notiApp.UpdateOwnerAlert,
	whatsapp notifications.WhatsAppProvider,
	tokens auth.TokenService,
) *Controller {
	return &Controller{
		Connect:          connect,
		Disconnect:       disconnect,
		Status:           status,
		ListTemplates:    listTemplates,
		UpdateTemplate:   updateTemplate,
		Broadcast:        broadcast,
		List:             list,
		ListOwnerAlerts:  listOwnerAlerts,
		UpdateOwnerAlert: updateOwnerAlert,
		WhatsApp:         whatsapp,
		OTPStore:         newWhatsappOTPStore(10 * time.Minute),
		Tokens:           tokens,
	}
}

func (ctrl *Controller) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		// UC-037 — 2-step WhatsApp connect: /start emits an OTP via the
		// provider, /connect verifies it and persists the gym credential.
		api.POST("/gyms/me/whatsapp/start", middleware.RequireOwner(), ctrl.handleWhatsappStart)
		api.POST("/gyms/me/whatsapp/connect", middleware.RequireOwner(), ctrl.handleConnect)
		// Wire shape canónico (item 7): el FE consume `GET /whatsapp` con un
		// shape rico (status, phone_number, stats, last_error). Mantenemos
		// `/status` como alias por compat — el handler es el mismo y devuelve
		// la versión rica. El DELETE desconecta el número.
		api.GET("/gyms/me/whatsapp", ctrl.handleStatus)
		api.GET("/gyms/me/whatsapp/status", ctrl.handleStatus)
		api.DELETE("/gyms/me/whatsapp", middleware.RequireOwner(), ctrl.handleDisconnect)
		api.GET("/notification-templates", ctrl.handleListTemplates)
		api.PATCH("/notification-templates/:key", middleware.RequireOwner(), ctrl.handleUpdateTemplate)
		// Broadcast (envío masivo a socios) es Plus: el gym de barrio
		// arranca con Standard y manda comms 1-a-1; el masivo aplica al
		// gym más activo que justifica Plus.
		plus := api.Group("")
		if ctrl.PlanGate != nil {
			plus.Use(ctrl.PlanGate)
		}
		plus.POST("/broadcasts", middleware.RequireOwner(), ctrl.handleBroadcast)
		api.GET("/notifications", ctrl.handleList)

		// Owner-alerts (UC-040 DA-40.1). Defaults live in
		// alertconfig.Defaults; only owner-flipped rows persist. The PATCH
		// drives audit + sync.
		api.GET("/owner-alerts", ctrl.handleListAlerts)
		api.PATCH("/owner-alerts/:key", middleware.RequireOwner(), ctrl.handleUpdateAlert)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type connectReq struct {
	Phone string `json:"phone" validate:"required,min=8,max=20"`
}

type whatsappStartReq struct {
	PhoneNumber string `json:"phone_number" validate:"required,min=8,max=20"`
}

type whatsappStartResp struct {
	ExpiresAt time.Time `json:"expires_at"`
	// In dev (stdout/mock provider) we surface the generated OTP so the
	// owner can complete the flow without an actual WhatsApp message. When
	// WhatsAppProvider is real this stays empty.
	DevCode string `json:"dev_code,omitempty"`
}

type connectResp struct {
	GymID       uuid.UUID `json:"gym_id"`
	Phone       string    `json:"phone"`
	SenderID    string    `json:"sender_id"`
	ConnectedAt time.Time `json:"connected_at"`
}

// statusResp espeja FE WhatsappState (cuadra-desktop/.../useNotifications.ts).
// Reemplaza al shape antiguo `{connected, phone, connected_at}` que tenía dos
// problemas: (a) los nombres no coincidían con el FE (phone vs phone_number),
// y (b) faltaban stats / last_error que el FE renderea cuando hay conexión.
//
// `stats` es nullable: el sidecar local no tiene contadores reales — los
// agregados de envíos viven cloud-side. Cuando el FE recibe null muestra
// "—" en cada métrica (fallback ya implementado en ConnectedView).
type whatsappStats struct {
	SentLast30d      int `json:"sent_last_30d"`
	DeliveredLast30d int `json:"delivered_last_30d"`
	FailedLast30d    int `json:"failed_last_30d"`
}

type statusResp struct {
	Status      string         `json:"status"` // not_connected | pending_verification | connected | error
	PhoneNumber *string        `json:"phone_number"`
	ConnectedAt *time.Time     `json:"connected_at"`
	Provider    *string        `json:"provider"`
	Stats       *whatsappStats `json:"stats"`
	LastError   *string        `json:"last_error"`
}

// templateResp matches the FE's NotificationTemplate interface
// (cuadra-desktop/src/hooks/useNotifications.ts). `is_default` is the inverse
// of internal `Custom`: FE renders a "Personalizada" badge cuando la plantilla
// tiene override, y un botón "Restaurar texto por defecto" que sólo aparece
// si !is_default. Mantenemos `default_body` aparte para previsualizar el
// original y permitir el restore sin re-pegarle al endpoint.
type templateResp struct {
	Key         string   `json:"key"`
	Channel     string   `json:"channel"`
	Category    string   `json:"category"`
	Variables   []string `json:"variables"`
	DefaultBody string   `json:"default_body"`
	Body        string   `json:"body"`
	Enabled     bool     `json:"enabled"`
	IsDefault   bool     `json:"is_default"`
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
	resp := statusResp{
		Status:      "not_connected",
		PhoneNumber: out.Phone,
		ConnectedAt: out.ConnectedAt,
		// Stats salen del agregado de notification_queue (sincronizada al
		// sidecar). Damos el shape aún cuando el gym no tiene WhatsApp
		// conectado — los contadores en cero son data útil ("nunca enviaste
		// nada por aquí").
		Stats: &whatsappStats{
			SentLast30d:      out.StatsSent,
			DeliveredLast30d: out.StatsDelivd,
			FailedLast30d:    out.StatsFailed,
		},
	}
	if out.Connected {
		resp.Status = "connected"
	}
	if out.LastError != "" {
		msg := out.LastError
		resp.LastError = &msg
	}
	utils.JsonResponse(c, http.StatusOK, resp)
}

// handleDisconnect — DELETE /api/v1/gyms/me/whatsapp (owner-only). Borra los
// campos WhatsApp del gym; el provider remoto (Twilio/Meta) NO recibe
// notificación todavía — mejor dejar el sender registrado que romper la API
// en producción cuando el dueño hizo click por error. El re-connect simplemente
// vuelve a llamar provider.RegisterSender, que es idempotente.
func (ctrl *Controller) handleDisconnect(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	if ctrl.Disconnect == nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, errBadAuth)
		return
	}
	if err := ctrl.Disconnect.Execute(c.Request.Context(), notiApp.DisconnectWhatsAppInput{
		GymID:       gymID,
		ActorUserID: userID,
	}); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"ok": true})
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
			Key:         r.Key,
			Channel:     r.Channel,
			Category:    r.Category,
			Variables:   r.Variables,
			DefaultBody: r.Default,
			Body:        r.Body,
			Enabled:     r.Enabled,
			IsDefault:   !r.Custom,
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

// ---------------------------------------------------------------------------
// UC-037 — 2-step WhatsApp connect (/start emits OTP, /connect verifies)
// ---------------------------------------------------------------------------

var (
	errInvalidOTP    = errors.New("código inválido o expirado")
	errOTPSendFailed = errors.New("no pudimos enviar el código por WhatsApp; intenta de nuevo")
)

func (ctrl *Controller) handleWhatsappStart(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	var req whatsappStartReq
	if !bindJSON(c, &req) {
		return
	}
	code, exp, err := ctrl.OTPStore.Issue(gymID, req.PhoneNumber)
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	// Deliver the OTP via WhatsApp. If delivery fails we surface a 502 so
	// the FE can prompt the operator to retry; the issued code stays in
	// the store, so a manual /start re-issues a fresh one (overwrites).
	if ctrl.WhatsApp != nil {
		if sendErr := ctrl.WhatsApp.SendOTP(c.Request.Context(), req.PhoneNumber, code); sendErr != nil {
			log.Printf("[notifications] whatsapp OTP send failed for gym=%s phone=%s: %v", gymID, req.PhoneNumber, sendErr)
			utils.ErrorResponse(c, http.StatusBadGateway, errOTPSendFailed)
			return
		}
	}
	// In dev (stdout/mock provider) we also echo the code back so the
	// operator can finish without an actual WhatsApp message.
	resp := whatsappStartResp{ExpiresAt: exp}
	if envIsDev() {
		resp.DevCode = code
	}
	utils.JsonResponse(c, http.StatusOK, resp)
}

// envIsDev returns true when we want to surface the OTP in /start responses.
// True when WHATSAPP_PROVIDER is unset/stdout/mock — i.e. the operator can't
// receive a real WhatsApp message and would otherwise be locked out.
func envIsDev() bool {
	v := strings.ToLower(os.Getenv("WHATSAPP_PROVIDER"))
	return v == "" || v == "stdout" || v == "mock"
}

// silence unused-import on errInvalidOTP until /connect wires it.
var _ = errInvalidOTP

// ---------------------------------------------------------------------------
// Owner-alerts (UC-040 DA-40.1)
// ---------------------------------------------------------------------------

// alertConfigWire mirrors the FE's AlertConfig (cuadra-desktop/.../useNotifications.ts).
// Each row carries both the toggle (enabled) and the mirror WhatsApp template
// (template_key + variables + default_body + body + is_default) so the FE
// renders toggle and editable copy in one place. GET and PATCH echo the
// same shape — no follow-up fetch needed after a save.
type alertConfigWire struct {
	Key         string   `json:"key"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description"`
	TemplateKey string   `json:"template_key"`
	Variables   []string `json:"variables"`
	DefaultBody string   `json:"default_body"`
	Body        string   `json:"body"`
	IsDefault   bool     `json:"is_default"`
}

type updateAlertReq struct {
	Enabled *bool   `json:"enabled,omitempty"`
	Body    *string `json:"body,omitempty"`
}

func toAlertWire(v notiApp.AlertConfigView) alertConfigWire {
	return alertConfigWire{
		Key:         string(v.Key),
		Enabled:     v.Enabled,
		Description: v.Description,
		TemplateKey: v.TemplateKey,
		Variables:   v.Variables,
		DefaultBody: v.DefaultBody,
		Body:        v.Body,
		IsDefault:   v.IsDefault,
	}
}

func (ctrl *Controller) handleListAlerts(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	rows, err := ctrl.ListOwnerAlerts.Execute(c.Request.Context(), gymID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]alertConfigWire, 0, len(rows))
	for _, r := range rows {
		items = append(items, toAlertWire(r))
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": items})
}

func (ctrl *Controller) handleUpdateAlert(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	key := alertDomain.Key(c.Param("key"))
	if alertDomain.LookupDefault(key) == nil {
		utils.ErrorResponse(c, http.StatusNotFound, errBadID)
		return
	}
	var req updateAlertReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	saved, err := ctrl.UpdateOwnerAlert.Execute(c.Request.Context(), notiApp.UpdateOwnerAlertInput{
		GymID:       gymID,
		ActorUserID: userID,
		Key:         key,
		Enabled:     req.Enabled,
		Body:        req.Body,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toAlertWire(*saved))
}
