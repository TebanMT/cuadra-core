package attempt

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/google/uuid"
)

var testNow = time.Date(2026, 10, 19, 3, 0, 0, 123000000, time.UTC)

func input() NewAttempt {
	return NewAttempt{Binding: Binding{ID: uuid.New(), ContinuationHash: [32]byte{1}}, ProposedAccountID: uuid.New(), Candidate: Candidate{ID: uuid.New(), PublicKey: [32]byte{2}, Platform: "ios", Label: "Teléfono de Lucía"}, Nonce: [32]byte{3}, Purpose: Access, DestinationE164: "+525512345678", Channel: verification.WhatsApp, RequestHash: [32]byte{4}}
}
func fresh(t *testing.T) *AuthAttempt {
	t.Helper()
	a, err := New(input(), testNow, MaxLifetime)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func clone(a *AuthAttempt) *AuthAttempt {
	b := *a
	b.AccountID = copyUUID(a.AccountID)
	b.ResultReference = copyUUID(a.ResultReference)
	if a.ProviderOperation != nil {
		op := *a.ProviderOperation
		b.ProviderOperation = &op
	}
	if a.VerifiedAt != nil {
		stamp := *a.VerifiedAt
		b.VerifiedAt = &stamp
	}
	if a.ConsumedAt != nil {
		stamp := *a.ConsumedAt
		b.ConsumedAt = &stamp
	}
	return &b
}
func provider(state verification.State) verification.Result {
	return verification.Result{Reference: "VE-test-fixture", Channel: verification.WhatsApp, State: state, CreatedAt: testNow, ExpiresAt: testNow.Add(5 * time.Minute)}
}
func sent(t *testing.T) *AuthAttempt {
	t.Helper()
	a := fresh(t)
	token, err := a.ReserveSend(uuid.New(), [32]byte{5}, verification.WhatsApp, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ApplyOutcome(token, provider(verification.Pending), testNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return a
}
func verified(t *testing.T) *AuthAttempt {
	t.Helper()
	a := sent(t)
	token, err := a.ReserveCheck(uuid.New(), [32]byte{6}, testNow.Add(2*time.Second), 3)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ApplyOutcome(token, provider(verification.Approved), testNow.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewAttemptValidatesBindingAndKnownExpiry(t *testing.T) {
	for name, mutate := range map[string]func(*NewAttempt){
		"id": func(i *NewAttempt) { i.ID = uuid.Nil }, "continuation": func(i *NewAttempt) { i.ContinuationHash = [32]byte{} }, "request": func(i *NewAttempt) { i.RequestHash = [32]byte{} },
		"proposed": func(i *NewAttempt) { i.ProposedAccountID = uuid.Nil }, "candidate": func(i *NewAttempt) { i.Candidate.ID = uuid.Nil }, "key": func(i *NewAttempt) { i.Candidate.PublicKey = [32]byte{} }, "nonce": func(i *NewAttempt) { i.Nonce = [32]byte{} },
		"platform": func(i *NewAttempt) { i.Candidate.Platform = "web" }, "label empty": func(i *NewAttempt) { i.Candidate.Label = "" }, "label long": func(i *NewAttempt) { i.Candidate.Label = strings.Repeat("ñ", 81) }, "label utf8": func(i *NewAttempt) { i.Candidate.Label = string([]byte{255}) }, "label nul": func(i *NewAttempt) { i.Candidate.Label = "name\x00" },
		"phone": func(i *NewAttempt) { i.DestinationE164 = "525512345678" }, "channel": func(i *NewAttempt) { i.Channel = "email" }, "purpose": func(i *NewAttempt) { i.Purpose = "signup" }, "phone change unbound": func(i *NewAttempt) { i.Purpose = PhoneChange },
	} {
		t.Run(name, func(t *testing.T) {
			i := input()
			mutate(&i)
			if _, err := New(i, testNow, MaxLifetime); !errors.Is(err, personalErrors.ErrInvalidAuthAttempt) {
				t.Fatal(err)
			}
		})
	}
	for _, ttl := range []time.Duration{0, -time.Second, time.Nanosecond} {
		if _, err := New(input(), testNow, ttl); err == nil {
			t.Fatal("invalid ttl accepted")
		}
	}
	if _, err := New(input(), time.Time{}, time.Minute); err == nil {
		t.Fatal("zero clock accepted")
	}
	i := input()
	i.Candidate.Label = strings.Repeat("ñ", 80)
	a, err := New(i, testNow.In(time.FixedZone("Mexico", -6*3600)), time.Hour)
	if err != nil || a.State != Pending || a.Revision != 1 || a.ExpiresAt.Sub(a.CreatedAt) != MaxLifetime || a.CreatedAt.Location() != time.UTC {
		t.Fatal("constructor lost bounds", err)
	}
	i.Purpose = PhoneChange
	i.AccountID = &i.ProposedAccountID
	a, err = New(i, testNow, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	*i.AccountID = uuid.New()
	if *a.AccountID != a.ProposedAccountID {
		t.Fatal("account pointer aliased")
	}
}

func TestAttemptPhonePossessionThenExplicitAccountConsumption(t *testing.T) {
	a := verified(t)
	if a.State != Verified || a.AccountID != nil || a.Revision != 5 || a.Attempts != 1 || a.ResultReference != nil || !a.ExpiresAt.Equal(testNow.Add(5*time.Minute)) {
		t.Fatal("OTP incorrectly authorized or extended lifetime", a)
	}
	if err := a.Consume(uuid.New(), testNow.Add(4*time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptState) {
		t.Fatal("unbound consumption", err)
	}
	if err := a.BindAccount(a.ProposedAccountID, testNow.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := a.BindAccount(*a.AccountID, testNow.Add(4*time.Second)); err != nil || a.Revision != 6 {
		t.Fatal("same binding not idempotent", err)
	}
	if err := a.BindAccount(uuid.New(), testNow.Add(4*time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptOperation) {
		t.Fatal("account switched", err)
	}
	result := uuid.New()
	if err := a.Consume(result, testNow.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if a.State != Consumed || *a.ResultReference != result || a.Revision != 7 || a.Validate() != nil {
		t.Fatal("invalid completed attempt")
	}
	before := clone(a)
	if _, err := a.ReserveSend(uuid.New(), [32]byte{8}, verification.SMS, testNow.Add(6*time.Second)); err == nil {
		t.Fatal("consumed sent")
	}
	if err := a.Cancel(testNow.Add(6 * time.Second)); err == nil {
		t.Fatal("consumed cancelled")
	}
	if err := a.Expire(a.ExpiresAt); err == nil {
		t.Fatal("consumed expired")
	}
	if !reflect.DeepEqual(before, a) {
		t.Fatal("terminal changed")
	}
}

func TestAttemptUncertainReleaseAndLateOutcomes(t *testing.T) {
	a := fresh(t)
	old, err := a.ReserveSend(uuid.New(), [32]byte{5}, verification.WhatsApp, testNow)
	if err != nil {
		t.Fatal(err)
	}
	before := clone(a)
	if _, err = a.ReserveSend(uuid.New(), [32]byte{6}, verification.SMS, testNow.Add(time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptUncertain) {
		t.Fatal(err)
	}
	if err = a.ReleaseUncertain(old, testNow.Add(time.Second), 0); !errors.Is(err, personalErrors.ErrInvalidAuthAttempt) {
		t.Fatal(err)
	}
	if err = a.ReleaseUncertain(old, testNow.Add(29*time.Second), 30*time.Second); !errors.Is(err, personalErrors.ErrAuthAttemptCooldown) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, a) {
		t.Fatal("rejected operation changed attempt")
	}
	if err = a.ReleaseUncertain(old, testNow.Add(30*time.Second), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if a.State != Pending || a.ProviderOperation.ID != old.ID || a.Revision != 3 {
		t.Fatal("release erased fingerprint")
	}
	if err = a.ApplyOutcome(old, provider(verification.Pending), testNow.Add(30*time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptOperation) {
		t.Fatal("late send applied after release", err)
	}
	if _, err = a.ReserveSend(old.ID, old.RequestHash, verification.SMS, testNow.Add(30*time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptOperation) {
		t.Fatal("same op sent twice", err)
	}
	newToken, err := a.ReserveSend(uuid.New(), [32]byte{8}, verification.SMS, testNow.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ApplyOutcome(old, provider(verification.Pending), testNow.Add(31*time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptOperation) {
		t.Fatal("late send overwrote resend", err)
	}
	response := provider(verification.Pending)
	response.Channel = verification.SMS
	if err = a.ApplyOutcome(newToken, response, testNow.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	if a.Channel != verification.SMS || a.Validate() != nil {
		t.Fatal("invalid fallback state")
	}
}

func TestAttemptCheckLimitsAndUncertainty(t *testing.T) {
	a := sent(t)
	for _, max := range []int{0, -1, math.MaxInt32 + 1} {
		if _, err := a.ReserveCheck(uuid.New(), [32]byte{7}, testNow.Add(2*time.Second), max); !errors.Is(err, personalErrors.ErrInvalidAuthAttempt) {
			t.Fatal(err)
		}
	}
	first, err := a.ReserveCheck(uuid.New(), [32]byte{7}, testNow.Add(2*time.Second), 2)
	if err != nil {
		t.Fatal(err)
	}
	if a.Attempts != 1 || a.State != Uncertain {
		t.Fatal("check not durably counted")
	}
	if err = a.ReleaseUncertain(first, testNow.Add(12*time.Second), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	second, err := a.ReserveCheck(uuid.New(), [32]byte{8}, testNow.Add(13*time.Second), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ApplyOutcome(first, provider(verification.Approved), testNow.Add(14*time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptOperation) {
		t.Fatal("lost approval replay accepted", err)
	}
	if err = a.ApplyOutcome(second, provider(verification.Pending), testNow.Add(14*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ReserveCheck(uuid.New(), [32]byte{9}, testNow.Add(15*time.Second), 2); !errors.Is(err, personalErrors.ErrAuthAttemptCheckLimit) {
		t.Fatal(err)
	}
	if a.Attempts != 2 || a.State != Pending {
		t.Fatal("limit changed attempt")
	}
}

func TestAttemptRejectsUnboundProviderApproval(t *testing.T) {
	a := sent(t)
	token, err := a.ReserveCheck(uuid.New(), [32]byte{6}, testNow.Add(2*time.Second), 3)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*OperationToken, *verification.Result){
		"id": func(token *OperationToken, _ *verification.Result) { token.ID = uuid.New() }, "hash": func(token *OperationToken, _ *verification.Result) { token.RequestHash = [32]byte{8} }, "revision": func(token *OperationToken, _ *verification.Result) { token.Revision++ }, "kind": func(token *OperationToken, _ *verification.Result) { token.Kind = Send },
		"reference": func(_ *OperationToken, r *verification.Result) { r.Reference = "VE-other" }, "empty ref": func(_ *OperationToken, r *verification.Result) { r.Reference = "" }, "channel": func(_ *OperationToken, r *verification.Result) { r.Channel = verification.SMS }, "unknown state": func(_ *OperationToken, r *verification.Result) { r.State = "404" }, "future created": func(_ *OperationToken, r *verification.Result) { r.CreatedAt = testNow.Add(time.Hour) }, "missing created": func(_ *OperationToken, r *verification.Result) { r.CreatedAt = time.Time{} }, "expiry before creation": func(_ *OperationToken, r *verification.Result) { r.ExpiresAt = r.CreatedAt },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := clone(a)
			badToken := token
			response := provider(verification.Approved)
			change(&badToken, &response)
			if err := candidate.ApplyOutcome(badToken, response, testNow.Add(3*time.Second)); err == nil {
				t.Fatal("unbound approval accepted")
			}
			if !reflect.DeepEqual(a, candidate) {
				t.Fatal("bad response mutated challenge")
			}
		})
	}
	response := provider(verification.Approved)
	response.ExpiresAt = testNow.Add(3 * time.Second)
	if err := a.ApplyOutcome(token, response, testNow.Add(3*time.Second)); err != nil || a.State != Expired || a.VerifiedAt != nil || a.Validate() != nil {
		t.Fatal("provider deadline did not persist an expired attempt", err)
	}
	b := fresh(t)
	send, err := b.ReserveSend(uuid.New(), [32]byte{8}, verification.WhatsApp, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.ApplyOutcome(send, provider(verification.Approved), testNow.Add(time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptOperation) {
		t.Fatal("send approved possession", err)
	}
}

func TestAttemptTerminalAndExpiryNeverRevive(t *testing.T) {
	for _, state := range []verification.State{verification.Cancelled, verification.Failed, verification.AttemptsExhausted, verification.Expired} {
		t.Run(string(state), func(t *testing.T) {
			a := sent(t)
			token, err := a.ReserveCheck(uuid.New(), [32]byte{6}, testNow.Add(2*time.Second), 3)
			if err != nil {
				t.Fatal(err)
			}
			if err = a.ApplyOutcome(token, provider(state), testNow.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			before := clone(a)
			if _, err = a.ReserveSend(uuid.New(), [32]byte{9}, verification.SMS, testNow.Add(4*time.Second)); err == nil {
				t.Fatal("terminal revived")
			}
			if err = a.ReleaseUncertain(token, testNow.Add(time.Minute), time.Second); err == nil {
				t.Fatal("terminal released")
			}
			if !reflect.DeepEqual(before, a) {
				t.Fatal("terminal changed")
			}
		})
	}
	a := fresh(t)
	if err := a.Expire(a.ExpiresAt.Add(-time.Nanosecond)); err == nil {
		t.Fatal("expired early")
	}
	if err := a.Expire(a.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ReserveSend(uuid.New(), [32]byte{5}, verification.SMS, a.ExpiresAt); !errors.Is(err, personalErrors.ErrAuthAttemptExpired) {
		t.Fatal(err)
	}
	b := verified(t)
	if err := b.Cancel(testNow.Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if b.Validate() != nil {
		t.Fatal("cancelled verified attempt invalid")
	}
}

func TestAttemptDoesNotExtendDeadlineOrAliasRecoveryAccount(t *testing.T) {
	a := sent(t)
	deadline := a.ExpiresAt
	token, err := a.ReserveSend(uuid.New(), [32]byte{6}, verification.WhatsApp, testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	response := provider(verification.Pending)
	response.ExpiresAt = testNow.Add(20 * time.Minute)
	if err = a.ApplyOutcome(token, response, testNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !a.ExpiresAt.Equal(deadline) {
		t.Fatal("resend extended TTL")
	}
	b := verified(t)
	b.Purpose = Recovery
	actual := uuid.New()
	if err = b.BindAccount(actual, testNow.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if *b.AccountID != actual || b.ProposedAccountID == actual {
		t.Fatal("recovery used proposed account")
	}
	b.Revision = math.MaxInt64
	if err = b.Consume(uuid.New(), testNow.Add(5*time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptConflict) {
		t.Fatal("revision overflow", err)
	}
}

func TestAttemptAlreadyExpiredProviderReferenceIsDurablyTerminal(t *testing.T) {
	a := fresh(t)
	token, err := a.ReserveSend(uuid.New(), [32]byte{5}, verification.WhatsApp, testNow)
	if err != nil {
		t.Fatal(err)
	}
	response := provider(verification.Pending)
	response.CreatedAt = testNow.Add(-11 * time.Minute)
	response.ExpiresAt = testNow.Add(-time.Minute)
	if err = a.ApplyOutcome(token, response, testNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if a.State != Expired || a.VerifiedAt != nil || !a.ExpiresAt.Equal(response.ExpiresAt) || a.Validate() != nil {
		t.Fatal("reused expired provider reference was not recorded safely")
	}
}
