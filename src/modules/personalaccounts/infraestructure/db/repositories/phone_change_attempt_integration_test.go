//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func phoneChangeAttemptDependencies(f *phoneChangeFixture) personalApp.PhoneChangeAttemptDependencies {
	return personalApp.PhoneChangeAttemptDependencies{Phone: f.d, Resolver: NewPhoneChangeAttemptResolver(), Policy: personalApp.PhoneRetryPolicy{ProviderTimeout: time.Millisecond, ReconcileAfter: 2 * time.Millisecond, ResendCooldown: 5 * time.Millisecond}}
}
func phoneChangeGet(t *testing.T, d personalApp.PhoneChangeAttemptDependencies) *personalApp.GetPhoneChangeAttempt {
	t.Helper()
	u, err := personalApp.NewGetPhoneChangeAttempt(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func phoneChangeResend(t *testing.T, d personalApp.PhoneChangeAttemptDependencies) *personalApp.ResendPhoneChange {
	t.Helper()
	u, err := personalApp.NewResendPhoneChange(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func phoneChangeResendInput(start personalApp.StartPhoneChangeInput) personalApp.ResendAuthInput {
	return personalApp.ResendAuthInput{OperationID: uuid.New(), Binding: start.Binding, RequestHash: sha256.Sum256([]byte(uuid.NewString())), Channel: verification.SMS, Origin: start.Origin}
}

type unexpectedPhoneStatusFallback struct{}

func (unexpectedPhoneStatusFallback) Execute(context.Context, attempt.Binding) (personalApp.AccessResult, error) {
	return personalApp.AccessResult{}, errors.New("phone status reached access finalizer")
}

type unexpectedPhoneResendFallback struct{}

func (unexpectedPhoneResendFallback) Execute(context.Context, personalApp.ResendAuthInput) (*attempt.AuthAttempt, error) {
	return nil, errors.New("phone resend reached prelogin producer")
}

func TestPhoneChangeAttemptPostgresUnknownSendExplicitSMSAndLimitedStatus(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in := f.startInput(t, accessTestPhone())
	normalStart := f.p.start
	f.p.start = func(context.Context, verification.StartRequest) (verification.Result, error) {
		return verification.Result{}, personalErrors.ErrVerificationUncertain
	}
	a, err := phoneChangeStart(t, f.d).Execute(context.Background(), in)
	if err != nil || a.State != attempt.Uncertain || a.ProviderReference != "" {
		t.Fatal("unknown send not durable")
	}
	d := phoneChangeAttemptDependencies(f)
	get, err := personalApp.NewGetAuthStatusRouter(f.d.Phone.UoW, f.d.Phone.Attempts, unexpectedPhoneStatusFallback{}, phoneChangeGet(t, d))
	if err != nil {
		t.Fatal(err)
	}
	view, err := get.Execute(context.Background(), in.Binding)
	if err != nil || view.Attempt.State != attempt.Uncertain || view.Authorized != nil || view.AuthorizationRequired {
		t.Fatalf("limited status failed: %v", err)
	}
	if f.p.fetches.Load() != 0 || f.p.starts.Load() != 1 {
		t.Fatal("GET sent or fetched without provider reference")
	}
	time.Sleep(10 * time.Millisecond)
	f.p.start = func(ctx context.Context, r verification.StartRequest) (verification.Result, error) {
		out, err := normalStart(ctx, r)
		out.Channel = r.Channel
		return out, err
	}
	request := phoneChangeResendInput(in)
	resend, err := personalApp.NewResendAuthRouter(f.d.Phone.UoW, f.d.Phone.Attempts, unexpectedPhoneResendFallback{}, phoneChangeResend(t, d))
	if err != nil {
		t.Fatal(err)
	}
	a, err = resend.Execute(context.Background(), request)
	if err != nil || a.State != attempt.Pending || a.Channel != verification.SMS || a.ID != in.Binding.ID {
		t.Fatalf("explicit SMS failed: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err = resend.Execute(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatalf("initial receipt stopped replaying after channel change: %v", err)
	}
	f.p.check = func(ctx context.Context, r verification.CheckRequest) (verification.Result, error) {
		out, _ := normalStart(ctx, verification.StartRequest{})
		out.Channel = verification.SMS
		out.State = verification.Approved
		return out, nil
	}
	complete, _ := f.completeInput(t, in)
	if _, err = phoneChangeComplete(t, f.d).Execute(context.Background(), complete); err != nil {
		t.Fatal(err)
	}
	view, err = get.Execute(context.Background(), in.Binding)
	if err != nil || view.Attempt.State != attempt.Consumed || view.Authorized != nil || view.AuthorizationRequired || view.StateOverride != "" {
		t.Fatalf("status exposed sensitive result: %v", err)
	}
	f.counts(t, in, 2, 1)
	if f.p.starts.Load() != 2 || f.p.checks.Load() != 1 {
		t.Fatal("limited retry duplicated provider calls")
	}
	check, err := personalApp.NewCheckPhoneVerification(f.d.Phone, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = check.Execute(context.Background(), personalApp.CheckPhoneVerificationInput{OperationID: uuid.New(), Binding: in.Binding, RequestHash: [32]byte{1}, Code: "123456", Origin: in.Origin}); !errors.Is(err, personalErrors.ErrAuthAttemptState) {
		t.Fatal("prelogin auth/check accepted phone change")
	}
}

func TestPhoneChangeAttemptPostgresFetchNeverApprovesAndNewCheckRequiresProof(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in := f.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	check, proofID := f.completeInput(t, in)
	normalCheck := f.p.check
	f.p.check = func(context.Context, verification.CheckRequest) (verification.Result, error) {
		return verification.Result{}, personalErrors.ErrVerificationUncertain
	}
	if _, err := phoneChangeComplete(t, f.d).Execute(context.Background(), check); !errors.Is(err, personalErrors.ErrAuthAttemptUncertain) {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	get := phoneChangeGet(t, phoneChangeAttemptDependencies(f))
	f.p.fetch = func(ctx context.Context, b verification.Binding) (verification.Result, error) {
		return normalCheck(ctx, verification.CheckRequest{Binding: b})
	}
	view, err := get.Execute(context.Background(), in.Binding)
	if err != nil || view.Attempt.State != attempt.Uncertain || view.Authorized != nil {
		t.Fatal("Fetch approved granted OTP authority")
	}
	f.p.fetch = func(context.Context, verification.Binding) (verification.Result, error) {
		return verification.Result{}, personalErrors.ErrVerificationMissing
	}
	view, err = get.Execute(context.Background(), in.Binding)
	if err != nil || view.Attempt.State != attempt.Uncertain {
		t.Fatal("404 became approval")
	}
	f.p.fetch = func(ctx context.Context, b verification.Binding) (verification.Result, error) {
		out, err := normalCheck(ctx, verification.CheckRequest{Binding: b})
		out.State = verification.Pending
		return out, err
	}
	view, err = get.Execute(context.Background(), in.Binding)
	if err != nil || view.Attempt.State != attempt.Pending {
		t.Fatalf("pending reconciliation failed: %v", err)
	}
	if proofTestGet(t, f.startProof, f.actor, proofID).ConsumedAt != nil {
		t.Fatal("status consumed proof")
	}
	if _, err = phoneChangeComplete(t, f.d).Execute(context.Background(), check); !errors.Is(err, personalErrors.ErrVerificationRejected) {
		t.Fatalf("old operation checked twice: %v", err)
	}
	f.p.check = normalCheck
	next, _ := f.completeInput(t, in)
	if _, err = phoneChangeComplete(t, f.d).Execute(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	f.counts(t, in, 1, 2)
	if f.p.checks.Load() != 2 || f.p.fetches.Load() != 3 {
		t.Fatal("unexpected reconciliation/check count")
	}
}

func TestPhoneChangeAttemptPostgresBindingScopeAndRevocationBeforeProvider(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in := f.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	d := phoneChangeAttemptDependencies(f)
	get, resend := phoneChangeGet(t, d), phoneChangeResend(t, d)
	request := phoneChangeResendInput(in)
	for _, ctx := range []context.Context{shared.WithPersonalAccount(context.Background(), f.actor.AccountID), shared.WithPersonalAttempt(context.Background(), in.Binding.ID, in.Binding.ContinuationHash), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := get.Execute(ctx, in.Binding); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("ambient scope accepted by GET")
		}
		if _, err := resend.Execute(ctx, request); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("ambient scope accepted by resend")
		}
	}
	wrong := in.Binding
	wrong.ContinuationHash[0]++
	if _, err := get.Execute(context.Background(), wrong); !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
		t.Fatalf("wrong continuation resolved actor: %v", err)
	}
	other := f.otherSession(t)
	if err := f.owner.Exec(`UPDATE mancuerna.auth_receipts SET result_reference=? WHERE operation_id=?`, other.SessionID, in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := get.Execute(context.Background(), in.Binding); !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
		t.Fatalf("SID substitution retained binding: %v", err)
	}
	if err := f.owner.Exec(`UPDATE mancuerna.auth_receipts SET result_reference=? WHERE operation_id=?`, f.actor.SessionID, in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.d.Phone.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := get.Execute(context.Background(), in.Binding); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("revoked initiator queried attempt")
	}
	if _, err := resend.Execute(context.Background(), request); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("revoked initiator resent")
	}
	if f.p.starts.Load() != 1 || f.p.fetches.Load() != 0 || f.p.checks.Load() != 0 {
		t.Fatal("rejected scope reached provider")
	}
}

func TestPhoneChangeAttemptPostgresConcurrentResendAndLateRevocation(t *testing.T) {
	f := newPhoneChangeFixture(t, 2)
	in := f.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	d := phoneChangeAttemptDependencies(f)
	resend := phoneChangeResend(t, d)
	request := phoneChangeResendInput(in)
	normal := f.p.start
	f.p.start = func(ctx context.Context, r verification.StartRequest) (verification.Result, error) {
		out, err := normal(ctx, r)
		out.Channel = r.Channel
		return out, err
	}
	f.p.fetch = func(context.Context, verification.Binding) (verification.Result, error) {
		return verification.Result{}, personalErrors.ErrVerificationMissing
	}
	const count = 5
	errs := make([]error, count)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-gate; _, errs[i] = resend.Execute(context.Background(), request) }(i)
	}
	close(gate)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.counts(t, in, 2, 0)
	if f.p.starts.Load() != 2 {
		t.Fatal("concurrent explicit resend sent twice")
	}
	// A later Fetch cannot write even metadata after its initiating session is
	// withdrawn while the provider call is outside the transaction.
	complete, _ := f.completeInput(t, in)
	f.p.check = func(context.Context, verification.CheckRequest) (verification.Result, error) {
		return verification.Result{}, personalErrors.ErrVerificationUncertain
	}
	if _, err := phoneChangeComplete(t, f.d).Execute(context.Background(), complete); !errors.Is(err, personalErrors.ErrAuthAttemptUncertain) {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	f.p.fetch = func(ctx context.Context, b verification.Binding) (verification.Result, error) {
		if err := f.d.Phone.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
			return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now())
		}); err != nil {
			return verification.Result{}, err
		}
		out, err := normal(ctx, verification.StartRequest{})
		out.Channel = verification.SMS
		return out, err
	}
	if _, err := phoneChangeGet(t, d).Execute(context.Background(), in.Binding); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatalf("late revoked outcome persisted: %v", err)
	}
	if f.attempt(t, in.Binding).State != attempt.Uncertain {
		t.Fatal("late outcome revived verification")
	}
}
