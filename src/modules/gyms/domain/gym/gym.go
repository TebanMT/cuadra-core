// Package gym holds the Gym aggregate. The entity has no GORM tags — those
// live in infraestructure/db/models. Mutators here enforce invariants; raw
// field access is fine for setters that don't need invariants (e.g. logo URL).
package gym

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	gymErrors "github.com/cuadra/cuadra-core/src/modules/gyms/domain/errors"
	phonepkg "github.com/cuadra/cuadra-core/src/shared/phone"
	"github.com/cuadra/cuadra-core/src/shared/runtime"
)

// SubscriptionPlan and SubscriptionStatus mirror the chk_ constraints in
// db_migrations/postgres/001_init_schema.sql.
//
// Plan naming convention: <tier>_<cadence>. Two tiers live today (Standard,
// Plus) across two cadences (monthly, annual). The annual SKUs are accepted
// by the domain so checkout/migrations can land them ahead of the gateway
// pricing wiring.
const (
	PlanTrial           = "trial"
	PlanStandardMonthly = "standard_monthly"
	PlanStandardAnnual  = "standard_annual"
	PlanPlusMonthly     = "plus_monthly"
	PlanPlusAnnual      = "plus_annual"

	StatusActive    = "active"
	StatusPastDue   = "past_due"
	StatusCancelled = "cancelled"

	PaymentCash     = "cash"
	PaymentTransfer = "transfer"
	PaymentCard     = "card"
)

// IsPlusPlan reports whether `plan` is one of the Plus-tier SKUs. Used por
// la lógica de gating que expone features exclusivas de Plus. Trial NO
// cuenta como Plus para el cálculo del SKU comprado (facturación,
// reporting interno).
func IsPlusPlan(plan string) bool {
	return plan == PlanPlusMonthly || plan == PlanPlusAnnual
}

// CanAccessPlusFeatures decide si un gym debe ver las features Plus.
//
// HOY (Plus aún no se vende — landing dice "Próximamente"): trial NO pasa.
// Mientras el bundle Plus no esté completo (faltan rutinas + app del socio
// según el roadmap), enseñarle al dueño en trial features que no se venden
// es prometer algo que no existe. Trial opera como Standard hasta que se
// libere Plus.
//
// CUANDO PLUS SE LIBERE (rutinas + app del socio + tap-to-sell + broadcast
// completo en prod), añadir `plan == PlanTrial` aquí. El trial debe poder
// probar Plus para evaluar el upgrade — ese era el plan original del ADR-009.
// Buscar este comentario al hacer el cambio.
//
// Usa IsPlusPlan SOLO cuando el punto de la decisión es el SKU comprado
// (facturación, reporting interno) — esa función nunca incluyó trial.
//
// DEV-MODE BYPASS: cuando TINTA_MODE=dev, este helper devuelve true para
// cualquier plan, desbloqueando todas las features Plus localmente sin
// tocar la BD. La dep del domain layer sobre shared/runtime es deliberada
// y excepcional: runtime es un paquete sin estado, sólo lee env vars
// (equivalente a llamar os.Getenv inline), y centralizar el switch acá
// hace cascade automático a todos los call-sites (sidebar, middleware,
// inline gates, deep-link locks) sin duplicar la condición. IsPlusPlan
// e IsPaidPlan NO se tocan — el SKU real sigue siendo honesto.
func CanAccessPlusFeatures(plan string) bool {
	if runtime.IsDev() {
		return true
	}
	return plan == PlanPlusMonthly || plan == PlanPlusAnnual
}

// IsPaidPlan reports whether the gym is on any paid SKU (Standard or Plus,
// monthly or annual). Trial returns false.
func IsPaidPlan(plan string) bool {
	switch plan {
	case PlanStandardMonthly, PlanStandardAnnual, PlanPlusMonthly, PlanPlusAnnual:
		return true
	}
	return false
}

