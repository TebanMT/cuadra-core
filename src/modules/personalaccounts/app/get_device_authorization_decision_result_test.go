package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type decisionResultUoW struct {
	shared.UnitOfWork
	calls int
}

func (u *decisionResultUoW) Command(_ context.Context, fn func(shared.Transaction) error) error {
	u.calls++
	return fn(nil)
}

type decisionResultSessions struct {
	repository.AuthSessionRepository
	current     security.AccessClaims
	original    *session.Session
	active      bool
	getErr      error
	databaseNow time.Time
	expiryErr   error
	calls       *[]string
}

func (s *decisionResultSessions) SessionActive(_ shared.Transaction, account, sid, did uuid.UUID) (bool, error) {
	*s.calls = append(*s.calls, "active")
	return s.active && account == s.current.AccountID && sid == s.current.SessionID && did == s.current.DeviceID, nil
}
func (s *decisionResultSessions) GetSession(_ shared.Transaction, account, sid uuid.UUID) (*session.Session, error) {
	*s.calls = append(*s.calls, "original")
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.original == nil || s.original.AccountID != account || s.original.ID != sid {
		return nil, personalErrors.ErrAuthObjectNotFound
	}
	return s.original, nil
}
func (s *decisionResultSessions) SessionExpired(_ shared.Transaction, account, sid uuid.UUID) (bool, error) {
	*s.calls = append(*s.calls, "expired")
	if s.expiryErr != nil {
		return false, s.expiryErr
	}
	if s.original == nil || s.original.AccountID != account || s.original.ID != sid {
		return false, personalErrors.ErrAuthObjectNotFound
	}
	return !s.databaseNow.Before(s.original.ExpiresAt), nil
}

type decisionResultReceipts struct {
	repository.AuthReceiptRepository
	value *receipt.Receipt
	err   error
	calls *[]string
}

func (r *decisionResultReceipts) Get(_ shared.Transaction, owner receipt.Owner, op uuid.UUID) (*receipt.Receipt, error) {
	*r.calls = append(*r.calls, "receipt")
	if r.err != nil {
		return nil, r.err
	}
	if r.value == nil {
		return nil, personalErrors.ErrReceiptNotFound
	}
	return r.value, nil
}

type decisionResultFixture struct {
	deps     GetDeviceAuthorizationDecisionResultDependencies
	uow      *decisionResultUoW
	sessions *decisionResultSessions
	receipts *decisionResultReceipts
	calls    *[]string
	current  security.AccessClaims
	in       GetDeviceAuthorizationDecisionResultInput
	now      time.Time
}

