// Package template owns the message template logic for the notifications BC.
//
// A Template is a (TemplateKey, Body) pair with a fixed set of typed
// variables. Defaults live in DefaultLibrary; per-gym overrides are stored in
// `notification_templates` (UC-038 DA-38.2 — owners can edit the copy but
// not the variable set, since Twilio/Meta's approval is bound to the
// placeholder structure).
package template

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
)

// Channel identifies which transport the template is approved for.
type Channel string

const (
	ChannelWhatsApp Channel = "whatsapp"
	ChannelEmail    Channel = "email"
)

// Category mirrors Meta's WhatsApp template category — utility / marketing /
// authentication. Used for cost reporting and consent gating; broadcast
// (marketing) requires opt-in and consent flag (ADR-007 §7).
type Category string

const (
	CategoryUtility        Category = "utility"
	CategoryMarketing      Category = "marketing"
	CategoryAuthentication Category = "authentication"
	CategoryService        Category = "service"
)

// Definition is the immutable metadata of a template. The Body is the *default*
// copy; per-gym overrides may replace it but not the Variables list. The
// system uses `{var_name}` placeholders (Cuadra-side rendering) — when the
// outbound provider needs Twilio's `{{1}} {{2}}` positional form the
// dispatcher substitutes locally with a fully-rendered string.
type Definition struct {
	Key       string
	Channel   Channel
	Category  Category
	Variables []string
	Body      string
	// Subject is only meaningful for email templates.
	Subject string
}

// MaxBodyLen mirrors WhatsApp's content limit with margin for variable
// expansion. Cuadra refuses anything longer (UC-038 update_template).
const MaxBodyLen = 1024

