package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	promoErrors "github.com/cuadra/cuadra-core/src/modules/promotions/domain/errors"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ApplyPromotionInput es lo que el use case de billing pasa cuando el
// operador eligió aplicar una promo al cobro. PromotionID y Code son
// excluyentes (uno o el otro, no ambos). MemberID es opcional según
// el target — requerido para applies_to=membership y para enforcement
// del límite per_member.
//
// CompanionMemberIDs sólo aplica cuando la promo es companion_memberships
// — sin ellos el use case rechaza el cobro entero.
type ApplyPromotionInput struct {
	GymID              uuid.UUID
	ActorUserID        uuid.UUID
	PromotionID        *uuid.UUID
	Code               *string
	PaymentID          uuid.UUID
	Target             string // "membership" o "sale"
	Subtotal           float64
	EnrollmentFee      float64
	HasEnrollment      bool
	MemberID           *uuid.UUID
	CompanionMemberIDs []uuid.UUID
	Notes              *string
	Today              time.Time
}

// ApplyPromotionResult es el efecto resuelto. El caller (billing) lo usa
// para ajustar el subtotal del pago y disparar los efectos secundarios
// (extra_days adjustment, companion memberships) dentro de la misma tx.
// AppliedID se llena DESPUÉS de llamar Persist(); en el output de
// Execute()/Resolve() está zero.
type ApplyPromotionResult struct {
	Promotion          *promoDomain.Promotion
	AppliedID          uuid.UUID
	Discount           float64
	DiscountReason     string
	ExtraDays          int
	CompanionMemberIDs []uuid.UUID
	// internas — el caller no las usa.
	gymID       uuid.UUID
	actorUserID uuid.UUID
	memberID    *uuid.UUID
	notes       *string
}

// ApplyPromotion no abre UoW propia — recibe la tx ya abierta por
// billing. Si algo falla, devuelve error y el rollback del caller cubre
// todo (la promo no queda half-applied).
type ApplyPromotion struct {
	Promotions promoRepo.PromotionRepository
	Applied    promoRepo.AppliedPromotionRepository
}

func NewApplyPromotion(p promoRepo.PromotionRepository, ap promoRepo.AppliedPromotionRepository) *ApplyPromotion {
	return &ApplyPromotion{Promotions: p, Applied: ap}
}

