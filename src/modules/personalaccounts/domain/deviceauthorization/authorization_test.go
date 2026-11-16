package deviceauthorization

import (
	"errors"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/google/uuid"
)

func verifiedAttempt() (*attempt.AuthAttempt, time.Time) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	accountID := uuid.New()
	verified := now.Add(-time.Minute)
	a := &attempt.AuthAttempt{Binding: attempt.Binding{ID: uuid.New(), ContinuationHash: [32]byte{1}}, AccountID: &accountID, ProposedAccountID: uuid.New(), Candidate: attempt.Candidate{ID: uuid.New(), PublicKey: [32]byte{2}, Platform: "ios", Label: "Synthetic"}, Nonce: [32]byte{3}, Purpose: attempt.Access, DestinationE164: "+525555550101", Channel: verification.SMS, RequestHash: [32]byte{4}, State: attempt.Verified, ProviderReference: "synthetic", Revision: 2, VerifiedAt: &verified, CreatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(8 * time.Minute)}
	return a, now
}
func TestAuthorizationCandidateDeadlineAndTerminalDecision(t *testing.T) {
	a, now := verifiedAttempt()
	v, err := New(uuid.New(), a, "000042", now)
	if err != nil || !v.ExpiresAt.Equal(a.ExpiresAt) || v.Candidate != a.Candidate {
		t.Fatal("authorization changed candidate or extended verified attempt")
	}
	if err = v.Decide(v.Candidate.ID, Approve, now); !errors.Is(err, personalErrors.ErrInvalidDeviceAuthorization) || v.Status != Pending {
		t.Fatal("candidate approved itself")
	}
	actor := uuid.New()
	if err = v.Decide(actor, Approve, now); err != nil || v.Status != Approved || v.ApprovedByDeviceID == nil || *v.ApprovedByDeviceID != actor {
		t.Fatal("approval not bound to existing actor")
	}
	if err = v.Decide(actor, Deny, now); !errors.Is(err, personalErrors.ErrDeviceAuthorizationState) || v.Status != Approved {
		t.Fatal("terminal approval changed")
	}
	if v.EffectiveStatus(now.Add(time.Hour)) != Approved {
		t.Fatal("committed authorization turned pending/expired")
	}
	denied, err := New(uuid.New(), a, "123456", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = denied.Decide(actor, Deny, now); err != nil || denied.Status != Denied || denied.ApprovedByDeviceID != nil {
		t.Fatal("denial recorded authority")
	}
	expired, err := New(uuid.New(), a, "123456", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = expired.Decide(actor, Approve, expired.ExpiresAt); !errors.Is(err, personalErrors.ErrDeviceAuthorizationExpired) || expired.Status != Pending {
		t.Fatal("exact deadline authorized device")
	}
	if expired.EffectiveStatus(expired.ExpiresAt) != Expired {
		t.Fatal("expired request still advertised pending")
	}
}
func TestAuthorizationRejectsUnverifiedRecoveryAndInvalidSnapshots(t *testing.T) {
	a, now := verifiedAttempt()
	for _, mutate := range []func(*attempt.AuthAttempt){func(a *attempt.AuthAttempt) { a.AccountID = nil }, func(a *attempt.AuthAttempt) { a.Purpose = attempt.Recovery }, func(a *attempt.AuthAttempt) { a.State = attempt.Pending; a.VerifiedAt = nil }, func(a *attempt.AuthAttempt) { a.Candidate.PublicKey = [32]byte{} }} {
		copy := *a
		mutate(&copy)
		if _, err := New(uuid.New(), &copy, "123456", now); err == nil {
			t.Fatal("ineligible attempt authorized request")
		}
	}
	for _, code := range []string{"12345", "1234567", "１２３４５６", "12345x"} {
		if _, err := New(uuid.New(), a, code, now); err == nil {
			t.Fatal("comparison code escaped six digits")
		}
	}
	v, err := New(uuid.New(), a, "123456", now)
	if err != nil {
		t.Fatal(err)
	}
	v.ExpiresAt = v.CreatedAt.Add(MaximumLifetime + time.Millisecond)
	if v.Validate() == nil {
		t.Fatal("oversized authorization lifetime accepted")
	}
}
