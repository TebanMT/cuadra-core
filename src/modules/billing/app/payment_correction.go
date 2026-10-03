package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	correctionDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/paymentcorrection"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CorrectPaymentInput struct {
	CashDestination               string
	GymID, ActorUserID, PaymentID uuid.UUID
	ActorRole                     string
	ExpectedVersion               int
	Reason                        string
	Annul                         bool
	Amount                        *float64
	PaymentMethod                 string
	CashDrawerID                  *uuid.UUID
	PaymentDate                   time.Time
	IdempotencyKey                string
}

type PaymentCorrectionSnapshot struct {
	CashDestination  string     `json:"cash_destination"`
	Version          int        `json:"version"`
	Amount           float64    `json:"amount"`
	RecognizedAmount float64    `json:"recognized_amount"`
	BalancePending   float64    `json:"balance_pending"`
	PaymentMethod    string     `json:"payment_method"`
	CashDrawerID     *uuid.UUID `json:"cash_drawer_id"`
	PaymentDate      string     `json:"payment_date"`
	Annulled         bool       `json:"annulled"`
}

type CorrectPaymentOutput struct {
	CorrectionID          uuid.UUID                 `json:"correction_id"`
	PaymentID             uuid.UUID                 `json:"payment_id"`
	PaymentVersion        int                       `json:"payment_version"`
	Before                PaymentCorrectionSnapshot `json:"before"`
	After                 PaymentCorrectionSnapshot `json:"after"`
	ServiceEffectsChanged bool                      `json:"service_effects_changed"`
	Annulled              bool                      `json:"annulled"`
}

type PaymentCorrectionHistory struct {
	ID                     uuid.UUID                 `json:"id"`
	PaymentID              uuid.UUID                 `json:"payment_id"`
	ExpectedPaymentVersion int                       `json:"expected_payment_version"`
	Reason                 string                    `json:"reason"`
	Before                 PaymentCorrectionSnapshot `json:"before"`
	After                  PaymentCorrectionSnapshot `json:"after"`
	CreatedBy              uuid.UUID                 `json:"created_by"`
	CreatedAt              time.Time                 `json:"created_at"`
	Annulled               bool                      `json:"annulled"`
}

type CorrectPayment struct {
	Payments    billingRepo.PaymentRepository
	Corrections billingRepo.PaymentCorrectionRepository
	CashMarker  CashSessionCorrectionMarker
	CashDrawers CashDrawerValidator
	Gyms        gymRepo.GymRepository
	UoW         sharedDomain.UnitOfWork
	Audit       audit.Recorder
}

