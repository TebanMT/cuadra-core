// Package repository declara los puertos del BC promotions.
// Las implementaciones viven en infraestructure/db/repositories/ (una
// versión por motor; comparten dominio).
package repository

import (
	"time"

	"github.com/google/uuid"

	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListFilter define los filtros del catálogo de promociones.
//   - IncludeInactive: si false, sólo Active=true.
//   - AppliesTo: vacío = sin filtro; "membership"/"sale" matchea + "any".
//   - CurrentlyValid: si non-nil, filtra por ValidFrom/ValidUntil cubriendo
//     la fecha dada (usado para listar promos vigentes hoy desde el cobro).
type ListFilter struct {
	GymID           uuid.UUID
	IncludeInactive bool
	AppliesTo       string
	CurrentlyValid  *time.Time
}

// AppliedSummary es el rollup mensual del reporte del dashboard.
type AppliedSummary struct {
	PromotionID    uuid.UUID
	PromotionName  string
	Kind           string
	UseCount       int
	TotalDiscount  float64
	MembersReached int
}

// PromotionRepository expone CRUD + búsqueda por código.
type PromotionRepository interface {
	Create(tx sharedDomain.Transaction, p *promoDomain.Promotion) (*promoDomain.Promotion, error)
	Update(tx sharedDomain.Transaction, p *promoDomain.Promotion) (*promoDomain.Promotion, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*promoDomain.Promotion, error)
	GetByCode(tx sharedDomain.Transaction, gymID uuid.UUID, codeLower string) (*promoDomain.Promotion, error)
	List(tx sharedDomain.Transaction, f ListFilter) ([]*promoDomain.Promotion, error)
	ExistsByCode(tx sharedDomain.Transaction, gymID uuid.UUID, codeLower string, excludeID *uuid.UUID) (bool, error)
}

// AppliedPromotionRepository persiste las aplicaciones + queries para
// enforcement de límites y reportes.
type AppliedPromotionRepository interface {
	Create(tx sharedDomain.Transaction, ap *promoDomain.AppliedPromotion) (*promoDomain.AppliedPromotion, error)
	CountByPromotion(tx sharedDomain.Transaction, promotionID uuid.UUID) (int, error)
	CountByPromotionAndMember(tx sharedDomain.Transaction, promotionID, memberID uuid.UUID) (int, error)
	SummaryByMonth(tx sharedDomain.Transaction, gymID uuid.UUID, monthStart, monthEnd time.Time) ([]AppliedSummary, error)
}
