//go:build server

// Package whatsapp houses the WhatsAppProvider implementations. The Twilio
// impl is `server`-only — the sidecar never sends; it only enqueues and
// relies on cloud to drain. ADR-007 §4.1 has the full reasoning.
package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/twilio/twilio-go"
	twilioApi "github.com/twilio/twilio-go/rest/api/v2010"

	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
)

// TwilioProvider implements notifications.WhatsAppProvider over the Twilio
// REST API. Auth credentials come from env vars (`TWILIO_ACCOUNT_SID`,
// `TWILIO_AUTH_TOKEN`); the per-gym `from_number` is supplied by the gym
// config (UC-037 stores it in `gyms.whatsapp_business_phone`).
type TwilioProvider struct {
	client            *twilio.RestClient
	statusCallbackURL string
	templateContent   map[string]string // template_key -> Twilio Content SID (HX...)
	otpFrom           string            // Cuadra master sender for UC-037 OTPs (E.164)
}

// TwilioOptions wires the provider. AccountSID / AuthToken are normally
// loaded from env in main.go; StatusCallbackURL is the public URL Twilio
// posts back to (e.g. https://api.entinta.app/api/v1/webhooks/twilio).
//
// TemplateContentSIDs maps Cuadra template keys (`expiry_reminder_3d`, etc)
// to the matching Twilio Content SID approved in the dashboard. Only keys
// present in the map are sendable through Twilio — others get rendered as
// freeform (which fails outside the 24h window, per ADR-007 §4.5).
type TwilioOptions struct {
	AccountSID          string
	AuthToken           string
	StatusCallbackURL   string
	TemplateContentSIDs map[string]string
	// OTPFromNumber is Cuadra's master WhatsApp business number used to
	// deliver UC-037 connect-step OTPs. It's distinct from the per-gym
	// `cfg.Phone` because at OTP time the gym hasn't connected yet. E.164.
	OTPFromNumber string
}

// NewTwilioProvider returns a configured provider. Returns nil-config error
// rather than ProviderUnavailable — the wiring layer should fail fast at
// boot if creds are missing.
func NewTwilioProvider(opts TwilioOptions) (*TwilioProvider, error) {
	if strings.TrimSpace(opts.AccountSID) == "" || strings.TrimSpace(opts.AuthToken) == "" {
		return nil, errors.New("twilio: account sid or auth token missing")
	}
	cli := twilio.NewRestClientWithParams(twilio.ClientParams{
		Username: opts.AccountSID,
		Password: opts.AuthToken,
	})
	if opts.TemplateContentSIDs == nil {
		opts.TemplateContentSIDs = map[string]string{}
	}
	return &TwilioProvider{
		client:            cli,
		statusCallbackURL: opts.StatusCallbackURL,
		templateContent:   opts.TemplateContentSIDs,
		otpFrom:           strings.TrimSpace(opts.OTPFromNumber),
	}, nil
}

// SendTemplate sends a pre-approved WhatsApp template message. When the
// template_key has no Content SID configured we degrade to a Body render
// (same outcome — Twilio sandbox numbers tolerate this; production numbers
// will fail with a useful error code).
func (p *TwilioProvider) SendTemplate(
	ctx context.Context,
	cfg notiDomain.GymWhatsAppConfig,
	recipient, templateKey string,
	vars map[string]string,
) (string, error) {
	from, err := whatsappAddress(cfg.Phone)
	if err != nil {
		return "", err
	}
	to, err := whatsappAddress(recipient)
	if err != nil {
		return "", err
	}

	params := &twilioApi.CreateMessageParams{}
	params.SetFrom(from)
	params.SetTo(to)
	if p.statusCallbackURL != "" {
		params.SetStatusCallback(p.statusCallbackURL)
	}

	if contentSID, ok := p.templateContent[templateKey]; ok && contentSID != "" {
		params.SetContentSid(contentSID)
		if len(vars) > 0 {
			cv := contentVariablesJSON(templateKey, vars)
			if cv != "" {
				params.SetContentVariables(cv)
			}
		}
	} else {
		body, err := renderForFreeform(templateKey, vars)
		if err != nil {
			return "", err
		}
		params.SetBody(body)
	}

	resp, err := p.client.Api.CreateMessage(params)
	if err != nil {
		return "", fmt.Errorf("%w: %v", notiErrors.ErrSendFailedRetry, err)
	}
	if resp == nil || resp.Sid == nil || *resp.Sid == "" {
		return "", notiErrors.ErrSendFailedRetry
	}
	return *resp.Sid, nil
}

