package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func paymentCommandFingerprint(input any) (string, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func validatePaymentCommandKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		// Internal/one-release compatibility. HTTP handlers reject a missing
		// key; this fallback keeps older in-process callers from becoming a
		// second public contract while intentionally providing no retry dedupe.
		return "legacy:" + uuid.NewString(), nil
	}
	if len(key) > 120 {
		return "", sharedDomain.NewValidationError(billingErrors.ErrIdempotencyKeyRequired)
	}
	return key, nil
}

func replayPaymentCommand[T any](tx sharedDomain.Transaction, payments billingRepo.PaymentRepository,
	gymID uuid.UUID, key, fingerprint, concept string, out *T) (bool, error) {
	store, ok := payments.(billingRepo.PaymentIdempotencyStore)
	if !ok {
		return false, sharedDomain.NewUnexpectedError(errors.New("payment idempotency store is not configured"))
	}
	existing, err := store.GetByIdempotencyKey(tx, gymID, key)
	if err != nil {
		return false, sharedDomain.NewUnexpectedError(err)
	}
	if existing == nil {
		return false, nil
	}
	if existing.Concept != concept || existing.IdempotencyFingerprint != fingerprint {
		return false, sharedDomain.NewBusinessError(billingErrors.ErrIdempotencyKeyConflict, "")
	}
	if len(existing.IdempotencyResult) == 0 {
		return false, sharedDomain.NewUnexpectedError(errors.New("keyed payment has no replay result"))
	}
	if err := json.Unmarshal(existing.IdempotencyResult, out); err != nil {
		return false, sharedDomain.NewUnexpectedError(err)
	}
	return true, nil
}

func beginPaymentCommand(p *paymentDomain.Payment, key, fingerprint string) error {
	if err := p.BeginIdempotency(key, fingerprint); err != nil {
		return sharedDomain.NewValidationError(err)
	}
	return nil
}

func finalizePaymentCommand(tx sharedDomain.Transaction, payments billingRepo.PaymentRepository,
	p *paymentDomain.Payment, result any, now time.Time) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	if err := p.FinalizeIdempotency(payload, now); err != nil {
		return sharedDomain.NewValidationError(err)
	}
	store, ok := payments.(billingRepo.PaymentIdempotencyStore)
	if !ok {
		return sharedDomain.NewUnexpectedError(errors.New("payment idempotency store is not configured"))
	}
	if err := store.FinalizeIdempotency(tx, p); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	return nil
}