// Gym is the multi-tenant root. Every other entity in the system carries the
// gym's id; for the gym itself the gym_id column equals id (ADR-002 §3.1).
type Gym struct {
	ID                 uuid.UUID
	Version            int
	Name               *string
	City               *string
	WhatsApp           *string
	Country            string
	Timezone           string
	RFC                *string
	RazonSocial        *string
	CodigoPostal       *string
	RegimenFiscal      *string
	LogoURL            *string
	PrimaryColor       *string
	SecondaryColor     *string
	PaymentMethods     []string
	OpenTime           *string // "HH:MM:SS"
	CloseTime          *string
	SubscriptionPlan   string
	TrialEndsAt        *time.Time
	SubscriptionEndsAt *time.Time
	SubscriptionStatus string
	// StripeCustomerID — capturado del primer webhook que trae customer
	// (checkout.session.completed o customer.subscription.created). Lo
	// usamos para abrir Stripe Billing Portal sin volver a pedir al usuario
	// que ingrese su tarjeta (UC: ver facturas / actualizar método de pago).
	// Nullable: gyms en trial sin checkout aún, o gyms cobrados sólo por MP.
	StripeCustomerID         *string
	SetupCompletedAt         *time.Time
	WhatsAppBusinessPhone    *string
	WhatsAppBusinessTokenEnc []byte
	WhatsAppConnectedAt      *time.Time
	KioskSettings            map[string]any
	// ChargeSettings — config a nivel gym de cuotas extra que se le
	// cobran al socio (inscripción y mantenimiento). Persistido como
	// JSONB para no tener que migrar el schema cada vez que cambie la
	// forma. Llaves usadas hoy:
	//   charges_enrollment      (bool)
	//   charges_maintenance     (bool)
	//   enrollment_amount       (number, MXN)
	//   maintenance_amount      (number, MXN)
	//   maintenance_frequency   (string: monthly|bimonthly|quarterly|semiannual|annual)
	// Reemplaza el storage previo en localStorage del desktop — volátil
	// y per-device, inconsistente cuando un gym tiene varios equipos.
	ChargeSettings map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time
}

