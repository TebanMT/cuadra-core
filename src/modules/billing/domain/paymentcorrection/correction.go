// Package paymentcorrection models immutable evidence for the exceptional
// administrative rewrite of a membership or extraordinary-income payment.
package paymentcorrection

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
)

type Correction struct {
	ID, GymID, PaymentID, CreatedBy uuid.UUID
	Version                         int
	ExpectedPaymentVersion          int
	Reason                          string
	BeforeSnapshot                  json.RawMessage
	AfterSnapshot                   json.RawMessage
	IdempotencyKey                  string
	IdempotencyFingerprint          string
	IdempotencyResult               json.RawMessage
	CreatedAt, UpdatedAt            time.Time
	DeletedAt                       *time.Time
}

type Input struct {
	ID, GymID, PaymentID, CreatedBy uuid.UUID
	ExpectedPaymentVersion          int
	Reason                          string
	BeforeSnapshot                  json.RawMessage
	AfterSnapshot                   json.RawMessage
	IdempotencyKey                  string
	IdempotencyFingerprint          string
	Now                             time.Time
}

func New(in Input) (*Correction, error) {
	reason, key, fingerprint := strings.TrimSpace(in.Reason), strings.TrimSpace(in.IdempotencyKey), strings.TrimSpace(in.IdempotencyFingerprint)
	if len(reason) < 3 || len(reason) > 200 {
		return nil, billingErrors.ErrPaymentCorrectionReasonRequired
	}
	if in.ExpectedPaymentVersion <= 0 {
		return nil, billingErrors.ErrPaymentVersionConflict
	}
	if key == "" || len(key) > 120 || fingerprint == "" {
		return nil, billingErrors.ErrIdempotencyKeyRequired
	}
	if !json.Valid(in.BeforeSnapshot) || !json.Valid(in.AfterSnapshot) {
		return nil, billingErrors.ErrSaleCorrectionSnapshotInvalid
	}
	return &Correction{
		ID: in.ID, GymID: in.GymID, PaymentID: in.PaymentID, CreatedBy: in.CreatedBy,
		Version: 1, ExpectedPaymentVersion: in.ExpectedPaymentVersion, Reason: reason,
		BeforeSnapshot: append(json.RawMessage(nil), in.BeforeSnapshot...), AfterSnapshot: append(json.RawMessage(nil), in.AfterSnapshot...),
		IdempotencyKey: key, IdempotencyFingerprint: fingerprint,
		CreatedAt: in.Now.UTC(), UpdatedAt: in.Now.UTC(),
	}, nil
}
