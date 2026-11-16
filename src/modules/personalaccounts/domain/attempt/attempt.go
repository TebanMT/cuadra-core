// Package attempt owns the durable phone verification challenge. Provider
// approval proves phone possession only; account/device/session authorization
// and idempotent response receipts belong to the central application flow.
package attempt

import (
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/google/uuid"
)

const MaxLifetime = 10 * time.Minute

type Purpose string

const (
	Access      Purpose = "access"
	Recovery    Purpose = "recovery"
	PhoneChange Purpose = "phone_change"
)

type State string

const (
	Pending   State = "pending"
	Uncertain State = "uncertain"
	Verified  State = "verified"
	Consumed  State = "consumed"
	Expired   State = "expired"
	Cancelled State = "cancelled"
)

type OperationKind string

const (
	Send  OperationKind = "send"
	Check OperationKind = "check"
)

type Binding struct {
	ID               uuid.UUID
	ContinuationHash [32]byte
}
type Candidate struct {
	ID        uuid.UUID
	PublicKey [32]byte
	Platform  string
	Label     string
}

// ProviderOperation records intent before leaving the database transaction.
// It never contains an OTP, continuation secret or provider response body.
type ProviderOperation struct {
	ID          uuid.UUID
	RequestHash [32]byte
	Kind        OperationKind
	StartedAt   time.Time
}

// OperationToken is captured after reservation and checked again when applying
// the external outcome. Revision invalidates a late outcome after any release.
type OperationToken struct {
	ID          uuid.UUID
	RequestHash [32]byte
	Kind        OperationKind
	Revision    int64
}
type AuthAttempt struct {
	Binding
	AccountID         *uuid.UUID
	ProposedAccountID uuid.UUID
	Candidate         Candidate
	Nonce             [32]byte
	Purpose           Purpose
	DestinationE164   string
	Channel           verification.Channel
	RequestHash       [32]byte
	State             State
	ProviderReference string
	ProviderOperation *ProviderOperation
	Attempts          int
	Revision          int64
	ExpiresAt         time.Time
	VerifiedAt        *time.Time
	ConsumedAt        *time.Time
	ResultReference   *uuid.UUID
	CreatedAt         time.Time
}
type NewAttempt struct {
	Binding
	AccountID         *uuid.UUID
	ProposedAccountID uuid.UUID
	Candidate         Candidate
	Nonce             [32]byte
	Purpose           Purpose
	DestinationE164   string
	Channel           verification.Channel
	RequestHash       [32]byte
}

// The caller supplies the known provider validity; there is no fallback TTL.
// The returned deadline is shortened again by the actual provider outcome.
func New(input NewAttempt, now time.Time, providerValidity time.Duration) (*AuthAttempt, error) {
	if now.IsZero() || providerValidity <= 0 {
		return nil, personalErrors.ErrInvalidAuthAttempt
	}
	if providerValidity > MaxLifetime {
		providerValidity = MaxLifetime
	}
	now = now.UTC().Truncate(time.Millisecond)
	a := &AuthAttempt{Binding: input.Binding, AccountID: copyUUID(input.AccountID), ProposedAccountID: input.ProposedAccountID, Candidate: input.Candidate, Nonce: input.Nonce, Purpose: input.Purpose, DestinationE164: input.DestinationE164, Channel: input.Channel, RequestHash: input.RequestHash, State: Pending, Revision: 1, CreatedAt: now, ExpiresAt: now.Add(providerValidity).Truncate(time.Millisecond)}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return a, nil
}

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

