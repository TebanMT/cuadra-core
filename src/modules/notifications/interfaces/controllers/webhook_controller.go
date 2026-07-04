//go:build server

package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/twilio/twilio-go/client"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	eventDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/event"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// WebhookProcessor is the use-case surface the webhook controller depends
// on. *notiApp.ProcessWebhook satisfies it; tests can pass a fake.
type WebhookProcessor interface {
	Execute(ctx context.Context, in notiApp.ProcessWebhookInput) (*notiApp.ProcessWebhookOutput, error)
}

// WebhookController exposes /api/v1/webhooks/twilio. Validates Twilio's
// X-Twilio-Signature header before parsing the payload (ADR-007 §5).
type WebhookController struct {
	Process    WebhookProcessor
	Validator  client.RequestValidator
	WebhookURL string // public URL Twilio posts to — included in the signature base
}

// NewWebhookController wires the controller. `authToken` is the Twilio auth
// token used to compute signatures; `webhookURL` is the absolute URL Twilio
// is configured with (must match what Twilio uses to compute the sig).
func NewWebhookController(process WebhookProcessor, authToken, webhookURL string) *WebhookController {
	return &WebhookController{
		Process:    process,
		Validator:  client.NewRequestValidator(authToken),
		WebhookURL: webhookURL,
	}
}

func (ctrl *WebhookController) RegisterRoutes(r *gin.Engine) {
	r.POST("/api/v1/webhooks/twilio", ctrl.handleStatusCallback)
}

// handleStatusCallback consumes Twilio's StatusCallback POSTs. Body is
// application/x-www-form-urlencoded.
func (ctrl *WebhookController) handleStatusCallback(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, notiErrors.ErrInvalidPayload)
		return
	}

	signature := c.GetHeader("X-Twilio-Signature")
	if !ctrl.Validator.ValidateBody(ctrl.WebhookURL, body, signature) {
		utils.ErrorResponse(c, http.StatusUnauthorized, notiErrors.ErrInvalidSignature)
		return
	}

	parsed, err := url.ParseQuery(string(body))
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, notiErrors.ErrInvalidPayload)
		return
	}

	// RawPayload va como el form YA parseado y marshaleado a JSON — NUNCA
	// el body crudo: Twilio manda x-www-form-urlencoded y la columna
	// whatsapp_events.raw_payload es JSONB (postgres) / CHECK json_valid
	// (sqlite). Guardar el body crudo rollbackeaba el INSERT con 22P02,
	// el webhook respondía 500 y Twilio reintentaba en loop (bug jul-2026).
	raw, err := formToJSON(parsed)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, notiErrors.ErrInvalidPayload)
		return
	}

	in := notiApp.ProcessWebhookInput{
		EventType:         eventTypeFromForm(parsed),
		ProviderMessageID: parsed.Get("MessageSid"),
		Status:            parsed.Get("MessageStatus"),
		ErrorCode:         parsed.Get("ErrorCode"),
		ErrorMessage:      parsed.Get("ErrorMessage"),
		// Inbound: From viene como "whatsapp:+52..."; Body es el texto del
		// socio. El use case detecta STOP/BAJA para opt-out de marketing.
		FromPhone:  parsed.Get("From"),
		Body:       parsed.Get("Body"),
		RawPayload: raw,
	}
	if _, err := ctrl.Process.Execute(c.Request.Context(), in); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(http.StatusOK)
}

// formToJSON serializa el form de Twilio como objeto JSON plano, preservando
// el callback completo para debug/cumplimiento: {"MessageStatus":"sent",...}.
// Twilio no repite llaves en la práctica; si alguna llegara repetida se
// preserva como array para no perder datos.
func formToJSON(form url.Values) ([]byte, error) {
	obj := make(map[string]any, len(form))
	for k, vs := range form {
		if len(vs) == 1 {
			obj[k] = vs[0]
		} else {
			obj[k] = vs
		}
	}
	return json.Marshal(obj)
}

// eventTypeFromForm decides whether this is an inbound message or a status
// callback. Twilio includes `From` + `Body` for incoming messages and
// `MessageStatus` for status updates — the two are mutually exclusive in
// practice.
func eventTypeFromForm(form url.Values) string {
	if form.Get("MessageStatus") != "" {
		return eventDomain.EventTypeStatus
	}
	if form.Get("Body") != "" || form.Get("From") != "" {
		return eventDomain.EventTypeIncoming
	}
	return eventDomain.EventTypeStatus
}