// DefaultLibrary returns the read-only set of templates approved at Cuadra
// boot. Adding a new template here is a code change + (for WhatsApp) a
// re-approval flow with Twilio. Order is stable for tests and UI.
func DefaultLibrary() []Definition {
	return []Definition{
		{
			Key:       "expiry_reminder_3d",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "gym_name", "expiry_date"},
			Body:      "Hola {member_first_name} 👋 Soy {gym_name}. Te aviso que tu mensualidad vence el {expiry_date}. ¿Te esperamos para renovar?",
		},
		{
			Key:       "expiry_reminder_today",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "gym_name"},
			Body:      "Hola {member_first_name}, hoy vence tu mensualidad en {gym_name}. ¡Pásate cuando puedas!",
		},
		{
			Key:       "expiry_reminder_5d_post",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "gym_name"},
			Body:      "Hola {member_first_name}, te extrañamos en {gym_name}. Te dejamos esta nota por si quieres regresar.",
		},
		{
			Key:       "receipt_membership",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "amount", "membership_type", "expiry_date", "gym_name"},
			Body:      "¡Listo, {member_first_name}! Recibimos tu pago de ${amount} por {membership_type}. Tu nueva vigencia es hasta el {expiry_date}. Gracias por entrenar con nosotros — {gym_name}.",
		},
		{
			Key:       "receipt_product",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "amount", "gym_name"},
			Body:      "¡Listo, {member_first_name}! Tu compra de ${amount} en {gym_name} fue registrada.",
		},
		{
			Key:       "owner_alert_low_stock",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "product_name", "stock"},
			Body:      "Alerta de {gym_name}: stock bajo de {product_name} ({stock} unidades).",
		},
		{
			Key:       "owner_alert_expired_batch",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "count"},
			Body:      "Alerta de {gym_name}: {count} socios vencidos sin contacto. Persíguelos en Cuadra.",
		},
		{
			Key:       "owner_alert_cash_close_diff",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "diff_amount"},
			Body:      "Alerta de {gym_name}: cierre de caja con diferencia de ${diff_amount}. Revisa en Cuadra.",
		},
		{
			Key:       "owner_alert_vip_no_visit",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "member_name", "days_inactive"},
			Body:      "Alerta de {gym_name}: {member_name} (socio VIP) no viene desde hace {days_inactive} días.",
		},
		{
			Key:       "owner_alert_no_payments_today",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "date"},
			Body:      "Alerta de {gym_name}: no se registraron cobros el {date}. Revisa con tu operador.",
		},
		{
			Key:       "broadcast_freeform",
			Channel:   ChannelWhatsApp,
			Category:  CategoryMarketing,
			Variables: []string{"member_first_name", "gym_name", "message"},
			Body:      "Hola {member_first_name}, {message} — {gym_name}",
		},
		{
			// operator_welcome_pin: se envía al operador (recepcionista) al
			// alta y cuando el dueño regenera su PIN. Reemplaza al viejo
			// "operator_temp_password" — los operadores nuevos ya no llevan
			// password, sólo PIN de 4 dígitos para login en recepción.
			Key:       "operator_welcome_pin",
			Channel:   ChannelWhatsApp,
			Category:  CategoryAuthentication,
			Variables: []string{"full_name", "gym_name", "pin"},
			Body:      "Hola {full_name} 👋 {gym_name} te dio de alta en Tinta. Tu PIN de acceso es *{pin}*. Lo usas en el sistema del gym para iniciar sesión.",
		},
		{
			// member_welcome_pin: se envía al socio al inscribirse y cuando
			// el operador regenera su PIN. El PIN es de 4 dígitos y se usa
			// en el kiosko para registrar la entrada. Categoría: utility
			// (no marketing — es información transaccional ligada a la
			// inscripción que el socio acaba de hacer).
			Key:       "member_welcome_pin",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "gym_name", "pin"},
			Body:      "Hola {member_first_name} 👋 Soy {gym_name}. Tu PIN de acceso es *{pin}*. Lo usas en el kiosko del gym para registrar tu entrada. ¡Bienvenido!",
		},
		{
			// UC-037 connect-step OTP. Sent from Cuadra's master WhatsApp
			// business number to the gym owner's number; once verified,
			// the gym's own number takes over outbound traffic.
			Key:       "whatsapp_connect_otp",
			Channel:   ChannelWhatsApp,
			Category:  CategoryAuthentication,
			Variables: []string{"code"},
			Body:      "Tu código de verificación de Cuadra es {code}. Vence en 10 minutos.",
		},
		// ── Renovación OXXO anual ────────────────────────────────────────
		// Cuatro variantes (30/14/3/día-0) porque Meta aprueba el cuerpo
		// completo del template, no admite variar "urgencia" por variable.
		// El cron de subscriptions/app/oxxo_renewal_reminder.go elige una
		// según los días restantes a SubscriptionEndsAt. Variables
		// compartidas: gym_name, voucher_url (link al Checkout Session que
		// genera la ficha nueva), expires_on (fecha legible de vencimiento).
		{
			Key:       "oxxo_renewal_reminder_30d",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "voucher_url", "expires_on"},
			Body:      "Hola {gym_name} 👋 Tu plan anual de Tinta vence el {expires_on}. Te dejamos tu link para pagar la próxima ficha en OXXO cuando puedas: {voucher_url}",
		},
		{
			Key:       "oxxo_renewal_reminder_14d",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "voucher_url", "expires_on"},
			Body:      "Hola {gym_name}, recordatorio: tu plan anual de Tinta vence el {expires_on}. Aquí tu ficha para pagar en OXXO: {voucher_url}",
		},
		{
			Key:       "oxxo_renewal_reminder_3d",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "voucher_url", "expires_on"},
			Body:      "Hola {gym_name}, tu plan vence el {expires_on} (en 3 días). Paga tu ficha en OXXO para no interrumpir el servicio: {voucher_url}",
		},
		{
			Key:       "oxxo_renewal_reminder_today",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "voucher_url", "expires_on"},
			Body:      "Hola {gym_name}, tu plan vence HOY. Paga tu ficha en OXXO cuanto antes para no perder el servicio: {voucher_url}",
		},
	}
}

