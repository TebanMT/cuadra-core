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

// DefaultLibrary returns the default text used for freeform and fallback delivery.
// Provider-approved WhatsApp templates keep their own body and positional variables.
// DefaultLibrary also defines the stable variable contracts used at Tinta
// boot. Adding a new template here is a code change + (for WhatsApp) a
// re-approval flow with Twilio. Order is stable for tests and UI.
func DefaultLibrary() []Definition {
	return []Definition{
		{
			Key:       "expiry_reminder_3d",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "gym_name", "expiry_date"},
			Body:      "Hola {member_first_name}. Tu membresía en {gym_name} vence el {expiry_date}. Puedes renovar en recepción.",
		},
		{
			Key:       "expiry_reminder_today",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "gym_name"},
			Body:      "Hola {member_first_name}. Tu membresía en {gym_name} vence hoy. Puedes renovar en recepción.",
		},
		{
			Key:       "expiry_reminder_5d_post",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "gym_name"},
			Body:      "Hola {member_first_name}. Tu membresía en {gym_name} está vencida. Puedes renovar en recepción.",
		},
		{
			// El template aprobado en Twilio tiene 6 vars; la 6ª es la URL del
			// comprobante (PDF subido a R2 por el dispatcher). El ORDEN fija
			// {{1}}..{{6}} vía contentVariablesJSON — NO reordenar sin cambiar
			// el template aprobado, o Twilio rechaza (count mismatch).
			Key:       "receipt_membership",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "amount", "membership_type", "expiry_date", "gym_name", "receipt_url"},
			Body:      "Hola {member_first_name}. Recibimos tu pago de ${amount} por {membership_type} en {gym_name}. Vigencia hasta el {expiry_date}. Comprobante: {receipt_url}",
		},
		{
			// 4 vars; la 4ª es la URL del comprobante.
			Key:       "receipt_product",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"member_first_name", "amount", "gym_name", "receipt_url"},
			Body:      "Hola {member_first_name}. Registramos tu compra de ${amount} en {gym_name}. Comprobante: {receipt_url}",
		},
		{
			Key:       "owner_alert_low_stock",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "product_name", "stock"},
			Body:      "{gym_name}: quedan {stock} unidades de {product_name}. Revisa las existencias en Tinta.",
		},
		{
			Key:       "owner_alert_expired_batch",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "count"},
			Body:      "{gym_name}: hay {count} socios con membresía vencida sin contacto reciente. Revísalos en Socios.",
		},
		{
			Key:       "owner_alert_cash_close_diff",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "diff_amount"},
			Body:      "{gym_name}: el corte de caja tiene una diferencia de ${diff_amount}. Revísalo en Caja.",
		},
		{
			Key:       "owner_alert_vip_no_visit",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "member_name", "days_inactive"},
			Body:      "{gym_name}: {member_name}, socio VIP, lleva {days_inactive} días sin asistir.",
		},
		{
			Key:       "owner_alert_no_payments_today",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "date"},
			Body:      "{gym_name}: no se registraron cobros el {date}. Consulta con recepción.",
		},
		{
			Key:       "broadcast_freeform",
			Channel:   ChannelWhatsApp,
			Category:  CategoryMarketing,
			Variables: []string{"member_first_name", "gym_name", "message"},
			// Texto del template Marketing aprobado por Meta ({{1}}=member_first_name,
			// {{2}}=gym_name, {{3}}=message). El dueño sólo escribe {message}; el
			// saludo y el cierre son fijos. Mantener en sync con el Content SID.
			Body: "Hola {member_first_name}, tienes un mensaje de parte de {gym_name}: {message} Esperamos verte pronto en el gym.",
		},
		{
			// operator_welcome_pin: se envía al operador (recepcionista) al
			// alta y cuando el dueño regenera su PIN. Reemplaza al viejo
			// "operator_temp_password" — los operadores nuevos ya no llevan
			// password, sólo PIN de 4 dígitos para login en recepción.
			Key:      "operator_welcome_pin",
			Channel:  ChannelWhatsApp,
			Category: CategoryAuthentication,
			// ADR-009: template Twilio de tipo MEDIA. El PIN no puede ir como
			// texto (Meta no lo aprueba), así que va horneado en el banner que
			// el dispatcher genera. Variables = contrato POSICIONAL del media
			// template aprobado: {{1}}=URL imagen, {{2}}=nombre, {{3}}=gym.
			// El Body de abajo NO se manda por WhatsApp — es sólo fallback de
			// email/mock y se renderiza con el payload (que aún trae el PIN).
			Variables: []string{"welcome_image_url", "full_name", "gym_name"},
			Body:      "Hola {full_name}. Tu PIN para entrar a Tinta en {gym_name} es *{pin}*.",
		},
		{
			// member_welcome_number: se envía al socio al inscribirse y cuando
			// el operador (re)asigna su número. El NÚMERO DE SOCIO (ADR-010)
			// es público y se usa en el kiosko para registrar la entrada.
			// Categoría: utility (no marketing — es información transaccional
			// ligada a la inscripción que el socio acaba de hacer).
			// OJO: esta clave indexa el Content SID de Twilio en
			// TWILIO_CONTENT_SIDS — al renombrar (ADR-010 purga) hay que
			// actualizar esa env. operator_welcome_pin (PIN de login) NO cambia.
			Key:      "member_welcome_number",
			Channel:  ChannelWhatsApp,
			Category: CategoryUtility,
			// ADR-009: template Twilio de tipo MEDIA. El número va horneado en
			// el banner que genera el dispatcher (Meta no aprueba código en
			// texto). Variables = contrato POSICIONAL del media template
			// aprobado (HXd072…): {{1}}=nombre socio, {{2}}=gym, {{3}}=URL
			// imagen. El Body es sólo fallback email/mock.
			Variables: []string{"member_first_name", "gym_name", "welcome_image_url"},
			Body:      "Hola {member_first_name}. Tu número de socio en {gym_name} es *{member_number}*. Úsalo para registrar tu entrada.",
		},
		{
			// owner_welcome: se envía al DUEÑO cuando vincula el primer
			// dispositivo del gym (ADR-010 / onboarding) — el momento en que
			// el sistema queda "vivo". Lleva su código de acceso horneado en
			// el banner. Categoría UTILITY para Meta: es un media template
			// transaccional (confirmación post-acción), NO authentication —
			// esa categoría es sólo para OTP con botón copy-code y no admite
			// header de imagen.
			Key:      "owner_welcome",
			Channel:  ChannelWhatsApp,
			Category: CategoryUtility,
			// ADR-009: template Twilio de tipo MEDIA. El código va horneado en
			// el banner (KindOwner). Variables = contrato POSICIONAL del media
			// template: {{1}}=URL imagen, {{2}}=nombre, {{3}}=gym. El Body es
			// sólo fallback email/mock. La clave indexa el Content SID en
			// TWILIO_CONTENT_SIDS.
			Variables: []string{"welcome_image_url", "full_name", "gym_name"},
			Body:      "Hola {full_name}. Tinta está listo en {gym_name}. Tu PIN para entrar en recepción es *{pin}*.",
		},
		{
			// UC-037 connect-step OTP. Sent from Cuadra's master WhatsApp
			// business number to the gym owner's number; once verified,
			// the gym's own number takes over outbound traffic.
			Key:       "whatsapp_connect_otp",
			Channel:   ChannelWhatsApp,
			Category:  CategoryAuthentication,
			Variables: []string{"code"},
			Body:      "Tu código de verificación de Tinta es {code}. Vence en 10 minutos.",
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
			Body:      "{gym_name}: tu plan anual de Tinta vence el {expires_on}. Genera tu ficha para pagar en OXXO: {voucher_url}",
		},
		{
			Key:       "oxxo_renewal_reminder_14d",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "voucher_url", "expires_on"},
			Body:      "{gym_name}: tu plan anual de Tinta vence el {expires_on}. Genera tu ficha para pagar en OXXO: {voucher_url}",
		},
		{
			Key:       "oxxo_renewal_reminder_3d",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "voucher_url", "expires_on"},
			Body:      "{gym_name}: tu plan anual de Tinta vence el {expires_on}, en 3 días. Genera tu ficha para renovar en OXXO: {voucher_url}",
		},
		{
			Key:       "oxxo_renewal_reminder_today",
			Channel:   ChannelWhatsApp,
			Category:  CategoryUtility,
			Variables: []string{"gym_name", "voucher_url"},
			Body:      "{gym_name}: tu plan anual de Tinta vence hoy. Genera tu ficha para renovar en OXXO: {voucher_url}",
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