// Resolve valida la promo + corre el calculator + devuelve el efecto a
// aplicar. NO inserta el applied_promotion (el caller necesita primero
// crear el payment para tener el payment_id del FK). Para persistir el
// applied_promotion, el caller llama Persist(...) con el payment ya
// creado.
//
// Si el caller no necesita el split (ej. un test integration), puede
// llamar Execute(...) que hace Resolve + Persist en una sola pasada
// asumiendo que el caller ya tiene el payment_id reservado.
func (uc *ApplyPromotion) Resolve(ctx context.Context, tx sharedDomain.Transaction, in ApplyPromotionInput, now time.Time) (*ApplyPromotionResult, error) {
	// 1. Resolver la promo (por ID o por código).
	var (
		p   *promoDomain.Promotion
		err error
	)
	switch {
	case in.PromotionID != nil:
		p, err = uc.Promotions.GetByID(tx, *in.PromotionID)
	case in.Code != nil:
		codeLower := strings.ToLower(strings.TrimSpace(*in.Code))
		if codeLower == "" {
			return nil, sharedDomain.NewValidationError(promoErrors.ErrPromotionCodeNotFound)
		}
		p, err = uc.Promotions.GetByCode(tx, in.GymID, codeLower)
	default:
		return nil, sharedDomain.NewValidationError(promoErrors.ErrPromotionNotFound)
	}
	if err != nil {
		return nil, err
	}
	if p.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(promoErrors.ErrCrossGym, "")
	}

	// 2. Vigencia + active.
	if err := p.IsCurrentlyValid(in.Today); err != nil {
		return nil, sharedDomain.NewBusinessError(err, "")
	}

	// 3. Target compatible.
	if !p.AppliesToTarget(in.Target) {
		return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionNotApplicableToTarget, "")
	}

	// 4. Límite total de usos.
	if p.MaxUsesTotal != nil {
		used, err := uc.Applied.CountByPromotion(tx, p.ID)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		if used >= *p.MaxUsesTotal {
			return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionUsageLimitExceeded, "")
		}
	}

	// 5. Límite per-member (sólo aplicable si hay socio).
	if p.MaxUsesPerMember != nil {
		if in.MemberID == nil {
			return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionRequiresMember, "")
		}
		used, err := uc.Applied.CountByPromotionAndMember(tx, p.ID, *in.MemberID)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		if used >= *p.MaxUsesPerMember {
			return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionUsageLimitPerMember, "")
		}
	}

	// 6. companion_memberships exige cantidad correcta de socios.
	if p.Kind == promoDomain.KindCompanionMemberships {
		want := 0
		if p.CompanionCount != nil {
			want = *p.CompanionCount
		}
		if len(in.CompanionMemberIDs) == 0 {
			return nil, sharedDomain.NewBusinessError(promoErrors.ErrCompanionMembersRequired, "")
		}
		if len(in.CompanionMemberIDs) != want {
			return nil, sharedDomain.NewBusinessError(promoErrors.ErrCompanionMembersMismatch, "")
		}
	}

	// 7. Calcular efecto.
	calc := promoDomain.Calculate(p, promoDomain.CalcInput{
		Subtotal:      in.Subtotal,
		EnrollmentFee: in.EnrollmentFee,
		HasEnrollment: in.HasEnrollment,
	})

	reason := "Promoción: " + p.Name
	if p.Code != nil && *p.Code != "" {
		reason += " (código " + strings.ToUpper(*p.Code) + ")"
	}
	return &ApplyPromotionResult{
		Promotion:          p,
		Discount:           calc.Discount,
		DiscountReason:     reason,
		ExtraDays:          calc.ExtraDays,
		CompanionMemberIDs: append([]uuid.UUID(nil), in.CompanionMemberIDs...),
		gymID:              in.GymID,
		actorUserID:        in.ActorUserID,
		memberID:           in.MemberID,
		notes:              in.Notes,
	}, nil
}

// Persist inserta el applied_promotion ahora que el payment ya existe.
// Llamado por el caller (billing) inmediatamente después de Payments.Create.
// Llena res.AppliedID en-place.
func (uc *ApplyPromotion) Persist(ctx context.Context, tx sharedDomain.Transaction, res *ApplyPromotionResult, paymentID uuid.UUID, now time.Time) error {
	if res == nil || res.Promotion == nil {
		return nil
	}
	ap := promoDomain.NewApplied(uuid.New(), res.gymID, res.Promotion, promoDomain.CalcResult{
		Discount:       res.Discount,
		ExtraDays:      res.ExtraDays,
		CompanionCount: len(res.CompanionMemberIDs),
	}, promoDomain.NewAppliedParams{
		PromotionID:     res.Promotion.ID,
		PaymentID:       paymentID,
		MemberID:        res.memberID,
		AppliedByUserID: res.actorUserID,
		Notes:           res.notes,
	}, now)
	stored, err := uc.Applied.Create(tx, ap)
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	res.AppliedID = stored.ID
	return nil
}

// Execute es el método legacy de un-paso para casos donde el caller ya
// reservó el paymentID y NO necesita el split Resolve/Persist (tests
// integration, callers que asumen FK deferrable). Internamente es
// Resolve + Persist en la misma tx.
func (uc *ApplyPromotion) Execute(ctx context.Context, tx sharedDomain.Transaction, in ApplyPromotionInput, now time.Time) (*ApplyPromotionResult, error) {
	res, err := uc.Resolve(ctx, tx, in, now)
	if err != nil {
		return nil, err
	}
	if err := uc.Persist(ctx, tx, res, in.PaymentID, now); err != nil {
		return nil, err
	}
	return res, nil
}
