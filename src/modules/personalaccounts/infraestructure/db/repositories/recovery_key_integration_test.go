//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/recovery"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func recoveryTestKey(t *testing.T, accountID uuid.UUID, generation int64) *recovery.RecoveryKey {
	t.Helper()
	digest := sha256.Sum256([]byte(uuid.NewString()))
	k, err := recovery.NewKey(accountID, uuid.New(), generation, security.RecoveryVerifier{KeyVersion: "synthetic-kit-v1", Digest: digest[:]}, time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func recoveryTestGet(t *testing.T, uow shared.UnitOfWork, repo *RecoveryKeyPostgresRepository, accountID, keyID uuid.UUID) *recovery.RecoveryKey {
	t.Helper()
	tx, err := uow.Query(shared.WithPersonalAccount(context.Background(), accountID))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(tx)
	k, err := repo.Get(tx, accountID, keyID)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRecoveryKeyPostgresIsolationAndPreparation(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := NewRecoveryKeyRepository()
	a := accessTestExisting(t, owner, accessTestPhone(), "active", true)
	b := accessTestExisting(t, owner, accessTestPhone(), "active", true)
	ka, kb := recoveryTestKey(t, a, 1), recoveryTestKey(t, b, 1)
	for _, key := range []*recovery.RecoveryKey{ka, kb} {
		if err := uow.Command(shared.WithPersonalAccount(context.Background(), key.AccountID), func(tx shared.Transaction) error { return repo.Create(tx, key.AccountID, key) }); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := uow.Query(shared.WithPersonalAccount(context.Background(), a))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(tx)
	live, err := repo.GetLive(tx, a)
	if err != nil || live.ID != ka.ID || live.Prepared() || live.PreparedAt != nil {
		t.Fatal("issued key falsely marked saved")
	}
	if _, err = repo.Get(tx, a, kb.ID); !errors.Is(err, personalErrors.ErrRecoveryKeyNotFound) {
		t.Fatal("foreign key exposed")
	}
	if _, err = repo.GetLive(tx, b); !errors.Is(err, personalErrors.ErrAuthSessionScope) {
		t.Fatal("foreign owner argument accepted")
	}
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	if err := live.ConfirmSaved(stamp); err != nil {
		t.Fatal(err)
	}
	if err := uow.Command(shared.WithPersonalAccount(context.Background(), a), func(tx shared.Transaction) error { return repo.SaveState(tx, a, live) }); err != nil {
		t.Fatal(err)
	}
	saved := recoveryTestGet(t, uow, repo, a, ka.ID)
	if !saved.Prepared() || !saved.PreparedAt.Equal(stamp) {
		t.Fatal("saved confirmation not persisted")
	}
	if err := saved.ConfirmSaved(stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := uow.Command(shared.WithPersonalAccount(context.Background(), a), func(tx shared.Transaction) error { return repo.SaveState(tx, a, saved) }); err != nil {
		t.Fatal(err)
	}
	if !recoveryTestGet(t, uow, repo, a, ka.ID).PreparedAt.Equal(stamp) {
		t.Fatal("repeat confirmation changed timestamp")
	}
}

func TestRecoveryKeyPostgresOneLiveAndImmutableVerifier(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uows := []shared.UnitOfWork{budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")), budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))}
	repo := NewRecoveryKeyRepository()
	a := accessTestExisting(t, owner, accessTestPhone(), "active", true)
	ctx := shared.WithPersonalAccount(context.Background(), a)
	keys := []*recovery.RecoveryKey{recoveryTestKey(t, a, 1), recoveryTestKey(t, a, 2)}
	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			errs[index] = uows[index].Command(ctx, func(tx shared.Transaction) error { return repo.Create(tx, a, keys[index]) })
		}(i)
	}
	close(start)
	wg.Wait()
	accepted := 0
	var live *recovery.RecoveryKey
	for i, err := range errs {
		if err == nil {
			accepted++
			live = keys[i]
		} else if !errors.Is(err, personalErrors.ErrRecoveryKeyConflict) {
			t.Fatal(err)
		}
	}
	if accepted != 1 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.recovery_keys WHERE account_id=?", a) != 1 {
		t.Fatal("more than one live key created")
	}
	for _, mutate := range []func(*recovery.RecoveryKey){func(k *recovery.RecoveryKey) { k.Verifier.Digest[0] ^= 1 }, func(k *recovery.RecoveryKey) { k.Verifier.KeyVersion = "another-version" }, func(k *recovery.RecoveryKey) { k.Generation++ }, func(k *recovery.RecoveryKey) { k.CreatedAt = k.CreatedAt.Add(-time.Second) }} {
		changed := recoveryTestGet(t, uows[0], repo, a, live.ID)
		mutate(changed)
		if err := uows[0].Command(ctx, func(tx shared.Transaction) error { return repo.SaveState(tx, a, changed) }); !errors.Is(err, personalErrors.ErrInvalidRecoveryKey) {
			t.Fatal("immutable verifier or identity was changed")
		}
	}
	if err := uows[0].Command(ctx, func(tx shared.Transaction) error { return repo.Create(tx, a, live) }); !errors.Is(err, personalErrors.ErrRecoveryKeyConflict) {
		t.Fatal("existing key overwritten")
	}
}

func TestRecoveryKeyPostgresConsumeReplacementRollsBackTogether(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := NewRecoveryKeyRepository()
	a := accessTestExisting(t, owner, accessTestPhone(), "active", true)
	ctx := shared.WithPersonalAccount(context.Background(), a)
	old, next := recoveryTestKey(t, a, 1), recoveryTestKey(t, a, 2)
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.Create(tx, a, old) }); err != nil {
		t.Fatal(err)
	}
	replace := func(tx shared.Transaction) error {
		current, err := repo.Get(tx, a, old.ID)
		if err != nil {
			return err
		}
		if err = current.Consume(time.Now()); err != nil {
			return err
		}
		if err = repo.SaveState(tx, a, current); err != nil {
			return err
		}
		return repo.Create(tx, a, next)
	}
	failure := errors.New("synthetic later recovery response failure")
	if err := uow.Command(ctx, func(tx shared.Transaction) error {
		if err := replace(tx); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if !recoveryTestGet(t, uow, repo, a, old.ID).Live() || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.recovery_keys WHERE id=?", next.ID) != 0 {
		t.Fatal("failed recovery retained consumption or replacement")
	}
	if err := uow.Command(ctx, replace); err != nil {
		t.Fatal(err)
	}
	consumed := recoveryTestGet(t, uow, repo, a, old.ID)
	if consumed.Live() || consumed.ConsumedAt == nil || !errors.Is(consumed.Consume(time.Now()), personalErrors.ErrRecoveryKeyState) {
		t.Fatal("consumed key could be reused")
	}
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.SaveState(tx, a, old) }); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
		t.Fatal("stale key resurrected consumed state")
	}
	newKey := recoveryTestGet(t, uow, repo, a, next.ID)
	if !newKey.Live() || newKey.Prepared() || newKey.PreparedAt != nil {
		t.Fatal("replacement inherited false saved status")
	}
}