func newDecisionResultFixture(t *testing.T) decisionResultFixture {
	t.Helper()
	now := time.Date(2026, 10, 22, 18, 0, 0, 123000000, time.UTC)
	account := uuid.New()
	original, err := session.NewSession(account, uuid.New(), uuid.New(), [32]byte{1}, uuid.New(), [32]byte{2}, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	current := security.AccessClaims{AccountID: account, SessionID: uuid.New(), DeviceID: uuid.New()}
	in := GetDeviceAuthorizationDecisionResultInput{AuthorizationID: uuid.New(), OperationID: uuid.New(), OriginalSessionID: original.ID, OriginalDeviceID: original.DeviceID, Decision: deviceauthorization.Approve, RequestHash: [32]byte{8}}
	calls := []string{}
	s := &decisionResultSessions{current: current, original: original, active: true, databaseNow: now, calls: &calls}
	r := &decisionResultReceipts{calls: &calls}
	uow := &decisionResultUoW{}
	return decisionResultFixture{deps: GetDeviceAuthorizationDecisionResultDependencies{UoW: uow, Sessions: s, Receipts: r, Now: func() time.Time { return now }}, uow: uow, sessions: s, receipts: r, calls: &calls, current: current, in: in, now: now}
}
func (f decisionResultFixture) committed(t *testing.T) *receipt.Receipt {
	t.Helper()
	r, err := receipt.New(f.in.OperationID, receipt.Owner{AccountID: f.current.AccountID}, deviceAuthorizationDecisionPurpose, f.in.RequestHash, f.now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r.ResultReference = f.in.AuthorizationID
	digest := authorizationActorDigest(security.AccessClaims{AccountID: f.current.AccountID, SessionID: f.in.OriginalSessionID, DeviceID: f.in.OriginalDeviceID}, f.in.AuthorizationID, f.in.OperationID)
	r.ResultSecretHash = append([]byte(nil), digest[:]...)
	f.receipts.value = r
	return r
}
func (f decisionResultFixture) execute(t *testing.T) (DeviceAuthorizationDecisionResult, error) {
	t.Helper()
	u, err := NewGetDeviceAuthorizationDecisionResult(f.deps)
	if err != nil {
		t.Fatal(err)
	}
	return u.Execute(context.Background(), f.current, f.in)
}
func TestDeviceAuthorizationDecisionResultCompletedUsesDurableReceiptBeforeOriginalSession(t *testing.T) {
	for _, decision := range []deviceauthorization.Decision{deviceauthorization.Approve, deviceauthorization.Deny} {
		t.Run(string(decision), func(t *testing.T) {
			f := newDecisionResultFixture(t)
			f.in.Decision = decision
			r := f.committed(t)
			f.sessions.original = nil // The original SID and response envelope need not remain readable.
			before := *r
			out, err := f.execute(t)
			if err != nil || out.Status != DeviceAuthorizationDecisionCompleted || out.CompletedAt == nil || *out.CompletedAt != accessTimestamp(r.CompletedAt) {
				t.Fatalf("receipt result: %+v, %v", out, err)
			}
			if out.AccountID != f.current.AccountID.String() || out.AuthorizationID != f.in.AuthorizationID.String() || out.OperationID != f.in.OperationID.String() || out.OriginalSessionID != f.in.OriginalSessionID.String() || out.OriginalDeviceID != f.in.OriginalDeviceID.String() || out.Decision != decision || out.RequestHash != hex.EncodeToString(f.in.RequestHash[:]) || out.CheckedAt != accessTimestamp(f.now) {
				t.Fatal("result identity or clock changed")
			}
			if !reflect.DeepEqual(*f.calls, []string{"active", "receipt"}) || !reflect.DeepEqual(before, *r) || f.uow.calls != 1 {
				t.Fatal("reader consulted or mutated effect state")
			}
			b, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if json.Unmarshal(b, &fields) != nil || len(fields) != 10 {
				t.Fatal("response boundary changed")
			}
		})
	}
}
func TestDeviceAuthorizationDecisionResultAbsenceIsTerminalOnlyForOriginalSessionRevocationOrExpiry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*decisionResultFixture)
		want   DeviceAuthorizationDecisionResultStatus
	}{
		{"active", func(*decisionResultFixture) {}, DeviceAuthorizationDecisionUnresolved},
		{"expires exactly now", func(f *decisionResultFixture) { f.sessions.original.ExpiresAt = f.now }, DeviceAuthorizationDecisionNotApplied},
		{"expired", func(f *decisionResultFixture) { f.sessions.original.ExpiresAt = f.now.Add(-time.Hour) }, DeviceAuthorizationDecisionNotApplied},
		{"revoked", func(f *decisionResultFixture) { at := f.now.Add(-time.Hour); f.sessions.original.RevokedAt = &at }, DeviceAuthorizationDecisionNotApplied},
		// ActiveAt is false here, but a future clock/last-used discrepancy is not terminal authority.
		{"not active for nonterminal reason", func(f *decisionResultFixture) { at := f.now.Add(time.Hour); f.sessions.original.LastUsedAt = &at }, DeviceAuthorizationDecisionUnresolved},
		{"application clock ahead", func(f *decisionResultFixture) {
			f.now = f.now.Add(91 * 24 * time.Hour)
			at := f.now
			f.deps.Now = func() time.Time { return at }
		}, DeviceAuthorizationDecisionUnresolved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecisionResultFixture(t)
			tc.change(&f)
			out, err := f.execute(t)
			if err != nil || out.Status != tc.want || out.CompletedAt != nil || out.CheckedAt != accessTimestamp(f.now) {
				t.Fatalf("absence: %+v %v", out, err)
			}
			calls := []string{"active", "receipt", "original"}
			if f.sessions.original.RevokedAt == nil {
				calls = append(calls, "expired")
			}
			if !reflect.DeepEqual(*f.calls, calls) {
				t.Fatal("absence check order changed")
			}
			b, _ := json.Marshal(out)
			var fields map[string]any
			_ = json.Unmarshal(b, &fields)
			v, ok := fields["completed_at"]
			if !ok || v != nil {
				t.Fatal("completed_at must be present and null")
			}
		})
	}
}
func TestDeviceAuthorizationDecisionResultRejectsReceiptMismatchesAndCorruption(t *testing.T) {
	cases := []struct {
		name   string
		change func(*decisionResultFixture, *receipt.Receipt)
		want   error
	}{
		{"hash", func(f *decisionResultFixture, r *receipt.Receipt) { f.in.RequestHash[0] ^= 1 }, personalErrors.ErrIdempotencyMismatch},
		{"purpose", func(f *decisionResultFixture, r *receipt.Receipt) { r.Purpose = "other" }, personalErrors.ErrIdempotencyMismatch},
		{"authorization", func(f *decisionResultFixture, r *receipt.Receipt) { f.in.AuthorizationID = uuid.New() }, personalErrors.ErrIdempotencyMismatch},
		{"original session", func(f *decisionResultFixture, r *receipt.Receipt) { f.in.OriginalSessionID = uuid.New() }, personalErrors.ErrIdempotencyMismatch},
		{"original device", func(f *decisionResultFixture, r *receipt.Receipt) { f.in.OriginalDeviceID = uuid.New() }, personalErrors.ErrIdempotencyMismatch},
		{"owner", func(f *decisionResultFixture, r *receipt.Receipt) { r.Owner.AccountID = uuid.New() }, personalErrors.ErrInvalidReceipt},
		{"operation", func(f *decisionResultFixture, r *receipt.Receipt) { r.OperationID = uuid.New() }, personalErrors.ErrInvalidReceipt},
		{"missing actor digest", func(f *decisionResultFixture, r *receipt.Receipt) { r.ResultSecretHash = nil }, personalErrors.ErrInvalidReceipt},
		{"malformed digest", func(f *decisionResultFixture, r *receipt.Receipt) { r.ResultSecretHash = []byte{1} }, personalErrors.ErrInvalidReceipt},
		{"missing result reference", func(f *decisionResultFixture, r *receipt.Receipt) { r.ResultReference = uuid.Nil }, personalErrors.ErrInvalidReceipt},
		{"invalid completion", func(f *decisionResultFixture, r *receipt.Receipt) { r.CompletedAt = time.Time{} }, personalErrors.ErrInvalidReceipt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecisionResultFixture(t)
			r := f.committed(t)
			tc.change(&f, r)
			out, err := f.execute(t)
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(out, DeviceAuthorizationDecisionResult{}) {
				t.Fatalf("bad receipt: %+v %v", out, err)
			}
		})
	}
}
func TestDeviceAuthorizationDecisionResultRequiresLiveCurrentActorAndKnownOriginalOwner(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*decisionResultFixture)
		want   error
		calls  []string
	}{
		{"current revoked", func(f *decisionResultFixture) { f.committed(t); f.sessions.active = false }, personalErrors.ErrSessionRevoked, []string{"active"}},
		{"unknown original", func(f *decisionResultFixture) { f.sessions.original = nil }, personalErrors.ErrDeviceAuthorizationNotFound, []string{"active", "receipt", "original"}},
		{"wrong original device", func(f *decisionResultFixture) { f.in.OriginalDeviceID = uuid.New() }, personalErrors.ErrDeviceAuthorizationNotFound, []string{"active", "receipt", "original"}},
		{"other account", func(f *decisionResultFixture) { f.current.AccountID = uuid.New(); f.sessions.current = f.current }, personalErrors.ErrDeviceAuthorizationNotFound, []string{"active", "receipt", "original"}},
		{"corrupt original", func(f *decisionResultFixture) { f.sessions.original.Generation = 0 }, personalErrors.ErrInvalidSession, []string{"active", "receipt", "original"}},
		{"receipt storage error", func(f *decisionResultFixture) { f.receipts.err = personalErrors.ErrAuthObjectUnavailable }, personalErrors.ErrAuthObjectUnavailable, []string{"active", "receipt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDecisionResultFixture(t)
			tc.change(&f)
			_, err := f.execute(t)
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(*f.calls, tc.calls) {
				t.Fatalf("wrong authority/error: %v, %v", err, *f.calls)
			}
		})
	}
}
func TestDeviceAuthorizationDecisionResultValidatesBeforeIOAndRejectsMissingDependencies(t *testing.T) {
	for _, change := range []func(*GetDeviceAuthorizationDecisionResultDependencies){func(d *GetDeviceAuthorizationDecisionResultDependencies) { d.UoW = nil }, func(d *GetDeviceAuthorizationDecisionResultDependencies) { d.Sessions = nil }, func(d *GetDeviceAuthorizationDecisionResultDependencies) { d.Receipts = nil }, func(d *GetDeviceAuthorizationDecisionResultDependencies) { d.Now = nil }} {
		f := newDecisionResultFixture(t)
		change(&f.deps)
		if _, err := NewGetDeviceAuthorizationDecisionResult(f.deps); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
			t.Fatal(err)
		}
	}
	for _, change := range []func(*decisionResultFixture){func(f *decisionResultFixture) { f.current.DeviceID = uuid.Nil }, func(f *decisionResultFixture) { f.in.AuthorizationID = uuid.Nil }, func(f *decisionResultFixture) { f.in.OperationID = uuid.Nil }, func(f *decisionResultFixture) { f.in.OriginalSessionID = uuid.Nil }, func(f *decisionResultFixture) { f.in.OriginalDeviceID = uuid.Nil }, func(f *decisionResultFixture) { f.in.Decision = "unknown" }, func(f *decisionResultFixture) { f.in.RequestHash = [32]byte{} }} {
		f := newDecisionResultFixture(t)
		change(&f)
		if _, err := f.execute(t); err == nil || f.uow.calls != 0 {
			t.Fatal("invalid input reached IO")
		}
	}
}

func TestDeviceAuthorizationDecisionResultExpiryReadFailureIsInconclusive(t *testing.T) {
	for _, tc := range []struct{ got, want error }{{personalErrors.ErrAuthObjectNotFound, personalErrors.ErrDeviceAuthorizationNotFound}, {personalErrors.ErrAuthSessionScope, personalErrors.ErrAuthSessionScope}} {
		f := newDecisionResultFixture(t)
		f.sessions.expiryErr = tc.got
		out, err := f.execute(t)
		if !errors.Is(err, tc.want) || !reflect.DeepEqual(out, DeviceAuthorizationDecisionResult{}) {
			t.Fatalf("expiry failure became a terminal result: %+v %v", out, err)
		}
	}
}
