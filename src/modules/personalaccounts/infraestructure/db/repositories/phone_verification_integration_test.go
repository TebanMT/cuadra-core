//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	hashing "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/budget"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// Provider only is a double. Transactions, attempts, receipts, shared budgets
// and rollback checks run on restricted PostgreSQL, never a real Verify send.
type phoneProviderDouble struct {
	starts, checks, fetches atomic.Int32
	start                   func(context.Context, verification.StartRequest) (verification.Result, error)
	check                   func(context.Context, verification.CheckRequest) (verification.Result, error)
	fetch                   func(context.Context, verification.Binding) (verification.Result, error)
}

func (p *phoneProviderDouble) Start(ctx context.Context, in verification.StartRequest) (verification.Result, error) {
	p.starts.Add(1)
	if p.start == nil {
		panic("unexpected phone Start")
	}
	return p.start(ctx, in)
}
func (p *phoneProviderDouble) Check(ctx context.Context, in verification.CheckRequest) (verification.Result, error) {
	p.checks.Add(1)
	if p.check == nil {
		panic("unexpected phone Check")
	}
	return p.check(ctx, in)
}
func (p *phoneProviderDouble) Fetch(ctx context.Context, in verification.Binding) (verification.Result, error) {
	p.fetches.Add(1)
	if p.fetch == nil {
		panic("unexpected phone Fetch")
	}
	return p.fetch(ctx, in)
}

type phoneClock struct{ nanoseconds atomic.Int64 }

func (c *phoneClock) Now() time.Time   { return time.Unix(0, c.nanoseconds.Load()).UTC() }
func (c *phoneClock) Set(at time.Time) { c.nanoseconds.Store(at.UnixNano()) }

type phoneFixture struct {
	d      personalApp.StartAuthDependencies
	a      *attempt.AuthAttempt
	p      *phoneProviderDouble
	clock  *phoneClock
	origin string
}

