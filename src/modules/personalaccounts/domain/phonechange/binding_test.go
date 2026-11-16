package phonechange

import (
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/google/uuid"
)

func TestPhoneChangeInitiatorBindingIncludesSessionAndOriginalOperation(t *testing.T) {
	actor := security.AccessClaims{AccountID: uuid.New(), SessionID: uuid.New(), DeviceID: uuid.New()}
	now := time.Now().UTC().Truncate(time.Millisecond)
	a, err := attempt.New(attempt.NewAttempt{Binding: attempt.Binding{ID: uuid.New(), ContinuationHash: [32]byte{1}}, AccountID: &actor.AccountID, ProposedAccountID: actor.AccountID, Candidate: attempt.Candidate{ID: actor.DeviceID, PublicKey: [32]byte{2}, Platform: "ios", Label: "fixture"}, Nonce: [32]byte{3}, Purpose: attempt.PhoneChange, DestinationE164: "+525555550001", Channel: verification.WhatsApp, RequestHash: [32]byte{4}}, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r, err := receipt.New(uuid.New(), receipt.Owner{AccountID: actor.AccountID, AttemptID: a.ID}, StartPurpose, a.RequestHash, now)
	if err != nil {
		t.Fatal(err)
	}
	r.ResultReference = actor.SessionID
	digest := ActorDigest(actor, a.ID, r.OperationID)
	r.ResultSecretHash = digest[:]
	if err = MatchInitiator(a, r, actor); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*security.AccessClaims){func(c *security.AccessClaims) { c.SessionID = uuid.New() }, func(c *security.AccessClaims) { c.DeviceID = uuid.New() }, func(c *security.AccessClaims) { c.AccountID = uuid.New() }} {
		bad := actor
		mutate(&bad)
		if MatchInitiator(a, r, bad) == nil {
			t.Fatal("changed actor accepted")
		}
	}
	copy := *r
	copy.OperationID = uuid.New()
	if MatchInitiator(a, &copy, actor) == nil {
		t.Fatal("changed operation accepted")
	}
	copy = *r
	copy.ResultReference = uuid.New()
	if MatchInitiator(a, &copy, actor) == nil {
		t.Fatal("changed attempt accepted")
	}
	copy = *r
	copy.RequestHash[0]++
	if MatchInitiator(a, &copy, actor) == nil {
		t.Fatal("changed request accepted")
	}
	// JWT refresh changes timestamps, not the initiating session identity.
	renewed := actor
	renewed.IssuedAt = now
	renewed.ExpiresAt = now.Add(time.Minute)
	if MatchInitiator(a, r, renewed) != nil {
		t.Fatal("JWT refresh broke durable actor binding")
	}
}
