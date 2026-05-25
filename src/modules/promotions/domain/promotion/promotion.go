// Package promotion holds the Promotion aggregate y la entity
// AppliedPromotion del BC promotions.
//
// Cambios sobre una Promotion no son retroactivos: cada applied_promotion
// guarda snapshot inmutable (name, kind, value) — desactivar o reescribir
// una promo no toca cobros viejos.
package promotion

import (
	"strings"
	"time"

	"github.com/google/uuid"

	promoErrors "github.com/cuadra/cuadra-core/src/modules/promotions/domain/errors"
)

// Kind values — mirror chk_promotions_kind del schema.
const (
	KindPercent              = "percent"
	KindFixedAmount          = "fixed_amount"
	KindFreeEnrollment       = "free_enrollment"
	KindExtraDays            = "extra_days"
	KindCompanionMemberships = "companion_memberships"
)

// AppliesTo values — mirror chk_promotions_applies_to.
const (
	AppliesToMembership = "membership"
	AppliesToSale       = "sale"
	AppliesToAny        = "any"
)

var allowedKinds = map[string]struct{}{
	KindPercent:              {},
	KindFixedAmount:          {},
	KindFreeEnrollment:       {},
	KindExtraDays:            {},
	KindCompanionMemberships: {},
}

var allowedAppliesTo = map[string]struct{}{
	AppliesToMembership: {},
	AppliesToSale:       {},
	AppliesToAny:        {},
}

// Promotion is the aggregate root del BC. Money en pesos float (la
// conversión a cents la hace el repo SQLite en su borde). Value
// semantics por Kind:
//
//	percent              -> 0-100 (porcentaje)
//	fixed_amount         -> > 0 pesos
//	extra_days           -> >= 1 días
//	free_enrollment      -> nil (no aplica)
//	companion_memberships -> nil; CompanionCount >= 1
type Promotion struct {
	ID               uuid.UUID
	GymID            uuid.UUID
	Version          int
	Name             string
	Description      *string
	Kind             string
	Value            *float64
	BuyN             int
	CompanionCount   *int
	AppliesTo        string
	Code             *string
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	MaxUsesTotal     *int
	MaxUsesPerMember *int
	Active           bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// NewParams agrupa el input del constructor para mantener firma corta —
// el aggregate ya tiene bastantes campos.
type NewParams struct {
	Name             string
	Description      *string
	Kind             string
	Value            *float64
	CompanionCount   *int
	AppliesTo        string
	Code             *string
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	MaxUsesTotal     *int
	MaxUsesPerMember *int
}

// New construye una Promotion validada. BuyN se fija en 1 (futuro N>1
// no expuesto en form; el schema permite el cambio sin migración).
func New(id, gymID uuid.UUID, p NewParams, now time.Time) (*Promotion, error) {
	pr := &Promotion{
		ID:        id,
		GymID:     gymID,
		Version:   1,
		BuyN:      1,
		Active:    true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := pr.applyFields(p); err != nil {
		return nil, err
	}
	return pr, nil
}

// Update reescribe los campos editables. Mantiene el ID y CreatedAt;
// bumpea Version. Snapshots de aplicaciones previas NO se tocan.
func (p *Promotion) Update(params NewParams, now time.Time) error {
	if err := p.applyFields(params); err != nil {
		return err
	}
	p.Version++
	p.UpdatedAt = now
	return nil
}

// Deactivate marca la promo como no disponible para nuevas aplicaciones.
// No es soft-delete (DeletedAt sigue nil) — los applied_promotions FK
// siguen apuntando a esta fila.
func (p *Promotion) Deactivate(now time.Time) {
	if !p.Active {
		return
	}
	p.Active = false
	p.Version++
	p.UpdatedAt = now
}

// Reactivate es simétrico. Re-valida vigencia al aplicar; no auto-extiende
// ValidUntil si ya pasó.
func (p *Promotion) Reactivate(now time.Time) {
	if p.Active {
		return
	}
	p.Active = true
	p.Version++
	p.UpdatedAt = now
}

// IsCurrentlyValid responde si la promo se puede aplicar en `today`
// considerando active + ventana valid_from/valid_until. NO chequea
// max_uses (eso requiere query al repo y vive en el use case).
func (p *Promotion) IsCurrentlyValid(today time.Time) error {
	if !p.Active {
		return promoErrors.ErrPromotionInactive
	}
	d := truncateDate(today)
	if p.ValidFrom != nil && d.Before(truncateDate(*p.ValidFrom)) {
		return promoErrors.ErrPromotionNotYetValid
	}
	if p.ValidUntil != nil && d.After(truncateDate(*p.ValidUntil)) {
		return promoErrors.ErrPromotionExpired
	}
	return nil
}

// AppliesToTarget responde si la promo permite el target dado
// ("membership" o "sale"). `applies_to=any` matchea ambos.
func (p *Promotion) AppliesToTarget(target string) bool {
	if p.AppliesTo == AppliesToAny {
		return true
	}
	return p.AppliesTo == target
}

// NormalizedCode devuelve el código en minúsculas (canonical form para
// lookup). Vacío cuando la promo no tiene código.
func (p *Promotion) NormalizedCode() string {
	if p.Code == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*p.Code))
}

