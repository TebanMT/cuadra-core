// Package salecorrection models the immutable audit record produced when an
// owner (or the original operator in the narrow record-only case) fixes a
// product sale. Money snapshots are JSON with integer cents by design.
package salecorrection

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
)

const (
	RecordOnly               = "record_only"
	RefundExcess             = "refund_excess"
	RefundPending            = "refund_pending"
	TypeEdit                 = "edit"
	TypeAnnul                = "annul"
	IncreasePending          = "pending"
	IncreaseAlreadyCollected = "already_collected"
	IncreaseCollectNow       = "collect_now"
)

type Correction struct {
	ID                     uuid.UUID
	GymID                  uuid.UUID
	Version                int
	SaleID                 uuid.UUID
	ExpectedSaleVersion    int
	Reason                 string
	CorrectionType         string
	MoneyResolution        string
	IncreaseResolution     string
	MonetaryDelta          float64
	BeforeSnapshot         json.RawMessage
	AfterSnapshot          json.RawMessage
	RefundID               *uuid.UUID
	IdempotencyKey         string
	IdempotencyFingerprint string
	IdempotencyResult      json.RawMessage
	CreatedBy              uuid.UUID
	CreatedAt              time.Time
	UpdatedAt              time.Time
	DeletedAt              *time.Time
}

type Input struct {
	ID, GymID, SaleID, CreatedBy  uuid.UUID
	ExpectedSaleVersion           int
	Reason, CorrectionType        string
	MoneyResolution               string
	IncreaseResolution            string
	MonetaryDelta                 float64
	BeforeSnapshot, AfterSnapshot json.RawMessage
	RefundID                      *uuid.UUID
	IdempotencyKey                string
	IdempotencyFingerprint        string
	IdempotencyResult             json.RawMessage
	Now                           time.Time
}

func New(in Input) (*Correction, error) {
	reason := strings.TrimSpace(in.Reason)
	key := strings.TrimSpace(in.IdempotencyKey)
	if reason == "" {
		return nil, billingErrors.ErrSaleCorrectionReasonRequired
	}
	if len(reason) > 200 {
		return nil, billingErrors.ErrSaleCorrectionReasonRequired
	}
	if in.ExpectedSaleVersion < 0 {
		return nil, billingErrors.ErrSaleVersionConflict
	}
	if in.MoneyResolution != RecordOnly && in.MoneyResolution != RefundExcess && in.MoneyResolution != RefundPending {
		return nil, billingErrors.ErrSaleCorrectionResolutionInvalid
	}
	correctionType := strings.TrimSpace(in.CorrectionType)
	if correctionType == "" {
		correctionType = TypeEdit
	}
	if correctionType != TypeEdit && correctionType != TypeAnnul {
		return nil, billingErrors.ErrSaleCorrectionResolutionInvalid
	}
	if in.IncreaseResolution != "" && in.IncreaseResolution != IncreasePending &&
		in.IncreaseResolution != IncreaseAlreadyCollected && in.IncreaseResolution != IncreaseCollectNow {
		return nil, billingErrors.ErrSaleCorrectionResolutionInvalid
	}
	if key == "" {
		return nil, billingErrors.ErrIdempotencyKeyRequired
	}
	fingerprint := strings.TrimSpace(in.IdempotencyFingerprint)
	if fingerprint == "" {
		return nil, billingErrors.ErrIdempotencyKeyRequired
	}
	if len(in.IdempotencyResult) > 0 && !json.Valid(in.IdempotencyResult) {
		return nil, billingErrors.ErrSaleCorrectionSnapshotInvalid
	}
	if !json.Valid(in.BeforeSnapshot) || !json.Valid(in.AfterSnapshot) {
		return nil, billingErrors.ErrSaleCorrectionSnapshotInvalid
	}
	return &Correction{
		ID: in.ID, GymID: in.GymID, Version: 1, SaleID: in.SaleID,
		ExpectedSaleVersion: in.ExpectedSaleVersion, Reason: reason, CorrectionType: correctionType,
		MoneyResolution: in.MoneyResolution, IncreaseResolution: in.IncreaseResolution,
		MonetaryDelta:  round(in.MonetaryDelta),
		BeforeSnapshot: append(json.RawMessage(nil), in.BeforeSnapshot...),
		AfterSnapshot:  append(json.RawMessage(nil), in.AfterSnapshot...), RefundID: in.RefundID,
		IdempotencyKey: key, IdempotencyFingerprint: fingerprint,
		IdempotencyResult: append(json.RawMessage(nil), in.IdempotencyResult...),
		CreatedBy:         in.CreatedBy, CreatedAt: in.Now, UpdatedAt: in.Now,
	}, nil
}

func round(v float64) float64 {
	if v >= 0 {
		return float64(int64(v*100+0.5)) / 100
	}
	return float64(int64(v*100-0.5)) / 100
}