// NewTrialGym creates a placeholder gym at signup (UC-001 step 1). Most fields
// are filled in later steps via UpdateBasicInfo / UpdatePaymentMethods.
func NewTrialGym(id uuid.UUID, trialDays int, now time.Time) *Gym {
	trialEnds := now.Add(time.Duration(trialDays) * 24 * time.Hour)
	return &Gym{
		ID:                 id,
		Version:            1,
		Country:            "MX",
		Timezone:           "America/Mexico_City",
		PaymentMethods:     []string{},
		SubscriptionPlan:   PlanTrial,
		SubscriptionStatus: StatusActive,
		TrialEndsAt:        &trialEnds,
		KioskSettings: map[string]any{
			"audio_volume":       80,
			"auto_close_seconds": 5,
		},
		ChargeSettings: map[string]any{},
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// IsSetupComplete reports whether the gym wizard finished (UC-001 step 5).
func (g *Gym) IsSetupComplete() bool { return g.SetupCompletedAt != nil }

// ---------------------------------------------------------------------------
// Subscription lifecycle (Fase 1, charging the gym owner).
// ---------------------------------------------------------------------------
// Transitions are deliberately permissive: the source of truth is the payment
// processor (Stripe / Mercado Pago). The cloud accepts any state change those
// processors push and reflects it. The only thing this layer enforces is the
// enum + the version bump.

// ActivateSubscription marks the gym as paying on `plan`, with the next billing
// cycle ending at endsAt (nil = indefinite, e.g. annual rolling plans). Called
// by the subscriptions use case on `subscription.created` and on
// `invoice.paid` events.
func (g *Gym) ActivateSubscription(plan string, endsAt *time.Time, now time.Time) error {
	if !IsPaidPlan(plan) {
		return gymErrors.ErrSetupIncomplete
	}
	g.SubscriptionPlan = plan
	g.SubscriptionStatus = StatusActive
	g.SubscriptionEndsAt = endsAt
	// Trial implicitly ends the moment a real plan kicks in. We don't clear
	// `TrialEndsAt` so the FE can keep showing "tu trial era de X días" copy.
	g.Version++
	g.UpdatedAt = now
	return nil
}

// MarkPastDue is called when the processor reports a failed charge or expired
// card. UI surfaces a non-blocking warning; access is NOT cut here — the
// founder is in barrio gyms where killing the app the day a card expires is
// the wrong product call. Repeated past_due → eventual Cancel.
func (g *Gym) MarkPastDue(now time.Time) {
	g.SubscriptionStatus = StatusPastDue
	g.Version++
	g.UpdatedAt = now
}

// Cancel marks the subscription cancelled. SubscriptionEndsAt acts as a
// grace-period signal: the FE keeps unlocking features until that moment.
func (g *Gym) Cancel(now time.Time) {
	g.SubscriptionStatus = StatusCancelled
	g.Version++
	g.UpdatedAt = now
}

// ExtendTrial pushes TrialEndsAt forward by `days` (admin / sales tool, not a
// processor event). Idempotent on already-extended trials.
func (g *Gym) ExtendTrial(days int, now time.Time) {
	if days <= 0 {
		return
	}
	base := now
	if g.TrialEndsAt != nil && g.TrialEndsAt.After(now) {
		base = *g.TrialEndsAt
	}
	t := base.Add(time.Duration(days) * 24 * time.Hour)
	g.TrialEndsAt = &t
	if g.SubscriptionPlan == PlanTrial {
		g.SubscriptionStatus = StatusActive
	}
	g.Version++
	g.UpdatedAt = now
}

// IsTrialExpired reports whether the gym is in trial AND the trial window
// has already lapsed at `now`. Returns false for paid plans.
func (g *Gym) IsTrialExpired(now time.Time) bool {
	if g.SubscriptionPlan != PlanTrial {
		return false
	}
	if g.TrialEndsAt == nil {
		return false
	}
	return g.TrialEndsAt.Before(now)
}

// HasActiveAccess returns whether the gym should currently be allowed to
// operate (sync, billing, kiosk). The rule:
//   - active + trial not expired      → allow
//   - active + paid plan              → allow
//   - past_due                        → allow (warning only — see MarkPastDue)
//   - cancelled + within grace window → allow
//   - everything else                 → deny
//
// Lo usa el cloud para decidir si sigue aceptando sync writes de un gym
// sin pagar. Para el bloqueo en el SIDECAR usamos IsAccessHardBlocked, que
// es más permisivo: respeta la decisión de producto "offline tolera, sync
// exitoso confirma cancelación".
func (g *Gym) HasActiveAccess(now time.Time) bool {
	switch g.SubscriptionStatus {
	case StatusActive:
		if g.SubscriptionPlan == PlanTrial {
			return !g.IsTrialExpired(now)
		}
		return true
	case StatusPastDue:
		return true
	case StatusCancelled:
		if g.SubscriptionEndsAt != nil && g.SubscriptionEndsAt.After(now) {
			return true
		}
		return false
	default:
		return false
	}
}

// IsAccessHardBlocked es el bloqueo de operación que aplica el sidecar.
// A diferencia de HasActiveAccess, NO bloquea por trial vencido localmente —
// el dueño puede seguir operando offline aunque el reloj local pase el
// trial_ends_at. Sólo se bloquea cuando el cloud ya marcó la suscripción
// como cancelled (vía sync) Y la grace period venció.
//
// Esta asimetría es deliberada por producto:
//   - Sin internet, Tinta debe seguir funcionando — es la promesa offline-first.
//   - Con internet + sync exitoso, el cloud es la fuente de verdad de billing
//     y el sidecar respeta lo que diga.
//
// El cron del cloud es quien decide CUÁNDO marcar cancelled (típicamente al
// final del trial sin upgrade, o tras N intentos fallidos de cobro). El
// sidecar no se mete en esa decisión — sólo aplica el resultado.
func (g *Gym) IsAccessHardBlocked(now time.Time) bool {
	if g.SubscriptionStatus != StatusCancelled {
		return false
	}
	// Grace period activa = sigue operando hasta que venza.
	if g.SubscriptionEndsAt != nil && g.SubscriptionEndsAt.After(now) {
		return false
	}
	return true
}

// SetupStep figures out which wizard step the user lands on if they bail
// mid-flow (UC-001 alt: wizard interrupted). The order matches USE-CASES:
//
//	step 2: gym name
//	step 3: at least one MembershipType (the gym owner doesn't know about
//	        types, so the caller passes hasMembershipType)
//	step 4: payment_methods
//	step 5: confirmation
func (g *Gym) NextSetupStep(hasMembershipType bool) int {
	if g.IsSetupComplete() {
		return 0
	}
	if g.Name == nil || strings.TrimSpace(*g.Name) == "" {
		return 2
	}
	if !hasMembershipType {
		return 3
	}
	if len(g.PaymentMethods) == 0 {
		return 4
	}
	return 5
}

// UpdateBasicInfo applies wizard step 2. El paso captura sólo nombre +
// ciudad — el WhatsApp del gym se conecta post-suscripción a Plus desde
// Settings → WhatsApp via UpdateProfile (ADR-009). City vacía queda NULL.
// WhatsApp NO se toca acá; queda como estaba (intencionalmente nil para
// gyms recién creados, o un valor existente si el dueño ya lo conectó).
func (g *Gym) UpdateBasicInfo(name, city string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return gymErrors.ErrInvalidGymName
	}
	g.Name = &name
	if c := strings.TrimSpace(city); c != "" {
		g.City = &c
	} else {
		g.City = nil
	}
	g.Version++
	g.UpdatedAt = time.Now().UTC()
	return nil
}

// UpdatePaymentMethods applies wizard step 4. Validates the set and rejects
// duplicates / unknown values; "Efectivo" pre-marked is the caller's job.
func (g *Gym) UpdatePaymentMethods(methods []string) error {
	if len(methods) == 0 {
		return gymErrors.ErrPaymentMethodsEmpty
	}
	seen := make(map[string]struct{}, len(methods))
	cleaned := make([]string, 0, len(methods))
	for _, m := range methods {
		m = strings.ToLower(strings.TrimSpace(m))
		if m != PaymentCash && m != PaymentTransfer && m != PaymentCard {
			return gymErrors.ErrInvalidPaymentMethod
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		cleaned = append(cleaned, m)
	}
	g.PaymentMethods = cleaned
	g.Version++
	g.UpdatedAt = time.Now().UTC()
	return nil
}

// CompleteSetup is the wizard final step (UC-001 step 5). It's a domain
// invariant that all the prerequisite fields are populated; the use case is
// expected to have called the previous mutators already.
func (g *Gym) CompleteSetup(hasMembershipType bool, now time.Time) error {
	if g.Name == nil || *g.Name == "" {
		return gymErrors.ErrSetupIncomplete
	}
	if !hasMembershipType {
		return gymErrors.ErrSetupIncomplete
	}
	if len(g.PaymentMethods) == 0 {
		return gymErrors.ErrSetupIncomplete
	}
	g.SetupCompletedAt = &now
	g.Version++
	g.UpdatedAt = now
	return nil
}

// ProfileUpdate carries the editable surface of UC-005. Only non-nil fields
// are applied; passing all-nils is a no-op (and the caller should treat it as
// such, not bump the version).
type ProfileUpdate struct {
	Name           *string
	City           *string
	WhatsApp       *string
	Timezone       *string
	RFC            *string
	RazonSocial    *string
	CodigoPostal   *string
	RegimenFiscal  *string
	LogoURL        *string
	PrimaryColor   *string
	SecondaryColor *string
	OpenTime       *string
	CloseTime      *string
	// KioskVolume / KioskFeedbackTTLMs map onto KioskSettings["audio_volume"]
	// and KioskSettings["feedback_ttl_ms"] respectively. Cosmetic kiosk knobs
	// (DA-031.5); pointers so the wire layer can patch one without touching
	// the other.
	KioskVolume        *int
	KioskFeedbackTTLMs *int
	// AccessWebhookURL is the outbound URL invoked when a checkin resolves
	// to allowed. Empty string clears the setting. Stored in KioskSettings.
	AccessWebhookURL *string
	// AccessWebhookSecret is an HMAC shared secret. Empty string clears,
	// nil keeps existing.
	AccessWebhookSecret *string
}

// HasPlusOnlyFields reporta si la actualización toca algún campo
// reservado al tier Plus. Lo usa el handler PATCH /gyms/me para gatear
// la mutación cuando el gym está en Standard: branding (logo + colores),
// datos fiscales (RFC, razón social, código postal, régimen) y webhook
// de access-granted son features Plus por pricing.
//
// City, WhatsApp, Timezone, OpenTime, CloseTime, KioskVolume / TTL
// siguen siendo Standard — no entran en este check.
func (u ProfileUpdate) HasPlusOnlyFields() bool {
	return u.RFC != nil ||
		u.RazonSocial != nil ||
		u.CodigoPostal != nil ||
		u.RegimenFiscal != nil ||
		u.LogoURL != nil ||
		u.PrimaryColor != nil ||
		u.SecondaryColor != nil ||
		// El webhook de acceso solo es acción Plus cuando se ESTABLECE una
		// URL/secret real. Un valor VACÍO significa "no usar" o "limpiar" la
		// feature y NO debe gatear: el desktop manda access_webhook_url:""
		// en CADA guardado de perfil aunque el gym Standard no tenga webhook,
		// y un puntero a "" no es nil (omitempty solo afecta el marshaling),
		// así que sin este guard TODO gym Standard recibía 402 al guardar su
		// propio perfil. ApplyProfileUpdate ya trata "" como clear sin error.
		profileFieldSet(u.AccessWebhookURL) ||
		profileFieldSet(u.AccessWebhookSecret)
}

// profileFieldSet reporta si el puntero trae un valor no vacío (tras trim).
// Distingue "el cliente mandó un valor real" de "mandó vacío/no lo tocó".
func profileFieldSet(p *string) bool {
	return p != nil && strings.TrimSpace(*p) != ""
}

func (g *Gym) ApplyProfileUpdate(u ProfileUpdate) error {
	if u.Name != nil {
		name := strings.TrimSpace(*u.Name)
		if name == "" || len(name) > 100 {
			return gymErrors.ErrInvalidGymName
		}
		g.Name = &name
	}
	if u.City != nil {
		v := strings.TrimSpace(*u.City)
		if v == "" {
			g.City = nil
		} else {
			g.City = &v
		}
	}
	if u.WhatsApp != nil {
		// Normaliza a E.164 (quita espacios/guiones/paréntesis y antepone +52
		// a nacionales de 10 dígitos) ANTES de validar, en vez de sólo
		// trim+rechazar — así el dueño puede teclear "+52 446 105 7446".
		v := phonepkg.Normalize(*u.WhatsApp)
		if v == "" {
			g.WhatsApp = nil
		} else {
			if !whatsappRegex.MatchString(v) {
				return gymErrors.ErrInvalidWhatsApp
			}
			g.WhatsApp = &v
		}
	}
	if u.Timezone != nil {
		v := strings.TrimSpace(*u.Timezone)
		if v != "" {
			g.Timezone = v
		}
	}
	if u.RFC != nil {
		v := strings.TrimSpace(strings.ToUpper(*u.RFC))
		if v == "" {
			g.RFC = nil
		} else {
			if !rfcRegex.MatchString(v) {
				return gymErrors.ErrInvalidRFC
			}
			g.RFC = &v
		}
	}
	if u.RazonSocial != nil {
		v := strings.TrimSpace(*u.RazonSocial)
		if v == "" {
			g.RazonSocial = nil
		} else {
			g.RazonSocial = &v
		}
	}
	if u.CodigoPostal != nil {
		v := strings.TrimSpace(*u.CodigoPostal)
		if v == "" {
			g.CodigoPostal = nil
		} else {
			g.CodigoPostal = &v
		}
	}
	if u.RegimenFiscal != nil {
		v := strings.TrimSpace(*u.RegimenFiscal)
		if v == "" {
			g.RegimenFiscal = nil
		} else {
			g.RegimenFiscal = &v
		}
	}
	if u.LogoURL != nil {
		v := strings.TrimSpace(*u.LogoURL)
		if v == "" {
			g.LogoURL = nil
		} else {
			g.LogoURL = &v
		}
	}
	if u.PrimaryColor != nil {
		if err := assignColor(&g.PrimaryColor, *u.PrimaryColor); err != nil {
			return err
		}
	}
	if u.SecondaryColor != nil {
		if err := assignColor(&g.SecondaryColor, *u.SecondaryColor); err != nil {
			return err
		}
	}
	if u.OpenTime != nil {
		v := strings.TrimSpace(*u.OpenTime)
		if v == "" {
			g.OpenTime = nil
		} else {
			g.OpenTime = &v
		}
	}
	if u.CloseTime != nil {
		v := strings.TrimSpace(*u.CloseTime)
		if v == "" {
			g.CloseTime = nil
		} else {
			g.CloseTime = &v
		}
	}
	if u.KioskVolume != nil {
		v := *u.KioskVolume
		if v < 0 || v > 100 {
			return gymErrors.ErrInvalidKioskVolume
		}
		if g.KioskSettings == nil {
			g.KioskSettings = map[string]any{}
		}
		g.KioskSettings["audio_volume"] = v
	}
	if u.KioskFeedbackTTLMs != nil {
		v := *u.KioskFeedbackTTLMs
		if v < 500 || v > 30000 {
			return gymErrors.ErrInvalidKioskFeedback
		}
		if g.KioskSettings == nil {
			g.KioskSettings = map[string]any{}
		}
		g.KioskSettings["feedback_ttl_ms"] = v
	}
	if u.AccessWebhookURL != nil {
		v := strings.TrimSpace(*u.AccessWebhookURL)
		if g.KioskSettings == nil {
			g.KioskSettings = map[string]any{}
		}
		if v == "" {
			delete(g.KioskSettings, "access_webhook_url")
		} else {
			// Light validation — allow http(s) URLs only. The webhook is
			// outbound from the sidecar so http://localhost:3000/door is
			// legitimately useful for a Pi on the same LAN as the kiosk.
			if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
				return gymErrors.ErrInvalidKioskFeedback
			}
			g.KioskSettings["access_webhook_url"] = v
		}
	}
	if u.AccessWebhookSecret != nil {
		v := strings.TrimSpace(*u.AccessWebhookSecret)
		if g.KioskSettings == nil {
			g.KioskSettings = map[string]any{}
		}
		if v == "" {
			delete(g.KioskSettings, "access_webhook_secret")
		} else {
			g.KioskSettings["access_webhook_secret"] = v
		}
	}
	g.Version++
	g.UpdatedAt = time.Now().UTC()
	return nil
}

// ChargeSettingsInput es la forma con la que la app capa pasa el set
// completo de configuraciones de cobros a nivel gym. Todos los campos
// son punteros para que cada llamada decida explícitamente qué tocar:
// un nil significa "no cambiar"; un valor (incluido false o 0) significa
// "fijar a este valor".
type ChargeSettingsInput struct {
	ChargesEnrollment    *bool
	ChargesMaintenance   *bool
	EnrollmentAmount     *float64
	MaintenanceAmount    *float64
	MaintenanceFrequency *string
}

var allowedMaintenanceFreqs = map[string]bool{
	"monthly":    true,
	"bimonthly":  true,
	"quarterly":  true,
	"semiannual": true,
	"annual":     true,
}

// UpdateChargeSettings reemplaza/parchea los campos de cobro del gym.
// Validaciones: montos deben ser >= 0, la frecuencia debe ser uno de
// los valores soportados. Vacío en frecuencia limpia el setting.
func (g *Gym) UpdateChargeSettings(in ChargeSettingsInput, now time.Time) error {
	if in.EnrollmentAmount != nil && *in.EnrollmentAmount < 0 {
		return gymErrors.ErrInvalidChargeAmount
	}
	if in.MaintenanceAmount != nil && *in.MaintenanceAmount < 0 {
		return gymErrors.ErrInvalidChargeAmount
	}
	if in.MaintenanceFrequency != nil {
		v := strings.TrimSpace(*in.MaintenanceFrequency)
		if v != "" && !allowedMaintenanceFreqs[v] {
			return gymErrors.ErrInvalidMaintenanceFrequency
		}
	}
	if g.ChargeSettings == nil {
		g.ChargeSettings = map[string]any{}
	}
	if in.ChargesEnrollment != nil {
		g.ChargeSettings["charges_enrollment"] = *in.ChargesEnrollment
	}
	if in.ChargesMaintenance != nil {
		g.ChargeSettings["charges_maintenance"] = *in.ChargesMaintenance
	}
	if in.EnrollmentAmount != nil {
		g.ChargeSettings["enrollment_amount"] = *in.EnrollmentAmount
	}
	if in.MaintenanceAmount != nil {
		g.ChargeSettings["maintenance_amount"] = *in.MaintenanceAmount
	}
	if in.MaintenanceFrequency != nil {
		v := strings.TrimSpace(*in.MaintenanceFrequency)
		if v == "" {
			delete(g.ChargeSettings, "maintenance_frequency")
		} else {
			g.ChargeSettings["maintenance_frequency"] = v
		}
	}
	g.Version++
	g.UpdatedAt = now
	return nil
}

// ConnectWhatsApp records the gym's WhatsApp Business number + the optional
// provider-side token (Twilio sub-account auth or Meta access token). Used
// by UC-037 after the verification ceremony succeeds. `now` is the
// connection moment — after this point notifications.dispatcher will route
// outbound messages through the configured number.
func (g *Gym) ConnectWhatsApp(phone string, tokenEnc []byte, now time.Time) error {
	v := phonepkg.Normalize(phone)
	if v == "" || !whatsappRegex.MatchString(v) {
		return gymErrors.ErrInvalidWhatsApp
	}
	g.WhatsAppBusinessPhone = &v
	if len(tokenEnc) > 0 {
		g.WhatsAppBusinessTokenEnc = tokenEnc
	}
	g.WhatsAppConnectedAt = &now
	g.Version++
	g.UpdatedAt = now
	return nil
}

// IsWhatsAppConnected reports whether the gym has finished UC-037.
func (g *Gym) IsWhatsAppConnected() bool {
	return g.WhatsAppConnectedAt != nil && g.WhatsAppBusinessPhone != nil && *g.WhatsAppBusinessPhone != ""
}

// UsesOwnWhatsAppNumber reports whether outbound socio/owner messages must be
// sent from the gym's OWN connected WhatsApp Business number instead of from
// Tinta's master sender. Per ADR-009 §2.1 only Plus-tier gyms that completed
// UC-037 send from their own number; Standard, trial, and Plus-not-yet-
// connected gyms all send from Tinta's master sender. The dispatcher uses
// this to resolve the `from`; the gym name already rides in the template
// variables so the socio keeps the gym context either way.
func (g *Gym) UsesOwnWhatsAppNumber() bool {
	return IsPlusPlan(g.SubscriptionPlan) && g.IsWhatsAppConnected()
}

// DisconnectWhatsApp limpia los campos de WhatsApp del gym (UC-037 desconectar).
// Idempotente: llamarlo sobre un gym que ya está desconectado es no-op.
// El número y token se borran completamente — si el dueño quiere volver a
// conectar tiene que pasar por el flow de OTP de cero.
func (g *Gym) DisconnectWhatsApp(now time.Time) {
	if !g.IsWhatsAppConnected() {
		return
	}
	g.WhatsAppBusinessPhone = nil
	g.WhatsAppBusinessTokenEnc = nil
	g.WhatsAppConnectedAt = nil
	g.Version++
	g.UpdatedAt = now
}

// NotifyMemberPinEnabled reports whether the welcome-PIN WhatsApp message
// should fire when a socio is enrolled or has their PIN regenerated.
// Stored under KioskSettings["notify_member_pin"]; default true (a missing
// key counts as enabled — auto-send is the default product behavior).
func (g *Gym) NotifyMemberPinEnabled() bool {
	if g.KioskSettings == nil {
		return true
	}
	v, ok := g.KioskSettings["notify_member_pin"]
	if !ok {
		return true
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return true
}

// MemberNumberDigits reports the configured length of the gym's member
// numbers (ADR-010 §2.1). Stored under KioskSettings["member_number_digits"];
// default DefaultMemberNumberDigits (4). El valor sólo crece (ver
// GrowMemberNumberDigits) — nunca baja, así los números ya asignados no se
// rompen. Un valor presente pero menor al default se trata como el default
// (fail-safe ante data corrupta o downgrades manuales).
func (g *Gym) MemberNumberDigits() int {
	const def = 4 // = member.DefaultMemberNumberDigits (sin importar el pkg members)
	if g.KioskSettings == nil {
		return def
	}
	v, ok := g.KioskSettings["member_number_digits"]
	if !ok {
		return def
	}
	// JSON round-trips numbers as float64; el seed local los deja como int.
	var d int
	switch n := v.(type) {
	case float64:
		d = int(n)
	case int:
		d = n
	case int64:
		d = int(n)
	default:
		return def
	}
	if d < def {
		return def
	}
	return d
}

// GrowMemberNumberDigits sube la longitud del número de socio a `digits`
// SÓLO si es mayor a la actual (monótono — ADR-010 §2.1: "sólo crece; al
// reconciliar entre dispositivos gana el máximo"). Devuelve true si cambió.
// El asignador lo llama al consumir ~50% del espacio del dígito actual.
func (g *Gym) GrowMemberNumberDigits(digits int, now time.Time) bool {
	if digits <= g.MemberNumberDigits() {
		return false
	}
	if g.KioskSettings == nil {
		g.KioskSettings = map[string]any{}
	}
	g.KioskSettings["member_number_digits"] = digits
	g.Version++
	g.UpdatedAt = now
	return true
}

// OwnerWelcomeSent reporta si ya se envió el banner "tu sistema ya está vivo"
// al dueño (ADR-010 / onboarding). Guardado en KioskSettings["owner_welcomed"].
// Es el guard de fire-once: el WhatsApp + la regeneración del código de acceso
// se disparan UNA sola vez, en el primer link del primer dispositivo del gym.
func (g *Gym) OwnerWelcomeSent() bool {
	if g.KioskSettings == nil {
		return false
	}
	if b, ok := g.KioskSettings["owner_welcomed"].(bool); ok {
		return b
	}
	return false
}

// MarkOwnerWelcomeSent fija el flag de fire-once. Idempotente: si ya estaba
// marcado, no bumpea version. Debe persistirse en la MISMA tx que la
// regeneración del código + el encolado de la notificación.
func (g *Gym) MarkOwnerWelcomeSent(now time.Time) {
	if g.OwnerWelcomeSent() {
		return
	}
	if g.KioskSettings == nil {
		g.KioskSettings = map[string]any{}
	}
	g.KioskSettings["owner_welcomed"] = true
	g.Version++
	g.UpdatedAt = now
}

func assignColor(target **string, raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" {
		*target = nil
		return nil
	}
	if !colorRegex.MatchString(v) {
		return gymErrors.ErrInvalidColor
	}
	*target = &v
	return nil
}

// ---------------------------------------------------------------------------
// Validators (chain of responsibility, Kash style)
// ---------------------------------------------------------------------------

var (
	// rfcRegex matches Mexican RFC (UC-005 validation).
	rfcRegex = regexp.MustCompile(`^[A-Z&Ñ]{3,4}[0-9]{6}[A-Z0-9]{3}$`)
	// whatsappRegex accepts E.164: optional +, 10-15 digits.
	whatsappRegex = regexp.MustCompile(`^\+?[1-9]\d{9,14}$`)
	// colorRegex matches #RRGGBB.
	colorRegex = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

// Validator is the chain interface; each validator may delegate to Next.
type Validator interface {
	Validate(g *Gym) error
}

type subscriptionValidator struct{ Next Validator }

func (v *subscriptionValidator) Validate(g *Gym) error {
	switch g.SubscriptionPlan {
	case PlanTrial, PlanStandardMonthly, PlanStandardAnnual, PlanPlusMonthly, PlanPlusAnnual:
	default:
		return gymErrors.ErrSetupIncomplete
	}
	if v.Next != nil {
		return v.Next.Validate(g)
	}
	return nil
}

// BuildValidatorChain returns the standard chain used at create time.
func BuildValidatorChain() Validator {
	return &subscriptionValidator{}
}