func (p *Promotion) applyFields(params NewParams) error {
	name := strings.TrimSpace(params.Name)
	if len(name) < 3 || len(name) > 100 {
		return promoErrors.ErrInvalidPromotionName
	}
	if _, ok := allowedKinds[params.Kind]; !ok {
		return promoErrors.ErrInvalidPromotionKind
	}
	if _, ok := allowedAppliesTo[params.AppliesTo]; !ok {
		return promoErrors.ErrInvalidAppliesTo
	}
	if err := validateValueByKind(params.Kind, params.Value, params.CompanionCount); err != nil {
		return err
	}
	if params.ValidFrom != nil && params.ValidUntil != nil {
		if truncateDate(*params.ValidUntil).Before(truncateDate(*params.ValidFrom)) {
			return promoErrors.ErrInvalidPromotionDates
		}
	}
	if params.MaxUsesTotal != nil && *params.MaxUsesTotal <= 0 {
		return promoErrors.ErrInvalidMaxUses
	}
	if params.MaxUsesPerMember != nil && *params.MaxUsesPerMember <= 0 {
		return promoErrors.ErrInvalidMaxUses
	}

	p.Name = name
	if params.Description != nil {
		d := strings.TrimSpace(*params.Description)
		if d == "" {
			p.Description = nil
		} else {
			p.Description = &d
		}
	} else {
		p.Description = nil
	}
	p.Kind = params.Kind
	p.AppliesTo = params.AppliesTo

	if params.Value != nil {
		v := *params.Value
		p.Value = &v
	} else {
		p.Value = nil
	}
	if params.CompanionCount != nil {
		c := *params.CompanionCount
		p.CompanionCount = &c
	} else {
		p.CompanionCount = nil
	}
	if params.Code != nil {
		code := strings.TrimSpace(*params.Code)
		if code == "" {
			p.Code = nil
		} else {
			p.Code = &code
		}
	} else {
		p.Code = nil
	}
	if params.ValidFrom != nil {
		t := truncateDate(*params.ValidFrom)
		p.ValidFrom = &t
	} else {
		p.ValidFrom = nil
	}
	if params.ValidUntil != nil {
		t := truncateDate(*params.ValidUntil)
		p.ValidUntil = &t
	} else {
		p.ValidUntil = nil
	}
	if params.MaxUsesTotal != nil {
		v := *params.MaxUsesTotal
		p.MaxUsesTotal = &v
	} else {
		p.MaxUsesTotal = nil
	}
	if params.MaxUsesPerMember != nil {
		v := *params.MaxUsesPerMember
		p.MaxUsesPerMember = &v
	} else {
		p.MaxUsesPerMember = nil
	}
	return nil
}

func validateValueByKind(kind string, value *float64, companionCount *int) error {
	switch kind {
	case KindPercent:
		if value == nil || *value < 0 || *value > 100 {
			return promoErrors.ErrInvalidPromotionValue
		}
	case KindFixedAmount:
		if value == nil || *value <= 0 {
			return promoErrors.ErrInvalidPromotionValue
		}
	case KindExtraDays:
		if value == nil || *value < 1 {
			return promoErrors.ErrInvalidPromotionValue
		}
	case KindFreeEnrollment:
		if value != nil {
			return promoErrors.ErrInvalidPromotionValue
		}
	case KindCompanionMemberships:
		if value != nil {
			return promoErrors.ErrInvalidPromotionValue
		}
		if companionCount == nil || *companionCount < 1 {
			return promoErrors.ErrInvalidCompanionCount
		}
	}
	return nil
}

// truncateDate normaliza a medianoche UTC — fechas calendario no llevan
// hora (ValidFrom/ValidUntil).
func truncateDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
