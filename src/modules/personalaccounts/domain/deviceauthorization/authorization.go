package deviceauthorization

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

type Status string

const (
	Pending   Status = "pending"
	Approved  Status = "approved"
	Denied    Status = "denied"
	Expired   Status = "expired"
	Cancelled Status = "cancelled"
)

type Decision string

const (
	Approve Decision = "approve"
	Deny    Decision = "deny"
)

const MaximumLifetime = 10 * time.Minute

// Candidate is an immutable snapshot of the challenge, not client input at
// approval time. The database projection joins its existing immutable row.
type Authorization struct {
	ID                 uuid.UUID
	AccountID          uuid.UUID
	AttemptID          uuid.UUID
	Candidate          attempt.Candidate
	ComparisonCode     string
	Status             Status
	ApprovedByDeviceID *uuid.UUID
	ExpiresAt          time.Time
	DecidedAt          *time.Time
	CreatedAt          time.Time
}

func (Authorization) String() string {
	return "device authorization (candidate and comparison redacted)"
}
func (a Authorization) GoString() string { return a.String() }

func ValidID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}
func validTime(t time.Time) bool { return !t.IsZero() && t.UTC().Year() >= 1 && t.UTC().Year() <= 9999 }
func ValidCandidate(c attempt.Candidate) bool {
	return ValidID(c.ID) && c.PublicKey != [32]byte{} && (c.Platform == "ios" || c.Platform == "android") && utf8.ValidString(c.Label) && !strings.ContainsRune(c.Label, 0) && utf8.RuneCountInString(c.Label) >= 1 && utf8.RuneCountInString(c.Label) <= 80
}
func New(id uuid.UUID, a *attempt.AuthAttempt, code string, now time.Time) (*Authorization, error) {
	if a == nil || a.Validate() != nil || a.Purpose != attempt.Access || a.State != attempt.Verified || a.AccountID == nil || !validTime(now) || now.Before(*a.VerifiedAt) || !now.Before(a.ExpiresAt) {
		return nil, personalErrors.ErrDeviceAuthorizationState
	}
	now = now.UTC().Truncate(time.Millisecond)
	expires := now.Add(MaximumLifetime)
	if a.ExpiresAt.Before(expires) {
		expires = a.ExpiresAt
	}
	v := &Authorization{ID: id, AccountID: *a.AccountID, AttemptID: a.ID, Candidate: a.Candidate, ComparisonCode: code, Status: Pending, CreatedAt: now, ExpiresAt: expires}
	return v, v.Validate()
}
func (a *Authorization) Validate() error {
	if a == nil || !ValidID(a.ID) || !ValidID(a.AccountID) || !ValidID(a.AttemptID) || !ValidCandidate(a.Candidate) || len(a.ComparisonCode) != 6 || !validTime(a.CreatedAt) || !validTime(a.ExpiresAt) || !a.ExpiresAt.After(a.CreatedAt) || a.ExpiresAt.Sub(a.CreatedAt) > MaximumLifetime {
		return personalErrors.ErrInvalidDeviceAuthorization
	}
	for _, c := range a.ComparisonCode {
		if c < '0' || c > '9' {
			return personalErrors.ErrInvalidDeviceAuthorization
		}
	}
	if a.DecidedAt != nil && (!validTime(*a.DecidedAt) || a.DecidedAt.Before(a.CreatedAt)) {
		return personalErrors.ErrInvalidDeviceAuthorization
	}
	if a.ApprovedByDeviceID != nil && (!ValidID(*a.ApprovedByDeviceID) || *a.ApprovedByDeviceID == a.Candidate.ID) {
		return personalErrors.ErrInvalidDeviceAuthorization
	}
	switch a.Status {
	case Pending:
		if a.DecidedAt != nil || a.ApprovedByDeviceID != nil {
			return personalErrors.ErrInvalidDeviceAuthorization
		}
	case Approved:
		if a.DecidedAt == nil || a.ApprovedByDeviceID == nil || !a.DecidedAt.Before(a.ExpiresAt) {
			return personalErrors.ErrInvalidDeviceAuthorization
		}
	case Denied:
		if a.DecidedAt == nil || a.ApprovedByDeviceID != nil || !a.DecidedAt.Before(a.ExpiresAt) {
			return personalErrors.ErrInvalidDeviceAuthorization
		}
	case Expired, Cancelled:
		// Cancellation of an unconsumed historical approval may retain its
		// approver. Expiry/cancellation cannot turn into a new approval.
	default:
		return personalErrors.ErrInvalidDeviceAuthorization
	}
	return nil
}
func (a *Authorization) EffectiveStatus(now time.Time) Status {
	if a.Status == Pending && !now.Before(a.ExpiresAt) {
		return Expired
	}
	return a.Status
}
func (a *Authorization) Decide(actorDeviceID uuid.UUID, decision Decision, now time.Time) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if !ValidID(actorDeviceID) || actorDeviceID == a.Candidate.ID || !validTime(now) || now.Before(a.CreatedAt) || (decision != Approve && decision != Deny) {
		return personalErrors.ErrInvalidDeviceAuthorization
	}
	if a.Status != Pending {
		return personalErrors.ErrDeviceAuthorizationState
	}
	if !now.Before(a.ExpiresAt) {
		return personalErrors.ErrDeviceAuthorizationExpired
	}
	at := now.UTC().Truncate(time.Millisecond)
	a.DecidedAt = &at
	if decision == Approve {
		a.Status = Approved
		actor := actorDeviceID
		a.ApprovedByDeviceID = &actor
	} else {
		a.Status = Denied
	}
	return a.Validate()
}

type ListPosition struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func (p ListPosition) Validate() error {
	if !ValidID(p.ID) || !validTime(p.CreatedAt) {
		return personalErrors.ErrInvalidDeviceAuthorization
	}
	return nil
}