// WhatsAppConnectOTPKey is the canonical template key used for the UC-037
// connect-step verification code. Exposed as a constant so providers and
// the controller agree on the same wire identifier.
const WhatsAppConnectOTPKey = "whatsapp_connect_otp"

// LookupDefault returns the default Definition for a key, or nil if unknown.
func LookupDefault(key string) *Definition {
	for _, d := range DefaultLibrary() {
		if d.Key == key {
			d := d
			return &d
		}
	}
	return nil
}

// Render performs `{var}` substitution and returns the final text. Missing
// variables yield ErrTemplateMissingVar so the caller doesn't ship a
// half-resolved message.
func Render(body string, vars map[string]string) (string, error) {
	out := body
	required := extractPlaceholders(body)
	sort.Strings(required)
	for _, v := range required {
		val, ok := vars[v]
		if !ok {
			return "", fmt.Errorf("%w: faltó {%s}", notiErrors.ErrTemplateMissingVar, v)
		}
		out = strings.ReplaceAll(out, "{"+v+"}", val)
	}
	return out, nil
}

// extractPlaceholders pulls every `{name}` token from the body. We do a
// single-pass scan to avoid pulling in regexp at hot path; templates are
// short enough that O(n) is fine.
func extractPlaceholders(body string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for i := 0; i < len(body); i++ {
		if body[i] != '{' {
			continue
		}
		end := strings.IndexByte(body[i+1:], '}')
		if end < 0 {
			break
		}
		name := body[i+1 : i+1+end]
		if name == "" || strings.ContainsAny(name, "{ \t\n") {
			continue
		}
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			out = append(out, name)
		}
		i += end
	}
	return out
}

// Validate checks an editable body against the original Definition: must
// reference every required variable at least once and respect the size cap.
// (ADR-007 §4.3 — variables are FIXED; copy is editable.)
func (d *Definition) Validate(body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return notiErrors.ErrTemplateBodyEmpty
	}
	if len(body) > MaxBodyLen {
		return notiErrors.ErrTemplateBodyTooLong
	}
	placeholders := extractPlaceholders(body)
	have := map[string]struct{}{}
	for _, p := range placeholders {
		have[p] = struct{}{}
	}
	for _, v := range d.Variables {
		if _, ok := have[v]; !ok {
			return fmt.Errorf("%w: falta {%s}", notiErrors.ErrTemplateMissingVar, v)
		}
	}
	return nil
}

// Override is the per-gym row in `notification_templates` (UC-038 DA-38.2).
type Override struct {
	ID          uuid.UUID
	GymID       uuid.UUID
	Version     int
	TemplateKey string
	Body        string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// NewOverride is the factory for owner-edited copy. `body` is validated
// against the Definition variables.
func NewOverride(id, gymID uuid.UUID, key, body string, now time.Time) (*Override, error) {
	def := LookupDefault(key)
	if def == nil {
		return nil, notiErrors.ErrTemplateNotFound
	}
	if err := def.Validate(body); err != nil {
		return nil, err
	}
	return &Override{
		ID:          id,
		GymID:       gymID,
		Version:     1,
		TemplateKey: key,
		Body:        strings.TrimSpace(body),
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// Edit replaces Body after validating against the Definition.
func (o *Override) Edit(body string, now time.Time) error {
	def := LookupDefault(o.TemplateKey)
	if def == nil {
		return notiErrors.ErrTemplateNotFound
	}
	if err := def.Validate(body); err != nil {
		return err
	}
	o.Body = strings.TrimSpace(body)
	o.Version++
	o.UpdatedAt = now
	return nil
}

// SetEnabled toggles the override (UC-040 DA-40.1 — owner can disable
// individual alerts without losing custom copy).
func (o *Override) SetEnabled(enabled bool, now time.Time) {
	if o.Enabled == enabled {
		return
	}
	o.Enabled = enabled
	o.Version++
	o.UpdatedAt = now
}