func newPhoneFixture(t *testing.T, knownReference bool, max int) *phoneFixture {
	t.Helper()
	uow := attemptUOW(t)
	a := attemptFixture(t)
	persistAttempt(t, uow, a)
	token, err := a.ReserveSend(uuid.New(), [32]byte{5}, verification.WhatsApp, attemptNow)
	if err != nil {
		t.Fatal(err)
	}
	if knownReference {
		if err = a.ApplyOutcome(token, phoneResult(verification.Pending), attemptNow.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err = uow.Command(attemptContext(a), func(tx shared.Transaction) error { return NewAuthAttemptPostgresRepository().Save(tx, a.Binding, a, 1) }); err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte(uuid.NewString()))
	subjects, err := hashing.NewSubjects(key[:])
	if err != nil {
		t.Fatal(err)
	}
	rules, err := budget.NewRules(map[budget.Scope]budget.Limit{budget.Destination: {Window: time.Hour, MaxSend: max, MaxCheck: max}, budget.Origin: {Window: time.Hour, MaxSend: max, MaxCheck: max}, budget.Service: {Window: time.Hour, MaxSend: max, MaxCheck: max}})
	if err != nil {
		t.Fatal(err)
	}
	clock := &phoneClock{}
	clock.Set(attemptNow.Add(2 * time.Second))
	provider := &phoneProviderDouble{}
	return &phoneFixture{a: a, p: provider, clock: clock, origin: uuid.NewString(), d: personalApp.StartAuthDependencies{UoW: uow, Attempts: NewAuthAttemptPostgresRepository(), Receipts: NewAuthReceiptPostgresRepository(), Budgets: NewOTPBudgetRepository(), Rules: rules, Subjects: subjects, Provider: provider, ServiceIdentity: uuid.NewString(), ProviderValidity: 10 * time.Minute, Now: clock.Now}}
}
func phoneResult(state verification.State) verification.Result {
	return verification.Result{Reference: "VE-phone-test-double", Channel: verification.WhatsApp, State: state, CreatedAt: attemptNow, ExpiresAt: attemptNow.Add(5 * time.Minute)}
}
func (f *phoneFixture) input() personalApp.CheckPhoneVerificationInput {
	return personalApp.CheckPhoneVerificationInput{OperationID: uuid.New(), Binding: f.a.Binding, RequestHash: sha256.Sum256([]byte(uuid.NewString())), Code: "123456", Origin: f.origin}
}
func (f *phoneFixture) resend() personalApp.ResendAuthInput {
	return personalApp.ResendAuthInput{OperationID: uuid.New(), Binding: f.a.Binding, RequestHash: sha256.Sum256([]byte(uuid.NewString())), Channel: verification.SMS, Origin: f.origin}
}
func (f *phoneFixture) get(t *testing.T) *personalApp.GetAttempt {
	t.Helper()
	u, err := personalApp.NewGetAttempt(personalApp.GetAttemptDependencies{UoW: f.d.UoW, Attempts: f.d.Attempts, Provider: f.p, Now: f.clock.Now, ProviderTimeout: 20 * time.Second, ReconcileAfter: 25 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func (f *phoneFixture) resender(t *testing.T) *personalApp.ResendAuth {
	t.Helper()
	u, err := personalApp.NewResendAuth(f.d, personalApp.PhoneRetryPolicy{ProviderTimeout: 20 * time.Second, ReconcileAfter: 25 * time.Second, ResendCooldown: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func (f *phoneFixture) checker(t *testing.T) *personalApp.CheckPhoneVerification {
	t.Helper()
	u, err := personalApp.NewCheckPhoneVerification(f.d, 4)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func (f *phoneFixture) counts(t *testing.T, send, check int) {
	t.Helper()
	tx, err := f.d.UoW.Query(attemptContext(f.a))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(tx)
	for scope, value := range map[budget.Scope]string{budget.Destination: f.a.DestinationE164, budget.Origin: f.origin, budget.Service: f.d.ServiceIdentity} {
		hash, err := f.d.Subjects.Hash(scope, value)
		if err != nil {
			t.Fatal(err)
		}
		var counts struct {
			Send  int
			Check int
		}
		err = tx.(*shared.GormTransaction).Tx.Raw(`SELECT coalesce(sum(send_count),0) AS send,coalesce(sum(check_count),0) AS check FROM mancuerna.otp_budgets WHERE scope=? AND subject_hash=?`, string(scope), hash[:]).Scan(&counts).Error
		if err != nil {
			t.Fatal(err)
		}
		if counts.Send != send || counts.Check != check {
			t.Fatalf("%s budget: sends=%d checks=%d, want %d/%d", scope, counts.Send, counts.Check, send, check)
		}
	}
}
func (f *phoneFixture) assertDurableOutsideTransaction(t *testing.T, operation uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(attemptContext(f.a), time.Second)
	defer cancel()
	err := f.d.UoW.Command(ctx, func(tx shared.Transaction) error {
		current, err := f.d.Attempts.GetForUpdate(tx, f.a.Binding)
		if err != nil {
			return err
		}
		if current.State != attempt.Uncertain || current.ProviderOperation.ID != operation {
			return errors.New("provider called without durable operation")
		}
		_, err = NewAuthReceiptPostgresRepository().Get(tx, receipt.Owner{AttemptID: f.a.ID}, operation)
		return err
	})
	if err != nil {
		t.Errorf("provider called with uncommitted state or held transaction: %v", err)
	}
}

func TestPhoneCheckPostgresDirectProofIdempotencyAndDetachedCommit(t *testing.T) {
	f := newPhoneFixture(t, true, 20)
	input := f.input()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.p.check = func(_ context.Context, in verification.CheckRequest) (verification.Result, error) {
		f.assertDurableOutsideTransaction(t, input.OperationID)
		if in.Reference != f.a.ProviderReference || in.DestinationE164 != f.a.DestinationE164 {
			t.Error("unbound check")
		}
		cancel()
		return phoneResult(verification.Approved), nil
	}
	u := f.checker(t)
	current, err := u.Execute(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != attempt.Verified || current.AccountID != nil || current.ResultReference != nil || current.ConsumedAt != nil {
		t.Fatal("OTP issued account access")
	}
	for i := 0; i < 3; i++ {
		current, err = u.Execute(context.Background(), input)
		if err != nil || current.State != attempt.Verified {
			t.Fatal(err)
		}
	}
	changed := input
	changed.RequestHash = [32]byte{9}
	if _, err = u.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("changed retry accepted", err)
	}
	wrong := input
	wrong.Binding.ContinuationHash = [32]byte{9}
	if _, err = u.Execute(context.Background(), wrong); !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
		t.Fatal("wrong continuation accepted", err)
	}
	if f.p.checks.Load() != 1 || f.p.starts.Load() != 0 || f.p.fetches.Load() != 0 {
		t.Fatal("retry repeated a provider action")
	}
	f.counts(t, 0, 1)
	q, err := f.d.UoW.Query(attemptContext(f.a))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	r, err := f.d.Receipts.Get(q, receipt.Owner{AttemptID: f.a.ID}, input.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Response.Ciphertext) != 0 || len(r.ResultSecretHash) != 0 || r.ResultReference != f.a.ID {
		t.Fatal("check receipt stored response secrets")
	}
}

func TestPhoneGetPostgresCannotCreditFetchApprovalOrMissing(t *testing.T) {
	f := newPhoneFixture(t, true, 20)
	input := f.input()
	f.p.check = func(context.Context, verification.CheckRequest) (verification.Result, error) {
		return verification.Result{}, &verification.Failure{Kind: verification.Uncertain}
	}
	current, err := f.checker(t).Execute(context.Background(), input)
	if err != nil || current.State != attempt.Uncertain {
		t.Fatal(err)
	}
	get := f.get(t)
	f.p.fetch = func(context.Context, verification.Binding) (verification.Result, error) {
		return phoneResult(verification.Approved), nil
	}
	if current, err = get.Execute(context.Background(), f.a.Binding); err != nil || current.State != attempt.Uncertain || f.p.fetches.Load() != 0 {
		t.Fatal("polled before provider timeout horizon", err)
	}
	f.clock.Set(attemptNow.Add(27 * time.Second))
	current, err = get.Execute(context.Background(), f.a.Binding)
	if err != nil || current.State != attempt.Uncertain || current.VerifiedAt != nil {
		t.Fatal("Fetch approval authorized this attempt", err)
	}
	f.p.fetch = func(context.Context, verification.Binding) (verification.Result, error) {
		return verification.Result{}, &verification.Failure{Kind: verification.Missing, HTTPStatus: 404}
	}
	current, err = get.Execute(context.Background(), f.a.Binding)
	if err != nil || current.State != attempt.Uncertain || current.VerifiedAt != nil {
		t.Fatal("404 authorized this attempt", err)
	}
	f.p.fetch = func(context.Context, verification.Binding) (verification.Result, error) {
		return phoneResult(verification.Pending), nil
	}
	current, err = get.Execute(context.Background(), f.a.Binding)
	if err != nil || current.State != attempt.Pending || current.VerifiedAt != nil {
		t.Fatal("pending reconciliation failed", err)
	}
	if f.p.starts.Load() != 0 || f.p.checks.Load() != 1 || f.p.fetches.Load() != 3 {
		t.Fatal("poll sent or checked again")
	}
	f.counts(t, 0, 1)
}

func TestPhoneResendPostgresExplicitCooldownFallbackAndBudgetRollback(t *testing.T) {
	f := newPhoneFixture(t, false, 20)
	u := f.resender(t)
	input := f.resend()
	f.p.start = func(_ context.Context, in verification.StartRequest) (verification.Result, error) {
		f.assertDurableOutsideTransaction(t, input.OperationID)
		if in.DestinationE164 != f.a.DestinationE164 || in.Channel != verification.SMS {
			t.Error("fallback changed destination")
		}
		result := phoneResult(verification.Pending)
		result.Channel = in.Channel
		return result, nil
	}
	if _, err := u.Execute(context.Background(), input); !errors.Is(err, personalErrors.ErrAuthAttemptCooldown) {
		t.Fatal("cooldown bypassed", err)
	}
	f.counts(t, 0, 0)
	f.clock.Set(attemptNow.Add(30 * time.Second))
	current, err := u.Execute(context.Background(), input)
	if err != nil || current.State != attempt.Pending || current.Channel != verification.SMS {
		t.Fatal("explicit fallback failed", err)
	}
	if _, err = u.Execute(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if f.p.starts.Load() != 1 || f.p.checks.Load() != 0 || f.p.fetches.Load() != 0 {
		t.Fatal("ambiguous send automatically repeated")
	}
	f.counts(t, 1, 0)
	current = readAttempt(t, f.d.UoW, f.a)
	if current.Candidate != f.a.Candidate || current.Nonce != f.a.Nonce || current.DestinationE164 != f.a.DestinationE164 || current.RequestHash != f.a.RequestHash {
		t.Fatal("resend replaced original binding")
	}
}

type phoneReceiptBarrier struct {
	repository.AuthReceiptRepository
	calls                       atomic.Int32
	readBarrier, commandBarrier chan struct{}
}

func (r *phoneReceiptBarrier) Get(tx shared.Transaction, owner receipt.Owner, id uuid.UUID) (*receipt.Receipt, error) {
	result, err := r.AuthReceiptRepository.Get(tx, owner, id)
	n := r.calls.Add(1)
	if n <= 2 {
		if n == 2 {
			close(r.readBarrier)
		}
		<-r.readBarrier
	} else if n <= 4 {
		if n == 4 {
			close(r.commandBarrier)
		}
		<-r.commandBarrier
	}
	return result, err
}
func TestPhoneCheckPostgresRacedReceiptRollsBackExtraBudget(t *testing.T) {
	f := newPhoneFixture(t, true, 20)
	input := f.input()
	f.p.check = func(context.Context, verification.CheckRequest) (verification.Result, error) {
		return phoneResult(verification.Pending), nil
	}
	f.d.Receipts = &phoneReceiptBarrier{AuthReceiptRepository: f.d.Receipts, readBarrier: make(chan struct{}), commandBarrier: make(chan struct{})}
	u := f.checker(t)
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := u.Execute(context.Background(), input); done <- err }()
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent receipt/budget deadlock")
		}
	}
	if f.p.checks.Load() != 1 {
		t.Fatal("raced exact check called provider twice")
	}
	f.counts(t, 0, 1)
}

func TestPhoneCheckPostgresBudgetExhaustionRollsBackAttemptAndReceipt(t *testing.T) {
	f := newPhoneFixture(t, true, 1)
	f.p.check = func(context.Context, verification.CheckRequest) (verification.Result, error) {
		return phoneResult(verification.Pending), nil
	}
	u := f.checker(t)
	first := f.input()
	if _, err := u.Execute(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	before := readAttempt(t, f.d.UoW, f.a)
	next := f.input()
	if _, err := u.Execute(context.Background(), next); !errors.Is(err, personalErrors.ErrBudgetExceeded) {
		t.Fatal("check budget exceeded", err)
	}
	after := readAttempt(t, f.d.UoW, f.a)
	if after.Revision != before.Revision || after.Attempts != before.Attempts {
		t.Fatal("rejected budget changed attempt")
	}
	q, err := f.d.UoW.Query(attemptContext(f.a))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	if _, err = f.d.Receipts.Get(q, receipt.Owner{AttemptID: f.a.ID}, next.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
		t.Fatal("rejected check left receipt", err)
	}
	if f.p.checks.Load() != 1 {
		t.Fatal("budget rejection still called provider")
	}
	f.counts(t, 0, 1)
}

func TestPhoneResendPostgresFetchBeforeExplicitReleaseAndLateCheckIgnored(t *testing.T) {
	f := newPhoneFixture(t, true, 20)
	input := f.input()
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	f.p.check = func(context.Context, verification.CheckRequest) (verification.Result, error) {
		close(started)
		<-release
		return phoneResult(verification.Approved), nil
	}
	u := f.checker(t)
	go func() { _, err := u.Execute(context.Background(), input); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Check did not start")
	}
	f.clock.Set(attemptNow.Add(32 * time.Second))
	resend := f.resend()
	f.p.fetch = func(context.Context, verification.Binding) (verification.Result, error) {
		return phoneResult(verification.Approved), nil
	}
	f.p.start = func(_ context.Context, in verification.StartRequest) (verification.Result, error) {
		if f.p.fetches.Load() != 1 {
			t.Error("resend did not reconcile known SID")
		}
		result := phoneResult(verification.Pending)
		result.Channel = in.Channel
		return result, nil
	}
	current, err := f.resender(t).Execute(context.Background(), resend)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("late Check did not finish")
	}
	current = readAttempt(t, f.d.UoW, f.a)
	if current.State != attempt.Pending || current.VerifiedAt != nil || current.ProviderOperation.ID != resend.OperationID {
		t.Fatal("late approved Check overwrote explicit resend")
	}
	if f.p.checks.Load() != 1 || f.p.starts.Load() != 1 || f.p.fetches.Load() != 1 {
		t.Fatal("unexpected provider action count")
	}
	f.counts(t, 1, 1)
}

func TestPhoneGetPostgresStaleFetchCannotOverwriteResend(t *testing.T) {
	f := newPhoneFixture(t, true, 20)
	f.p.check = func(context.Context, verification.CheckRequest) (verification.Result, error) {
		return verification.Result{}, &verification.Failure{Kind: verification.Uncertain}
	}
	if _, err := f.checker(t).Execute(context.Background(), f.input()); err != nil {
		t.Fatal(err)
	}
	f.clock.Set(attemptNow.Add(32 * time.Second))
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	f.p.fetch = func(context.Context, verification.Binding) (verification.Result, error) {
		if f.p.fetches.Load() == 1 {
			close(started)
			<-release
			return phoneResult(verification.Pending), nil
		}
		return phoneResult(verification.Approved), nil
	}
	get := f.get(t)
	go func() { _, err := get.Execute(context.Background(), f.a.Binding); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Fetch did not start")
	}
	f.p.start = func(_ context.Context, in verification.StartRequest) (verification.Result, error) {
		result := phoneResult(verification.Pending)
		result.Channel = in.Channel
		return result, nil
	}
	input := f.resend()
	if _, err := f.resender(t).Execute(context.Background(), input); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("late Fetch did not finish")
	}
	current := readAttempt(t, f.d.UoW, f.a)
	if current.State != attempt.Pending || current.Channel != verification.SMS || current.ProviderOperation.ID != input.OperationID || current.VerifiedAt != nil {
		t.Fatal("stale Fetch changed newer operation")
	}
	if f.p.starts.Load() != 1 || f.p.fetches.Load() != 2 || f.p.checks.Load() != 1 {
		t.Fatal("unexpected action count")
	}
	f.counts(t, 1, 1)
}

type phoneFailReceipt struct {
	repository.AuthReceiptRepository
	failure error
}

func (r *phoneFailReceipt) Create(shared.Transaction, *receipt.Receipt) error { return r.failure }
func TestPhoneCheckPostgresReceiptFailureAndMandatoryConfiguration(t *testing.T) {
	f := newPhoneFixture(t, true, 20)
	if _, err := personalApp.NewCheckPhoneVerification(f.d, 0); !errors.Is(err, personalErrors.ErrVerificationConfiguration) {
		t.Fatal("missing attempt check limit", err)
	}
	if _, err := personalApp.NewGetAttempt(personalApp.GetAttemptDependencies{UoW: f.d.UoW, Attempts: f.d.Attempts, Provider: f.p, Now: f.clock.Now, ProviderTimeout: 20 * time.Second, ReconcileAfter: 19 * time.Second}); !errors.Is(err, personalErrors.ErrVerificationConfiguration) {
		t.Fatal("reconciliation before provider timeout allowed", err)
	}
	if _, err := personalApp.NewResendAuth(f.d, personalApp.PhoneRetryPolicy{ProviderTimeout: 20 * time.Second, ReconcileAfter: 25 * time.Second}); !errors.Is(err, personalErrors.ErrVerificationConfiguration) {
		t.Fatal("missing resend cooldown allowed", err)
	}
	noRules := f.d
	noRules.Rules = budget.Rules{}
	if _, err := personalApp.NewCheckPhoneVerification(noRules, 4); !errors.Is(err, personalErrors.ErrBudgetConfiguration) {
		t.Fatal("missing shared budgets allowed", err)
	}
	before := readAttempt(t, f.d.UoW, f.a)
	failure := errors.New("synthetic receipt failure")
	f.d.Receipts = &phoneFailReceipt{AuthReceiptRepository: f.d.Receipts, failure: failure}
	input := f.input()
	if _, err := f.checker(t).Execute(context.Background(), input); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	after := readAttempt(t, f.d.UoW, f.a)
	if after.Revision != before.Revision || after.Attempts != before.Attempts || after.State != before.State {
		t.Fatal("receipt failure left durable provider intent")
	}
	if f.p.checks.Load() != 0 {
		t.Fatal("receipt failure still checked OTP")
	}
	f.counts(t, 0, 0)
}
