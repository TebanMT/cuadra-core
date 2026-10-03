package app

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type RegisterOtherIncomeInput struct {
	GymID, ActorUserID uuid.UUID
	Amount             float64
	Method             string
	CashDrawerID       *uuid.UUID
	CashDestination    string
	Description        string
	PaymentDate        time.Time
	IdempotencyKey     string
}

type RegisterOtherIncomeOutput struct {
	PaymentID uuid.UUID
	Folio     string
	Amount    float64
}

type RegisterOtherIncome struct {
	Payments    billingRepo.PaymentRepository
	Folios      *folioSvc.Generator
	UoW         sharedDomain.UnitOfWork
	Audit       audit.Recorder
	Gyms        gymRepo.GymRepository
	CashDrawers CashDrawerValidator
}

func (uc *RegisterOtherIncome) WithCashDrawers(v CashDrawerValidator) *RegisterOtherIncome {
	uc.CashDrawers = v
	return uc
}

func NewRegisterOtherIncome(payments billingRepo.PaymentRepository, folios *folioSvc.Generator, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *RegisterOtherIncome {
	return &RegisterOtherIncome{Payments: payments, Folios: folios, UoW: uow, Audit: recorder}
}

func (uc *RegisterOtherIncome) WithGyms(gyms gymRepo.GymRepository) *RegisterOtherIncome {
	uc.Gyms = gyms
	return uc
}

func (uc *RegisterOtherIncome) Execute(ctx context.Context, in RegisterOtherIncomeInput) (*RegisterOtherIncomeOutput, error) {
	if in.CashDestination == "" {
		in.CashDestination = "cash_drawer"
	}
	if (in.CashDestination != "cash_drawer" && in.CashDestination != "gym_fund") || (in.CashDestination == "gym_fund" && in.CashDrawerID != nil) {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentCorrectionUnsupported)
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" || len(key) > 120 {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrIdempotencyKeyRequired)
	}
	paymentID := uuid.NewSHA1(in.GymID, []byte("other-income:"+key))
	now := time.Now().UTC()
	var out RegisterOtherIncomeOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		paymentDay, dateErr := resolveMonetaryDate(tx, uc.Gyms, in.GymID, in.PaymentDate, now)
		if dateErr != nil {
			return dateErr
		}
		in.PaymentDate = paymentDay
		existing, lookupErr := uc.Payments.GetByID(tx, paymentID)
		if lookupErr == nil {
			if existing.GymID != in.GymID || existing.Concept != paymentDomain.ConceptOther ||
				math.Round(existing.Amount*100) != math.Round(in.Amount*100) ||
				existing.EffectiveCashDestination() != in.CashDestination || existing.PaymentMethod != in.Method || existing.PaymentDate.Format("2006-01-02") != in.PaymentDate.Format("2006-01-02") ||
				existing.Notes == nil || strings.TrimSpace(*existing.Notes) != strings.TrimSpace(in.Description) ||
				(in.Method == paymentDomain.MethodCash && in.CashDestination == "cash_drawer" && existing.EffectiveCashDrawerID() != requestedCashDrawer(in.GymID, in.CashDrawerID)) {
				return sharedDomain.NewBusinessError(billingErrors.ErrIdempotencyKeyConflict, "")
			}
			out = RegisterOtherIncomeOutput{PaymentID: existing.ID, Folio: existing.Folio, Amount: existing.Amount}
			return nil
		}
		if !errors.Is(lookupErr, billingErrors.ErrPaymentNotFound) {
			return sharedDomain.NewUnexpectedError(lookupErr)
		}
		if in.CashDestination == "cash_drawer" {
			if err := validateRequestedCashDrawer(uc.CashDrawers, tx, in.GymID, in.Method, in.CashDrawerID); err != nil {
				return err
			}
		}
		folio, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptOther)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		p, err := paymentDomain.NewOtherIncomePayment(paymentID, in.GymID, in.ActorUserID, folio, in.Amount, in.Method, in.Description, in.PaymentDate, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if err = p.SetCashDestination(in.CashDestination); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if in.CashDrawerID != nil {
			p.WithCashDrawer(*in.CashDrawerID)
		}
		if _, err = uc.Payments.Create(tx, p); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID: in.GymID, EntityType: "payments", EntityID: p.ID,
			Action: audit.ActionCreate, ActorUserID: &in.ActorUserID,
			Changes: map[string]any{"concept": p.Concept, "amount": p.Amount, "payment_method": p.PaymentMethod,
				"cash_destination": p.EffectiveCashDestination(), "cash_drawer_id": p.CashDrawerID, "description": in.Description, "idempotency_key": key},
			IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: now,
		}); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		out = RegisterOtherIncomeOutput{PaymentID: p.ID, Folio: p.Folio, Amount: p.Amount}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func requestedCashDrawer(gymID uuid.UUID, requested *uuid.UUID) uuid.UUID {
	if requested != nil && *requested != uuid.Nil {
		return *requested
	}
	return gymID
}