// SendFreeform sends an arbitrary text message. Fails outside the 24h
// service window (ADR-007 §4.5) with Twilio error 63018 — Cuadra surfaces
// it as ErrSendFailedFinal upstream.
func (p *TwilioProvider) SendFreeform(
	ctx context.Context,
	cfg notiDomain.GymWhatsAppConfig,
	recipient, text string,
) (string, error) {
	from, err := whatsappAddress(cfg.Phone)
	if err != nil {
		return "", err
	}
	to, err := whatsappAddress(recipient)
	if err != nil {
		return "", err
	}
	params := &twilioApi.CreateMessageParams{}
	params.SetFrom(from)
	params.SetTo(to)
	params.SetBody(strings.TrimSpace(text))
	if p.statusCallbackURL != "" {
		params.SetStatusCallback(p.statusCallbackURL)
	}
	resp, err := p.client.Api.CreateMessage(params)
	if err != nil {
		return "", fmt.Errorf("%w: %v", notiErrors.ErrSendFailedRetry, err)
	}
	if resp == nil || resp.Sid == nil || *resp.Sid == "" {
		return "", notiErrors.ErrSendFailedRetry
	}
	return *resp.Sid, nil
}

// SendOTP delivers the UC-037 connect-step verification code over WhatsApp.
// Uses Cuadra's master sender (`OTPFromNumber`). The content must be an
// approved authentication template — Meta forbids freeform OTP delivery.
// The template key is registered as `whatsapp_connect_otp` and its Twilio
// Content SID lives in TemplateContentSIDs. Falls back to a freeform body
// when the SID isn't configured (sandbox / dev) so the operator on a
// sandbox number can still complete the flow.
func (p *TwilioProvider) SendOTP(ctx context.Context, phone, code string) error {
	from, err := whatsappAddress(p.otpFrom)
	if err != nil {
		return err
	}
	to, err := whatsappAddress(phone)
	if err != nil {
		return err
	}
	params := &twilioApi.CreateMessageParams{}
	params.SetFrom(from)
	params.SetTo(to)
	if p.statusCallbackURL != "" {
		params.SetStatusCallback(p.statusCallbackURL)
	}
	if contentSID, ok := p.templateContent[tplDomain.WhatsAppConnectOTPKey]; ok && contentSID != "" {
		params.SetContentSid(contentSID)
		params.SetContentVariables(fmt.Sprintf(`{"1":%q}`, code))
	} else {
		body, rerr := tplDomain.Render(otpFreeformBody, map[string]string{"code": code})
		if rerr != nil {
			return rerr
		}
		params.SetBody(body)
	}
	resp, err := p.client.Api.CreateMessage(params)
	if err != nil {
		return fmt.Errorf("%w: %v", notiErrors.ErrSendFailedRetry, err)
	}
	if resp == nil || resp.Sid == nil || *resp.Sid == "" {
		return notiErrors.ErrSendFailedRetry
	}
	return nil
}

// otpFreeformBody is used only when the approved template SID isn't
// configured (dev sandbox). It mirrors the template copy.
const otpFreeformBody = "Tu código de verificación de Cuadra es {code}. Vence en 10 minutos."

// RegisterSender stubs the UC-037 onboarding flow. Real Twilio sender
// registration requires a multi-step "WhatsApp Sender" provisioning that
// involves Meta verification — done from the Twilio Console in MVP. The
// API path here returns an opaque marker the controller treats as "started"
// so the UI can advance; production wires the WhatsApp Senders API once
// approved.
func (p *TwilioProvider) RegisterSender(ctx context.Context, phone string) (string, error) {
	if _, err := whatsappAddress(phone); err != nil {
		return "", err
	}
	// Sentinel marker — UC-037 controller surfaces a "pending verification"
	// status. The real sender SID is filled in by the owner after Meta
	// verification.
	return "PENDING_SENDER_REGISTRATION", nil
}

func whatsappAddress(phone string) (string, error) {
	p := strings.TrimSpace(phone)
	if p == "" {
		return "", notiErrors.ErrInvalidPhone
	}
	if strings.HasPrefix(p, "whatsapp:") {
		return p, nil
	}
	if !strings.HasPrefix(p, "+") {
		p = "+" + p
	}
	return "whatsapp:" + p, nil
}

// contentVariablesJSON converts Cuadra's named variables (e.g.
// `member_first_name`) to Twilio's positional variables (`{"1": ..., "2": ...}`)
// using the fixed order declared in template.DefaultLibrary. Unknown
// templates fall through to an empty payload — Twilio will use the
// template's default values.
func contentVariablesJSON(templateKey string, vars map[string]string) string {
	def := tplDomain.LookupDefault(templateKey)
	if def == nil {
		return ""
	}
	parts := make([]string, 0, len(def.Variables))
	for i, name := range def.Variables {
		v, ok := vars[name]
		if !ok {
			v = ""
		}
		// Twilio expects a JSON object keyed by string position ("1","2",...).
		parts = append(parts, fmt.Sprintf("%q:%q", fmt.Sprint(i+1), v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func renderForFreeform(templateKey string, vars map[string]string) (string, error) {
	def := tplDomain.LookupDefault(templateKey)
	if def == nil {
		return "", notiErrors.ErrTemplateNotFound
	}
	return tplDomain.Render(def.Body, vars)
}