func NewCorrectPayment(payments billingRepo.PaymentRepository, corrections billingRepo.PaymentCorrectionRepository,
	cashMarker CashSessionCorrectionMarker, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CorrectPayment {
	return &CorrectPayment{Payments: payments, Corrections: corrections, CashMarker: cashMarker, UoW: uow, Audit: recorder}
}

func (uc *CorrectPayment) WithCashDrawers(v CashDrawerValidator) *CorrectPayment {
	uc.CashDrawers = v
	return uc
}

func (uc *CorrectPayment) WithGyms(gyms gymRepo.GymRepository) *CorrectPayment {
	uc.Gyms = gyms
	return uc
}

func (uc *CorrectPayment) Execute(ctx context.Context, in CorrectPaymentInput) (*CorrectPaymentOutput, error) {
	if in.ActorRole != "owner" {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrPaymentCorrectionForbidden, "")
	}
	if len(strings.TrimSpace(in.Reason)) < 3 {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentCorrectionReasonRequired)
	}
	if in.ExpectedVersion <= 0 {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentVersionConflict)
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" || len(key) > 120 {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrIdempotencyKeyRequired)
	}
	if in.Amount != nil && *in.Amount <= 0 {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrAmountInvalid)
	}
	if in.Annul && (in.CashDestination != "" || in.Amount != nil || in.PaymentMethod != "" || in.CashDrawerID != nil || !in.PaymentDate.IsZero()) {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentCorrectionAnnulMixed)
	}
	if in.PaymentMethod != "" && !validRealRefundMethod(in.PaymentMethod) {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodInvalid)
	}
	fingerprint, err := paymentCorrectionFingerprint(in)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	var out CorrectPaymentOutput
	now := time.Now().UTC()
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		var today time.Time
		if !in.Annul {
			var calendarErr error
			today, calendarErr = strictGymLocalPaymentDate(tx, uc.Gyms, in.GymID, now)
			if calendarErr != nil {
				return calendarErr
			}
		}
		existing, err := uc.Corrections.GetByIdempotencyKey(tx, in.GymID, key)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if existing != nil {
			return replayPaymentCorrection(existing, fingerprint, &out)
		}
		payment, err := uc.Payments.GetByID(tx, in.PaymentID)
		if err != nil {
			return err
		}
		if payment.GymID != in.GymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}
		if payment.Version != in.ExpectedVersion {
			return sharedDomain.NewBusinessError(billingErrors.ErrPaymentVersionConflict, "")
		}
		if payment.ParentPaymentID != nil || (payment.Concept != paymentDomain.ConceptMembership && payment.Concept != paymentDomain.ConceptOther) {
			return sharedDomain.NewBusinessError(billingErrors.ErrPaymentCorrectionUnsupported, "")
		}
		locker, ok := uc.Payments.(billingRepo.PaymentRefundLocker)
		if !ok {
			return sharedDomain.NewUnexpectedError(errors.New("payment refund locker is not configured"))
		}
		balance, err := locker.RefundBalanceForUpdate(tx, in.GymID, payment.ID)
		if err != nil {
			return err
		}
		if cents(balance.Refunded) != 0 || cents(balance.Collected) != cents(payment.Amount) || cents(balance.Recognized) != cents(payment.RecognizedAmount) {
			return sharedDomain.NewBusinessError(billingErrors.ErrPaymentCorrectionUnsupported, "")
		}

		before := paymentCorrectionSnapshot(payment)
		if in.Annul {
			if payment.Concept == paymentDomain.ConceptMembership {
				return sharedDomain.NewBusinessError(billingErrors.ErrPaymentCorrectionAnnulMembership, "")
			}
			if err := payment.AnnulAdministrative(now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
		} else {
			amount := payment.Amount
			if in.Amount != nil {
				amount = roundMoney(*in.Amount)
			}
			newBalance := payment.BalancePending
			if payment.Concept == paymentDomain.ConceptMembership && in.Amount != nil {
				obligation := roundMoney(payment.Amount + payment.BalancePending)
				if amount > obligation {
					return sharedDomain.NewValidationError(billingErrors.ErrPartialAmountInvalid)
				}
				newBalance = roundMoney(obligation - amount)
			}
			method := payment.PaymentMethod
			if in.PaymentMethod != "" {
				method = in.PaymentMethod
			}
			date := payment.PaymentDate
			if !in.PaymentDate.IsZero() {
				date = in.PaymentDate
			}
			date = time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
			if date.After(today) {
				return sharedDomain.NewValidationError(billingErrors.ErrPaymentDateInvalid)
			}
			if in.CashDestination != "" {
				if err := payment.SetCashDestination(in.CashDestination); err != nil {
					return sharedDomain.NewValidationError(err)
				}
			}
			var drawerID *uuid.UUID
			if method == paymentDomain.MethodCash && payment.EffectiveCashDestination() != "gym_fund" {
				drawer := payment.EffectiveCashDrawerID()
				if payment.PaymentMethod != paymentDomain.MethodCash || drawer == uuid.Nil {
					drawer = in.GymID
				}
				if in.CashDrawerID != nil {
					drawer = *in.CashDrawerID
				}
				drawerID = &drawer
			}
			if payment.EffectiveCashDestination() != "gym_fund" {
				if err := validateRequestedCashDrawer(uc.CashDrawers, tx, in.GymID, method, drawerID); err != nil {
					return err
				}
			}
			if before.CashDestination == payment.EffectiveCashDestination() && samePaymentCorrection(before, amount, newBalance, method, drawerID, date) {
				return sharedDomain.NewValidationError(billingErrors.ErrPaymentCorrectionNoChanges)
			}
			if err := payment.CorrectAdministrative(amount, newBalance, method, drawerID, date, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
		}
		writer, ok := uc.Payments.(billingRepo.PaymentAdministrativeCorrectionStore)
		if !ok {
			return sharedDomain.NewUnexpectedError(errors.New("payment administrative correction store is not configured"))
		}
		if _, err := writer.UpdateForAdministrativeCorrection(tx, payment, in.ExpectedVersion); err != nil {
			return err
		}
		after := paymentCorrectionSnapshot(payment)
		beforeRaw, _ := json.Marshal(before)
		afterRaw, _ := json.Marshal(after)
		correction, err := correctionDomain.New(correctionDomain.Input{
			ID: uuid.New(), GymID: in.GymID, PaymentID: payment.ID, CreatedBy: in.ActorUserID,
			ExpectedPaymentVersion: in.ExpectedVersion, Reason: in.Reason,
			BeforeSnapshot: beforeRaw, AfterSnapshot: afterRaw, IdempotencyKey: key,
			IdempotencyFingerprint: fingerprint, Now: now,
		})
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Corrections.Create(tx, correction); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err := uc.markCorrectedCashSessions(ctx, tx, in, before, after, payment.CreatedAt); err != nil {
			return err
		}
		out = CorrectPaymentOutput{CorrectionID: correction.ID, PaymentID: payment.ID, PaymentVersion: payment.Version,
			Before: before, After: after, ServiceEffectsChanged: false, Annulled: in.Annul}
		result, err := json.Marshal(out)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		correction.IdempotencyResult = result
		if err := uc.Corrections.FinalizeIdempotency(tx, correction); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err := uc.Audit.Record(ctx, tx, audit.Entry{GymID: in.GymID, EntityType: "payment_corrections", EntityID: correction.ID,
			Action: audit.ActionCreate, ActorUserID: &in.ActorUserID, Changes: map[string]any{
				"payment_id": payment.ID, "reason": correction.Reason, "before": before, "after": after,
			}, IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: now}); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		return nil
	})
	if err != nil {
		if replay, replayErr := uc.replayCommitted(ctx, in.GymID, key, fingerprint); replayErr != nil {
			return nil, replayErr
		} else if replay != nil {
			return replay, nil
		}
		return nil, err
	}
	return &out, nil
}