func validUUID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}
func validChannel(channel verification.Channel) bool {
	return channel == verification.SMS || channel == verification.WhatsApp
}
func copyUUID(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func (b Binding) Validate() error {
	if !validUUID(b.ID) || b.ContinuationHash == [32]byte{} {
		return personalErrors.ErrInvalidAuthAttempt
	}
	return nil
}
func (a *AuthAttempt) Validate() error {
	if a == nil || a.Binding.Validate() != nil || !validUUID(a.ProposedAccountID) || !validUUID(a.Candidate.ID) || a.RequestHash == [32]byte{} || a.Nonce == [32]byte{} || a.Candidate.PublicKey == [32]byte{} || a.Revision < 1 || a.Attempts < 0 || a.Attempts > math.MaxInt32 || a.CreatedAt.IsZero() || a.ExpiresAt.IsZero() || (a.State != Expired && !a.ExpiresAt.After(a.CreatedAt)) || a.ExpiresAt.Sub(a.CreatedAt) > MaxLifetime || !e164.MatchString(a.DestinationE164) || !validChannel(a.Channel) {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if a.AccountID != nil && !validUUID(*a.AccountID) {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if a.Purpose != Access && a.Purpose != Recovery && a.Purpose != PhoneChange {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if a.Purpose == PhoneChange && (a.AccountID == nil || *a.AccountID != a.ProposedAccountID) {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if (a.Candidate.Platform != "android" && a.Candidate.Platform != "ios") || !utf8.ValidString(a.Candidate.Label) || strings.ContainsRune(a.Candidate.Label, 0) || utf8.RuneCountInString(a.Candidate.Label) < 1 || utf8.RuneCountInString(a.Candidate.Label) > 80 {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if len(a.ProviderReference) > 256 || strings.ContainsAny(a.ProviderReference, "\x00\r\n") {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if op := a.ProviderOperation; op != nil {
		if !validUUID(op.ID) || op.RequestHash == [32]byte{} || (op.Kind != Send && op.Kind != Check) || op.StartedAt.Before(a.CreatedAt) || (a.State != Expired && !op.StartedAt.Before(a.ExpiresAt)) {
			return personalErrors.ErrInvalidAuthAttempt
		}
	}
	if a.VerifiedAt != nil && (a.ProviderReference == "" || a.VerifiedAt.Before(a.CreatedAt) || !a.VerifiedAt.Before(a.ExpiresAt)) {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if a.ConsumedAt != nil && (a.VerifiedAt == nil || a.ConsumedAt.Before(*a.VerifiedAt) || !a.ConsumedAt.Before(a.ExpiresAt)) {
		return personalErrors.ErrInvalidAuthAttempt
	}
	switch a.State {
	case Pending, Uncertain:
		if a.VerifiedAt != nil || a.ConsumedAt != nil || a.ResultReference != nil || (a.State == Uncertain && a.ProviderOperation == nil) {
			return personalErrors.ErrInvalidAuthAttempt
		}
	case Verified:
		if a.VerifiedAt == nil || a.ConsumedAt != nil || a.ResultReference != nil {
			return personalErrors.ErrInvalidAuthAttempt
		}
	case Consumed:
		if a.AccountID == nil || a.VerifiedAt == nil || a.ConsumedAt == nil || a.ResultReference == nil || !validUUID(*a.ResultReference) {
			return personalErrors.ErrInvalidAuthAttempt
		}
	case Expired, Cancelled:
		if a.ConsumedAt != nil || a.ResultReference != nil {
			return personalErrors.ErrInvalidAuthAttempt
		}
	default:
		return personalErrors.ErrInvalidAuthAttempt
	}
	return nil
}

func (a *AuthAttempt) live(now time.Time) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if now.IsZero() || now.Before(a.CreatedAt) {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if (a.ProviderOperation != nil && now.Before(a.ProviderOperation.StartedAt)) || (a.VerifiedAt != nil && now.Before(*a.VerifiedAt)) {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if !now.Before(a.ExpiresAt) {
		return personalErrors.ErrAuthAttemptExpired
	}
	if a.Revision == math.MaxInt64 {
		return personalErrors.ErrAuthAttemptConflict
	}
	return nil
}

func (a *AuthAttempt) reserve(id uuid.UUID, hash [32]byte, kind OperationKind, now time.Time) (OperationToken, error) {
	if err := a.live(now); err != nil {
		return OperationToken{}, err
	}
	if a.State == Uncertain {
		return OperationToken{}, personalErrors.ErrAuthAttemptUncertain
	}
	if a.State != Pending {
		return OperationToken{}, personalErrors.ErrAuthAttemptState
	}
	if !validUUID(id) || hash == [32]byte{} || (a.ProviderOperation != nil && a.ProviderOperation.ID == id) {
		return OperationToken{}, personalErrors.ErrAuthAttemptOperation
	}
	a.ProviderOperation = &ProviderOperation{ID: id, RequestHash: hash, Kind: kind, StartedAt: now.UTC().Truncate(time.Millisecond)}
	a.State, a.Revision = Uncertain, a.Revision+1
	return OperationToken{ID: id, RequestHash: hash, Kind: kind, Revision: a.Revision}, nil
}

func (a *AuthAttempt) ReserveSend(id uuid.UUID, hash [32]byte, channel verification.Channel, now time.Time) (OperationToken, error) {
	if !validChannel(channel) {
		return OperationToken{}, personalErrors.ErrInvalidAuthAttempt
	}
	token, err := a.reserve(id, hash, Send, now)
	if err == nil {
		a.Channel = channel
	}
	return token, err
}

// The maximum is mandatory configuration, not a hardcoded commercial policy.
// The increment is committed before Check so an ambiguous outcome costs a try.
func (a *AuthAttempt) ReserveCheck(id uuid.UUID, hash [32]byte, now time.Time, maxChecks int) (OperationToken, error) {
	if err := a.live(now); err != nil {
		return OperationToken{}, err
	}
	if maxChecks <= 0 || maxChecks > math.MaxInt32 {
		return OperationToken{}, personalErrors.ErrInvalidAuthAttempt
	}
	if a.ProviderReference == "" {
		return OperationToken{}, personalErrors.ErrAuthAttemptState
	}
	if a.Attempts >= maxChecks {
		return OperationToken{}, personalErrors.ErrAuthAttemptCheckLimit
	}
	token, err := a.reserve(id, hash, Check, now)
	if err == nil {
		a.Attempts++
	}
	return token, err
}

func (a *AuthAttempt) matches(token OperationToken) bool {
	op := a.ProviderOperation
	return op != nil && op.ID == token.ID && op.RequestHash == token.RequestHash && op.Kind == token.Kind && a.Revision == token.Revision
}

// ApplyOutcome accepts only known provider facts tied to this exact reserved
// operation. Missing/timeout/404 are not facts of approval: leave uncertain.
// A known provider deadline in the past persists Expired (never Verified),
// even if the local deadline has not yet elapsed. Inspect State after success.
func (a *AuthAttempt) ApplyOutcome(token OperationToken, result verification.Result, now time.Time) error {
	if err := a.live(now); err != nil {
		return err
	}
	if a.State != Uncertain || !a.matches(token) {
		return personalErrors.ErrAuthAttemptOperation
	}
	if result.Reference == "" || len(result.Reference) > 256 || strings.ContainsAny(result.Reference, "\x00\r\n") || result.Channel != a.Channel || result.CreatedAt.IsZero() || result.CreatedAt.After(now) || !result.ExpiresAt.After(result.CreatedAt) {
		return personalErrors.ErrAuthAttemptOperation
	}
	if token.Kind == Check && result.Reference != a.ProviderReference {
		return personalErrors.ErrAuthAttemptOperation
	}
	deadline := a.ExpiresAt
	if result.ExpiresAt.Before(deadline) {
		deadline = result.ExpiresAt.UTC().Truncate(time.Millisecond)
	}
	next := Pending
	switch result.State {
	case verification.Pending:
	case verification.Approved:
		if token.Kind != Check {
			return personalErrors.ErrAuthAttemptOperation
		}
		next = Verified
	case verification.Expired:
		next = Expired
	case verification.Cancelled, verification.AttemptsExhausted, verification.Failed:
		next = Cancelled
	default:
		return personalErrors.ErrAuthAttemptOperation
	}
	// A reused provider verification may already have expired before this local
	// attempt was created. Persist that known terminal fact without extending it.
	if !now.Before(deadline) {
		next = Expired
	}
	a.ProviderReference, a.ExpiresAt, a.State = result.Reference, deadline, next
	if next == Verified {
		stamp := now.UTC().Truncate(time.Millisecond)
		a.VerifiedAt = &stamp
	}
	a.Revision++
	return nil
}

// ReleaseUncertain is for a NEW explicit user request after reconciliation and
// a configured cooldown. The caller handles budget reservations and receipts.
// Keep the old operation fingerprint; its token is invalidated by revision.
func (a *AuthAttempt) ReleaseUncertain(token OperationToken, now time.Time, minDelay time.Duration) error {
	if err := a.live(now); err != nil {
		return err
	}
	if a.State != Uncertain || !a.matches(token) {
		return personalErrors.ErrAuthAttemptOperation
	}
	if minDelay <= 0 {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if now.Before(a.ProviderOperation.StartedAt.Add(minDelay)) {
		return personalErrors.ErrAuthAttemptCooldown
	}
	a.State, a.Revision = Pending, a.Revision+1
	return nil
}

// BindAccount does not resolve identity or create an account. The caller must
// first prove and lock that identity. A recovery attempt never creates the
// client-proposed account; its verified kit selects an already existing one.
func (a *AuthAttempt) BindAccount(id uuid.UUID, now time.Time) error {
	if err := a.live(now); err != nil {
		return err
	}
	if a.State != Verified || !validUUID(id) {
		return personalErrors.ErrAuthAttemptState
	}
	if a.AccountID != nil {
		if *a.AccountID == id {
			return nil
		}
		return personalErrors.ErrAuthAttemptOperation
	}
	a.AccountID = copyUUID(&id)
	a.Revision++
	return nil
}

func (a *AuthAttempt) Consume(resultReference uuid.UUID, now time.Time) error {
	if err := a.live(now); err != nil {
		return err
	}
	if a.State != Verified || a.AccountID == nil || !validUUID(resultReference) || now.Before(*a.VerifiedAt) {
		return personalErrors.ErrAuthAttemptState
	}
	stamp := now.UTC().Truncate(time.Millisecond)
	a.State, a.ConsumedAt, a.ResultReference, a.Revision = Consumed, &stamp, copyUUID(&resultReference), a.Revision+1
	return nil
}
func (a *AuthAttempt) Cancel(now time.Time) error {
	if err := a.live(now); err != nil {
		return err
	}
	if a.State != Pending && a.State != Uncertain && a.State != Verified {
		return personalErrors.ErrAuthAttemptState
	}
	a.State, a.Revision = Cancelled, a.Revision+1
	return nil
}
func (a *AuthAttempt) Expire(now time.Time) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if now.Before(a.ExpiresAt) || a.Revision == math.MaxInt64 {
		return personalErrors.ErrAuthAttemptState
	}
	if a.State != Pending && a.State != Uncertain && a.State != Verified {
		return personalErrors.ErrAuthAttemptState
	}
	a.State, a.Revision = Expired, a.Revision+1
	return nil
}
