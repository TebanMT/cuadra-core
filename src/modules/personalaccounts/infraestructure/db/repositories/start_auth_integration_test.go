//go:build server && integration

package repositories

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	hashing "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/budget"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Only the external provider is a test double here. All intents, receipts,
// budgets and concurrent transactions use real restricted PostgreSQL pools.
type startProvider struct {
	calls    atomic.Int32
	callback func(verification.StartRequest) (verification.Result, error)
}

func (p *startProvider) Start(_ context.Context, in verification.StartRequest) (verification.Result, error) {
	p.calls.Add(1)
	return p.callback(in)
}
func (*startProvider) Check(context.Context, verification.CheckRequest) (verification.Result, error) {
	panic("not used by StartAuth")
}
func (*startProvider) Fetch(context.Context, verification.Binding) (verification.Result, error) {
	panic("not used by StartAuth")
}

func startInput(t *testing.T) personalApp.StartAuthInput {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return personalApp.StartAuthInput{OperationID: uuid.New(), Binding: attempt.Binding{ID: uuid.New(), ContinuationHash: sha256.Sum256([]byte(uuid.NewString()))}, RequestHash: sha256.Sum256([]byte(uuid.NewString())), ProposedAccountID: uuid.New(), Candidate: attempt.Candidate{ID: uuid.New(), PublicKey: [32]byte(pub), Platform: "android", Label: "Synthetic test"}, Purpose: attempt.Access, DestinationE164: "+525555555555", Channel: verification.SMS, Origin: "127.0.0.1"}
}
func startUseCase(t *testing.T, auth *gorm.DB, p verification.VerificationProvider, service string, max int) (*personalApp.StartAuth, shared.UnitOfWork) {
	t.Helper()
	uow := budgetTestUoW(t, auth)
	hasher, err := hashing.NewSubjects([]byte("synthetic-start-budget-key-at-least-32"))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := budget.NewRules(map[budget.Scope]budget.Limit{budget.Destination: {Window: time.Minute, MaxSend: max, MaxCheck: 20}, budget.Origin: {Window: time.Minute, MaxSend: max, MaxCheck: 20}, budget.Service: {Window: time.Minute, MaxSend: max, MaxCheck: 20}})
	if err != nil {
		t.Fatal(err)
	}
	useCase, err := personalApp.NewStartAuth(personalApp.StartAuthDependencies{UoW: uow, Attempts: NewAuthAttemptPostgresRepository(), Receipts: NewAuthReceiptPostgresRepository(), Budgets: NewOTPBudgetRepository(), Rules: rules, Subjects: hasher, Provider: p, ServiceIdentity: service, ProviderValidity: 10 * time.Minute, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return useCase, uow
}

func TestStartAuthPostgresConcurrentRetriesSendOnceAfterCommit(t *testing.T) {
	auth := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	input := startInput(t)
	input.Origin = uuid.NewString() // A distinct synthetic origin for this fixture.
	input.DestinationE164 = "+525555555501"
	provider := &startProvider{}
	useCase, uow := startUseCase(t, auth, provider, uuid.NewString(), 100)
	provider.callback = func(in verification.StartRequest) (verification.Result, error) {
		ctx, cancel := context.WithTimeout(shared.WithPersonalAttempt(context.Background(), input.Binding.ID, input.Binding.ContinuationHash), time.Second)
		defer cancel()
		q, err := uow.Query(ctx)
		if err != nil {
			return verification.Result{}, err
		}
		defer shared.CloseQuery(q)
		stored, err := NewAuthAttemptPostgresRepository().Get(q, input.Binding)
		if err != nil {
			return verification.Result{}, err
		}
		if stored.State != attempt.Uncertain || stored.ProviderOperation.ID != input.OperationID {
			t.Error("provider called before durable reservation")
		}
		if _, err = NewAuthReceiptPostgresRepository().Get(q, receipt.Owner{AttemptID: input.Binding.ID}, input.OperationID); err != nil {
			t.Error("provider called before durable receipt")
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		return verification.Result{Reference: "VE-synthetic", Channel: in.Channel, State: verification.Pending, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}, nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := useCase.Execute(context.Background(), input)
			if err == nil && a.ID != input.Binding.ID {
				err = errors.New("wrong attempt")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if provider.calls.Load() != 1 {
		t.Fatal("retry sent more than once")
	}
	a, err := useCase.Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != attempt.Pending || a.ProviderReference != "VE-synthetic" {
		t.Fatal("persisted provider outcome missing")
	}
	changed := input
	changed.RequestHash = sha256.Sum256([]byte("changed request"))
	if _, err = useCase.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("changed retry accepted")
	}
	wrong := input
	wrong.Binding.ContinuationHash = sha256.Sum256([]byte("wrong secret"))
	if _, err = useCase.Execute(context.Background(), wrong); err == nil {
		t.Fatal("wrong continuation accepted")
	}
	if provider.calls.Load() != 1 {
		t.Fatal("rejected request sent OTP")
	}
}

func TestStartAuthPostgresUncertainRestartAndBudgetRollback(t *testing.T) {
	auth := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	input := startInput(t)
	input.DestinationE164 = "+525555555502"
	input.Origin = uuid.NewString()
	provider := &startProvider{callback: func(verification.StartRequest) (verification.Result, error) {
		return verification.Result{}, &verification.Failure{Kind: verification.Uncertain}
	}}
	service := uuid.NewString()
	u, _ := startUseCase(t, auth, provider, service, 1)
	a, err := u.Execute(context.Background(), input)
	if err != nil || a.State != attempt.Uncertain {
		t.Fatal("uncertain outcome lost")
	}
	restarted, uow := startUseCase(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"), provider, service, 1)
	a, err = restarted.Execute(context.Background(), input)
	if err != nil || a.State != attempt.Uncertain {
		t.Fatal("restart lost intent")
	}
	other := startInput(t)
	other.DestinationE164 = input.DestinationE164
	other.Origin = input.Origin
	if _, err = restarted.Execute(context.Background(), other); !errors.Is(err, personalErrors.ErrBudgetExceeded) {
		t.Fatal("uncertain send refunded budget")
	}
	if provider.calls.Load() != 1 {
		t.Fatal("uncertain/rejected request resent")
	}
	q, err := uow.Query(shared.WithPersonalAttempt(context.Background(), other.Binding.ID, other.Binding.ContinuationHash))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	if _, err = NewAuthAttemptPostgresRepository().Get(q, other.Binding); !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
		t.Fatal("failed budget left attempt")
	}
	if _, err = NewAuthReceiptPostgresRepository().Get(q, receipt.Owner{AttemptID: other.Binding.ID}, other.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
		t.Fatal("failed budget left receipt")
	}
}