func (uc *CorrectPayment) History(ctx context.Context, gymID, paymentID uuid.UUID) ([]PaymentCorrectionHistory, error) {
	out := []PaymentCorrectionHistory{}
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		rows, err := uc.Corrections.ListByPayment(tx, gymID, paymentID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		for _, row := range rows {
			var before, after PaymentCorrectionSnapshot
			if json.Unmarshal(row.BeforeSnapshot, &before) != nil || json.Unmarshal(row.AfterSnapshot, &after) != nil {
				return sharedDomain.NewUnexpectedError(errors.New("payment correction snapshot is invalid"))
			}
			out = append(out, PaymentCorrectionHistory{ID: row.ID, PaymentID: row.PaymentID,
				ExpectedPaymentVersion: row.ExpectedPaymentVersion, Reason: row.Reason, Before: before, After: after,
				CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, Annulled: after.Annulled})
		}
		return nil
	})
	return out, err
}

func (uc *CorrectPayment) replayCommitted(ctx context.Context, gymID uuid.UUID, key, fingerprint string) (*CorrectPaymentOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, nil
	}
	existing, err := uc.Corrections.GetByIdempotencyKey(tx, gymID, key)
	if err != nil || existing == nil {
		return nil, nil
	}
	var out CorrectPaymentOutput
	if err := replayPaymentCorrection(existing, fingerprint, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func replayPaymentCorrection(c *correctionDomain.Correction, fingerprint string, out *CorrectPaymentOutput) error {
	if c.IdempotencyFingerprint != fingerprint {
		return sharedDomain.NewBusinessError(billingErrors.ErrIdempotencyKeyConflict, "")
	}
	if len(c.IdempotencyResult) == 0 || json.Unmarshal(c.IdempotencyResult, out) != nil {
		return sharedDomain.NewUnexpectedError(errors.New("payment correction idempotency result is missing or invalid"))
	}
	return nil
}

func paymentCorrectionSnapshot(p *paymentDomain.Payment) PaymentCorrectionSnapshot {
	var drawerID *uuid.UUID
	if p.PaymentMethod == paymentDomain.MethodCash && p.EffectiveCashDestination() != "gym_fund" {
		v := p.EffectiveCashDrawerID()
		drawerID = &v
	}
	return PaymentCorrectionSnapshot{CashDestination: p.EffectiveCashDestination(), Version: p.Version, Amount: p.Amount, RecognizedAmount: p.RecognizedAmount,
		BalancePending: p.BalancePending, PaymentMethod: p.PaymentMethod, CashDrawerID: drawerID,
		PaymentDate: p.PaymentDate.Format("2006-01-02"), Annulled: p.DeletedAt != nil}
}

func samePaymentCorrection(before PaymentCorrectionSnapshot, amount, balance float64, method string, drawerID *uuid.UUID, date time.Time) bool {
	if cents(before.Amount) != cents(amount) || cents(before.BalancePending) != cents(balance) || before.PaymentMethod != method || before.PaymentDate != date.Format("2006-01-02") {
		return false
	}
	beforeDrawer, afterDrawer := uuid.Nil, uuid.Nil
	if before.CashDrawerID != nil {
		beforeDrawer = *before.CashDrawerID
	}
	if drawerID != nil {
		afterDrawer = *drawerID
	}
	return beforeDrawer == afterDrawer
}

func paymentCorrectionFingerprint(in CorrectPaymentInput) (string, error) {
	var amountCents *int64
	if in.Amount != nil {
		v := int64(math.Round(*in.Amount * 100))
		amountCents = &v
	}
	drawerID := ""
	if in.CashDrawerID != nil {
		drawerID = in.CashDrawerID.String()
	}
	payload := struct {
		GymID, ActorUserID, ActorRole, PaymentID string
		ExpectedVersion                          int
		Reason                                   string
		Annul                                    bool
		AmountCents                              *int64
		PaymentMethod, CashDrawerID, PaymentDate string
		CashDestination                          string
	}{GymID: in.GymID.String(), ActorUserID: in.ActorUserID.String(), ActorRole: strings.TrimSpace(in.ActorRole),
		PaymentID: in.PaymentID.String(), ExpectedVersion: in.ExpectedVersion, Reason: strings.TrimSpace(in.Reason),
		Annul: in.Annul, AmountCents: amountCents, PaymentMethod: strings.TrimSpace(in.PaymentMethod),
		CashDrawerID: drawerID, PaymentDate: correctionFingerprintDate(in.PaymentDate), CashDestination: in.CashDestination}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (uc *CorrectPayment) markCorrectedCashSessions(ctx context.Context, tx sharedDomain.Transaction, in CorrectPaymentInput,
	before, after PaymentCorrectionSnapshot, recordedAt time.Time) error {
	if uc.CashMarker == nil {
		return nil
	}
	type cashLocation struct {
		day    string
		drawer uuid.UUID
	}
	seen := map[cashLocation]bool{}
	for _, snapshot := range []PaymentCorrectionSnapshot{before, after} {
		if snapshot.PaymentMethod != paymentDomain.MethodCash || snapshot.CashDrawerID == nil {
			continue
		}
		location := cashLocation{day: snapshot.PaymentDate, drawer: *snapshot.CashDrawerID}
		if seen[location] {
			continue
		}
		seen[location] = true
		day, err := time.Parse("2006-01-02", snapshot.PaymentDate)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		drawer := location.drawer
		if err := uc.CashMarker.MarkRecordCorrection(ctx, tx, CashSessionAdjustmentInput{GymID: in.GymID, ActorUserID: in.ActorUserID,
			OperationalDate: day, OriginalRecordedAt: recordedAt, Reason: strings.TrimSpace(in.Reason), DrawerID: &drawer}); err != nil {
			return err
		}
	}
	return nil
}