func TestRecoveryKeyPostgresRevocationAndInactiveAccount(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := NewRecoveryKeyRepository()
	a := accessTestExisting(t, owner, accessTestPhone(), "active", true)
	ctx := shared.WithPersonalAccount(context.Background(), a)
	k := recoveryTestKey(t, a, 1)
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.Create(tx, a, k) }); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	if err := k.Revoke(stamp); err != nil {
		t.Fatal(err)
	}
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.SaveState(tx, a, k) }); err != nil {
		t.Fatal(err)
	}
	if err := k.Revoke(stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.SaveState(tx, a, k) }); err != nil {
		t.Fatal(err)
	}
	if !recoveryTestGet(t, uow, repo, a, k.ID).RevokedAt.Equal(stamp) {
		t.Fatal("revocation changed")
	}
	tx, err := uow.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.GetLive(tx, a); !errors.Is(err, personalErrors.ErrRecoveryKeyNotFound) {
		t.Fatal("revoked key returned as live")
	}
	shared.CloseQuery(tx)
	if err = owner.Exec("UPDATE mancuerna.accounts SET status='deletion_pending',deletion_requested_at=clock_timestamp() WHERE id=?", a).Error; err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(ctx, func(tx shared.Transaction) error { return repo.Create(tx, a, recoveryTestKey(t, a, 2)) }); !errors.Is(err, personalErrors.ErrRecoveryKeyNotFound) {
		t.Fatal("inactive account created key")
	}
}

func TestRecoveryKeyPostgresBoundSignupRollback(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := NewRecoveryKeyRepository()
	f := accessTestFixture(t, owner, "", uuid.Nil)
	failure := errors.New("synthetic later signup failure")
	if err := uow.Command(accessTestContext(f), func(tx shared.Transaction) error {
		bound, err := accessTestRepository(t).ResolveVerifiedAccess(tx, f.attempt)
		if err != nil {
			return err
		}
		if err = repo.Create(tx, bound.AccountID, recoveryTestKey(t, bound.AccountID, 1)); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=?", f.proposed) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.recovery_keys WHERE account_id=?", f.proposed) != 0 {
		t.Fatal("key committed outside signup transaction")
	}
}
