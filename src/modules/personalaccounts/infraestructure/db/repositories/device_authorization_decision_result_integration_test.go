//go:build server && integration

package repositories

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func decisionResultTestReader(t *testing.T, f authorizationFixture) *personalApp.GetDeviceAuthorizationDecisionResult {
	t.Helper()
	u, err := personalApp.NewGetDeviceAuthorizationDecisionResult(personalApp.GetDeviceAuthorizationDecisionResultDependencies{UoW: f.d.UoW, Sessions: f.d.Sessions, Receipts: f.d.Receipts, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func decisionResultTestInput(actor security.AccessClaims, in personalApp.DecideDeviceAuthorizationInput) personalApp.GetDeviceAuthorizationDecisionResultInput {
	return personalApp.GetDeviceAuthorizationDecisionResultInput{AuthorizationID: in.AuthorizationID, OperationID: in.OperationID, OriginalSessionID: actor.SessionID, OriginalDeviceID: actor.DeviceID, Decision: in.Decision, RequestHash: in.RequestHash}
}
func decisionResultTestCounts(t *testing.T, f authorizationFixture) []int {
	t.Helper()
	result := []int{}
	for _, table := range []string{"devices", "sessions", "auth_receipts", "security_events", "device_proofs", "device_authorizations"} {
		result = append(result, accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", f.actor.AccountID))
	}
	result = append(result, accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.device_proofs WHERE account_id=? AND consumed_at IS NOT NULL", f.actor.AccountID))
	return result
}
func decisionResultTestRevoke(t *testing.T, f authorizationFixture, actor security.AccessClaims) {
	t.Helper()
	if err := f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, actor.AccountID, actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceAuthorizationDecisionResultPostgresCommittedFromNewActorWithoutCiphertext(t *testing.T) {
	for _, decision := range []deviceauthorization.Decision{deviceauthorization.Approve, deviceauthorization.Deny} {
		t.Run(string(decision), func(t *testing.T) {
			f := authorizationTestFixture(t)
			start, _ := authorizationTestCandidate(t, f, nil)
			if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), start); err != nil {
				t.Fatal(err)
			}
			in, _ := authorizationTestDecision(t, f, f.actor, f.key, start.AuthorizationID, decision)
			ack, err := authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, in)
			if err != nil {
				t.Fatal(err)
			}
			current, _ := authorizationTestAdditionalActor(t, f)
			decisionResultTestRevoke(t, f, f.actor)
			// The durable receipt survives expiry/removal of the separate candidate response.
			if err = f.owner.Exec("UPDATE mancuerna.auth_receipts SET response_ciphertext=NULL,key_version=NULL WHERE account_id=? AND purpose='device_authorization_result'", f.actor.AccountID).Error; err != nil {
				t.Fatal(err)
			}
			before := decisionResultTestCounts(t, f)
			u := decisionResultTestReader(t, f)
			input := decisionResultTestInput(f.actor, in)
			for range 3 {
				out, err := u.Execute(context.Background(), current, input)
				if err != nil || out.Status != personalApp.DeviceAuthorizationDecisionCompleted || out.CompletedAt == nil || *out.CompletedAt != ack.CompletedAt || out.Decision != decision || out.OriginalSessionID != f.actor.SessionID.String() || out.OriginalDeviceID != f.actor.DeviceID.String() {
					t.Fatalf("durable result: %+v %v", out, err)
				}
			}
			if !reflect.DeepEqual(before, decisionResultTestCounts(t, f)) {
				t.Fatal("receipt lookup repeated an effect")
			}
			for _, mutate := range []func(*personalApp.GetDeviceAuthorizationDecisionResultInput){func(v *personalApp.GetDeviceAuthorizationDecisionResultInput) { v.RequestHash[0] ^= 1 }, func(v *personalApp.GetDeviceAuthorizationDecisionResultInput) { v.AuthorizationID = uuid.New() }, func(v *personalApp.GetDeviceAuthorizationDecisionResultInput) {
				v.OriginalSessionID = current.SessionID
			}, func(v *personalApp.GetDeviceAuthorizationDecisionResultInput) { v.OriginalDeviceID = current.DeviceID }} {
				changed := input
				mutate(&changed)
				if _, err = u.Execute(context.Background(), current, changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
					t.Fatalf("changed binding accepted: %v", err)
				}
			}
			foreign := authorizationTestFixture(t)
			if _, err = u.Execute(context.Background(), foreign.actor, input); !errors.Is(err, personalErrors.ErrDeviceAuthorizationNotFound) {
				t.Fatalf("other account learned result: %v", err)
			}
			decisionResultTestRevoke(t, f, current)
			if _, err = u.Execute(context.Background(), current, input); !errors.Is(err, personalErrors.ErrSessionRevoked) {
				t.Fatalf("revoked current actor read receipt: %v", err)
			}
		})
	}
}

func TestDeviceAuthorizationDecisionResultPostgresAbsenceNeedsTerminalOriginalSession(t *testing.T) {
	f := authorizationTestFixture(t)
	current, _ := authorizationTestAdditionalActor(t, f)
	u := decisionResultTestReader(t, f)
	input := personalApp.GetDeviceAuthorizationDecisionResultInput{AuthorizationID: uuid.New(), OperationID: uuid.New(), OriginalSessionID: f.actor.SessionID, OriginalDeviceID: f.actor.DeviceID, Decision: deviceauthorization.Deny, RequestHash: [32]byte{12}}
	for range 2 {
		out, err := u.Execute(context.Background(), current, input)
		if err != nil || out.Status != personalApp.DeviceAuthorizationDecisionUnresolved || out.CompletedAt != nil {
			t.Fatalf("live session was called final: %+v %v", out, err)
		}
	}
	changed := input
	changed.OriginalDeviceID = uuid.New()
	if _, err := u.Execute(context.Background(), current, changed); !errors.Is(err, personalErrors.ErrDeviceAuthorizationNotFound) {
		t.Fatal(err)
	}
	changed = input
	changed.OriginalSessionID = uuid.New()
	if _, err := u.Execute(context.Background(), current, changed); !errors.Is(err, personalErrors.ErrDeviceAuthorizationNotFound) {
		t.Fatal(err)
	}
	decisionResultTestRevoke(t, f, f.actor)
	out, err := u.Execute(context.Background(), current, input)
	if err != nil || out.Status != personalApp.DeviceAuthorizationDecisionNotApplied || out.CompletedAt != nil {
		t.Fatalf("revoked absence: %+v %v", out, err)
	}
	// A distinct original SID expires without revocation. Only its actual expiry is terminal.
	original, _ := authorizationTestAdditionalActor(t, f)
	if err = f.owner.Exec("UPDATE mancuerna.sessions SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE account_id=? AND id=?", f.actor.AccountID, original.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	input.OriginalSessionID = original.SessionID
	input.OriginalDeviceID = original.DeviceID
	out, err = u.Execute(context.Background(), current, input)
	if err != nil || out.Status != personalApp.DeviceAuthorizationDecisionNotApplied || out.CompletedAt != nil {
		t.Fatalf("expired absence: %+v %v", out, err)
	}
}

func TestDeviceAuthorizationDecisionResultPostgresCorruptReceiptCannotComplete(t *testing.T) {
	f := authorizationTestFixture(t)
	start, _ := authorizationTestCandidate(t, f, nil)
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	in, _ := authorizationTestDecision(t, f, f.actor, f.key, start.AuthorizationID, deviceauthorization.Deny)
	if _, err := authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, in); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Exec("UPDATE mancuerna.auth_receipts SET result_secret_hash=NULL WHERE account_id=? AND operation_id=?", f.actor.AccountID, in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := decisionResultTestReader(t, f).Execute(context.Background(), f.actor, decisionResultTestInput(f.actor, in)); !errors.Is(err, personalErrors.ErrInvalidReceipt) {
		t.Fatalf("missing durable actor binding completed: %v", err)
	}
}

type decisionResultHeldEvent struct {
	repository.SecurityEventRepository
	entered chan struct{}
	release chan struct{}
}

func (g *decisionResultHeldEvent) Append(tx shared.Transaction, e repository.SecurityEvent) error {
	if err := g.SecurityEventRepository.Append(tx, e); err != nil {
		return err
	}
	close(g.entered)
	<-g.release
	return nil
}

type decisionResultStartedUoW struct {
	shared.UnitOfWork
	entered chan struct{}
}

func (g *decisionResultStartedUoW) Command(ctx context.Context, fn func(shared.Transaction) error) error {
	close(g.entered)
	return g.UnitOfWork.Command(ctx, fn)
}

type decisionResultAsync struct {
	out personalApp.DeviceAuthorizationDecisionResult
	err error
}

func TestDeviceAuthorizationDecisionResultPostgresWaitsForOriginalDecisionCommit(t *testing.T) {
	f := authorizationTestFixture(t)
	start, _ := authorizationTestCandidate(t, f, nil)
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	in, _ := authorizationTestDecision(t, f, f.actor, f.key, start.AuthorizationID, deviceauthorization.Approve)
	current, _ := authorizationTestAdditionalActor(t, f)
	held := &decisionResultHeldEvent{SecurityEventRepository: f.decide.Events, entered: make(chan struct{}), release: make(chan struct{})}
	f.decide.Events = held
	var once sync.Once
	release := func() { once.Do(func() { close(held.release) }) }
	defer release()
	commit := make(chan error, 1)
	go func() {
		_, err := authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, in)
		commit <- err
	}()
	select {
	case <-held.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("original decision did not reach held commit")
	}
	started := &decisionResultStartedUoW{UnitOfWork: f.d.UoW, entered: make(chan struct{})}
	reader, err := personalApp.NewGetDeviceAuthorizationDecisionResult(personalApp.GetDeviceAuthorizationDecisionResultDependencies{UoW: started, Sessions: f.d.Sessions, Receipts: f.d.Receipts, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan decisionResultAsync, 1)
	go func() {
		out, err := reader.Execute(context.Background(), current, decisionResultTestInput(f.actor, in))
		result <- decisionResultAsync{out, err}
	}()
	<-started.entered
	select {
	case got := <-result:
		t.Fatalf("reader escaped account mutex before original commit: %+v %v", got.out, got.err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-commit; err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil || got.out.Status != personalApp.DeviceAuthorizationDecisionCompleted {
		t.Fatalf("reader missed serialized commit: %+v %v", got.out, got.err)
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE account_id=? AND operation_id=?", f.actor.AccountID, in.OperationID) != 1 {
		t.Fatal("original receipt duplicated")
	}
}

func TestDeviceAuthorizationDecisionResultPostgresWaitsForOriginalRevocationCommit(t *testing.T) {
	f := authorizationTestFixture(t)
	current, _ := authorizationTestAdditionalActor(t, f)
	input := personalApp.GetDeviceAuthorizationDecisionResultInput{AuthorizationID: uuid.New(), OperationID: uuid.New(), OriginalSessionID: f.actor.SessionID, OriginalDeviceID: f.actor.DeviceID, Decision: deviceauthorization.Approve, RequestHash: [32]byte{17}}
	entered := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	defer release()
	commit := make(chan error, 1)
	go func() {
		commit <- f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
			if err := f.d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now()); err != nil {
				return err
			}
			close(entered)
			<-released
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not hold account mutex")
	}
	started := &decisionResultStartedUoW{UnitOfWork: f.d.UoW, entered: make(chan struct{})}
	reader, err := personalApp.NewGetDeviceAuthorizationDecisionResult(personalApp.GetDeviceAuthorizationDecisionResultDependencies{UoW: started, Sessions: f.d.Sessions, Receipts: f.d.Receipts, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan decisionResultAsync, 1)
	go func() {
		out, err := reader.Execute(context.Background(), current, input)
		result <- decisionResultAsync{out, err}
	}()
	<-started.entered
	select {
	case got := <-result:
		t.Fatalf("reader escaped revocation mutex: %+v %v", got.out, got.err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-commit; err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil || got.out.Status != personalApp.DeviceAuthorizationDecisionNotApplied {
		t.Fatalf("reader missed terminal session: %+v %v", got.out, got.err)
	}
}

func TestDeviceAuthorizationDecisionResultPostgresApplicationClockCannotProveSessionExpiry(t *testing.T) {
	f := authorizationTestFixture(t)
	start, _ := authorizationTestCandidate(t, f, nil)
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	in, _ := authorizationTestDecision(t, f, f.actor, f.key, start.AuthorizationID, deviceauthorization.Approve)
	current, _ := authorizationTestAdditionalActor(t, f)
	reader, err := personalApp.NewGetDeviceAuthorizationDecisionResult(personalApp.GetDeviceAuthorizationDecisionResultDependencies{
		UoW: f.d.UoW, Sessions: f.d.Sessions, Receipts: f.d.Receipts, Now: func() time.Time { return time.Now().Add(91 * 24 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := reader.Execute(context.Background(), current, decisionResultTestInput(f.actor, in))
	if err != nil {
		t.Fatal(err)
	}
	// The real authorization clock is PostgreSQL's. A future application clock
	// cannot promise that this original, still-live operation will never commit.
	if _, err = authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, in); err != nil {
		t.Fatalf("original operation unexpectedly stopped: %v", err)
	}
	if out.Status != personalApp.DeviceAuthorizationDecisionUnresolved || out.CompletedAt != nil {
		t.Fatalf("application clock declared absence final before the original operation committed: %s", out.Status)
	}
}

func TestDeviceAuthorizationDecisionResultPostgresExpiryPredicateRequiresExactAccountAndMutex(t *testing.T) {
	f := authorizationTestFixture(t)
	foreign := authorizationTestFixture(t)
	err := f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		expired, err := f.d.Sessions.SessionExpired(tx, f.actor.AccountID, f.actor.SessionID)
		if err != nil || expired {
			t.Fatalf("live session expiry: %v %v", expired, err)
		}
		if expired, err = f.d.Sessions.SessionExpired(tx, foreign.actor.AccountID, foreign.actor.SessionID); !errors.Is(err, personalErrors.ErrAuthSessionScope) || expired {
			t.Fatalf("foreign account scope accepted: %v %v", expired, err)
		}
		for _, id := range []uuid.UUID{foreign.actor.SessionID, uuid.New(), uuid.Nil} {
			if expired, err = f.d.Sessions.SessionExpired(tx, f.actor.AccountID, id); !errors.Is(err, personalErrors.ErrAuthObjectNotFound) || expired {
				t.Fatalf("missing or foreign SID disclosed expiry: %v %v", expired, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	q, err := f.d.UoW.Query(shared.WithPersonalAccount(context.Background(), f.actor.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	if expired, err := f.d.Sessions.SessionExpired(q, f.actor.AccountID, f.actor.SessionID); !errors.Is(err, personalErrors.ErrAuthSessionScope) || expired {
		t.Fatalf("read-only handle bypassed account mutex: %v %v", expired, err)
	}
	decisionResultTestRevoke(t, f, f.actor)
	if err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		expired, err := f.d.Sessions.SessionExpired(tx, f.actor.AccountID, f.actor.SessionID)
		if err != nil || expired {
			t.Fatalf("revocation was confused with expiry: %v %v", expired, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
