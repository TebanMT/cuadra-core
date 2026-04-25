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
)

// SubscriptionPlan and SubscriptionStatus mirror the chk_ constraints in
// db_migrations/postgres/001_init_schema.sql.
const (
	PlanTrial      = "trial"
	PlanProMonthly = "pro_monthly"
	PlanProAnnual  = "pro_annual"

	StatusActive    = "active"
	StatusPastDue   = "past_due"
	StatusCancelled = "cancelled"

	PaymentCash     = "cash"
	PaymentTransfer = "transfer"
	PaymentCard     = "card"
)

// Gym is the multi-tenant root. Every other entity in the system carries the
// gym's id; for the gym itself the gym_id column equals id (ADR-002 §3.1).
type Gym struct {
	ID                       uuid.UUID
	Version                  int
	Name                     *string
	City                     *string
	WhatsApp                 *string
	Country                  string
	Timezone                 string
	RFC                      *string
	RazonSocial              *string
	CodigoPostal             *string
	RegimenFiscal            *string
	LogoURL                  *string
	PrimaryColor             *string
	SecondaryColor           *string
	PaymentMethods           []string
	OpenTime                 *string // "HH:MM:SS"
	CloseTime                *string
	SubscriptionPlan         string
	TrialEndsAt              *time.Time
	SubscriptionEndsAt       *time.Time
	SubscriptionStatus       string
	SetupCompletedAt         *time.Time
	WhatsAppBusinessPhone    *string
	WhatsAppBusinessTokenEnc []byte
	WhatsAppConnectedAt      *time.Time
	KioskSettings            map[string]any
	CreatedAt                time.Time
	UpdatedAt                time.Time
	DeletedAt                *time.Time
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
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// IsSetupComplete reports whether the gym wizard finished (UC-001 step 5).
func (g *Gym) IsSetupComplete() bool { return g.SetupCompletedAt != nil }

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

// UpdateBasicInfo applies wizard step 2. Empty strings are normalised to nil
// so the column stays NULL until something is provided.
func (g *Gym) UpdateBasicInfo(name, city, whatsapp string) error {
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
	if w := strings.TrimSpace(whatsapp); w != "" {
		if !whatsappRegex.MatchString(w) {
			return gymErrors.ErrInvalidWhatsApp
		}
		g.WhatsApp = &w
	} else {
		g.WhatsApp = nil
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
		v := strings.TrimSpace(*u.WhatsApp)
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
	g.Version++
	g.UpdatedAt = time.Now().UTC()
	return nil
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
	case PlanTrial, PlanProMonthly, PlanProAnnual:
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
