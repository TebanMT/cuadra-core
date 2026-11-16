//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	port "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type accessFixture struct {
	attempt, proposed uuid.UUID
	hash              [32]byte
	phone             string
}

func accessTestPhone() string {
	id := uuid.New()
	return fmt.Sprintf("+5299%010d", binary.BigEndian.Uint64(id[:8])%10000000000)
}

func accessTestFixture(t *testing.T, owner *gorm.DB, phone string, proposed uuid.UUID) accessFixture {
	t.Helper()
	if phone == "" {
		phone = accessTestPhone()
	}
	if proposed == uuid.Nil {
		proposed = uuid.New()
	}
	f := accessFixture{attempt: uuid.New(), proposed: proposed, hash: sha256.Sum256([]byte(uuid.NewString())), phone: phone}
	key := sha256.Sum256([]byte(uuid.NewString()))
	if err := owner.Exec(`INSERT INTO mancuerna.auth_challenges(id,proposed_account_id,candidate_device_id,nonce,candidate_public_key,candidate_platform,candidate_label,purpose,destination_e164,channel,status,continuation_hash,request_hash,expires_at,verified_at,created_at,provider_reference)
 VALUES(?,?,?,?,?,'ios','Synthetic access device','access',?,'whatsapp','verified',?,?,clock_timestamp()+interval '5 minutes',clock_timestamp()-interval '1 second',clock_timestamp()-interval '2 seconds',?)`, f.attempt, f.proposed, uuid.New(), key[:], key[:], f.phone, f.hash[:], key[:], "VE"+fmt.Sprintf("%x", uuid.New())).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func accessTestRepository(t *testing.T) *IdentityAccessRepository {
	t.Helper()
	r, err := NewIdentityAccessRepository([]byte("synthetic-stable-identity-phone-key-32"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func accessTestContext(f accessFixture) context.Context {
	return shared.WithPersonalAttempt(context.Background(), f.attempt, f.hash)
}

func accessTestResolve(uow shared.UnitOfWork, repo *IdentityAccessRepository, f accessFixture) (port.BoundAccess, error) {
	var result port.BoundAccess
	ctx, cancel := context.WithTimeout(accessTestContext(f), 5*time.Second)
	defer cancel()
	err := uow.Command(ctx, func(tx shared.Transaction) error {
		var err error
		result, err = repo.ResolveVerifiedAccess(tx, f.attempt)
		return err
	})
	return result, err
}

func accessTestCount(t *testing.T, owner *gorm.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := owner.Raw(query, args...).Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func accessTestExisting(t *testing.T, owner *gorm.DB, phone, status string, stream bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := owner.Exec(`INSERT INTO mancuerna.accounts(id,phone_e164,status,phone_verified_at,deletion_requested_at,deleted_at)
 VALUES(?,?,?,clock_timestamp(),CASE WHEN ?='active' THEN NULL ELSE clock_timestamp() END,CASE WHEN ?='deleted' THEN clock_timestamp() ELSE NULL END)`, id, phone, status, status, status).Error; err != nil {
		t.Fatal(err)
	}
	if stream {
		if err := owner.Exec("INSERT INTO mancuerna.sync_streams(account_id,epoch) VALUES(?,?)", id, uuid.New()).Error; err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestIdentityAccessPostgresSerializesPhoneWithoutAuthorizingCandidate(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uows := []shared.UnitOfWork{budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")), budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))}
	phone := accessTestPhone()
	fixtures := []accessFixture{accessTestFixture(t, owner, phone, uuid.Nil), accessTestFixture(t, owner, phone, uuid.Nil)}
	results := make([]port.BoundAccess, 2)
	errorsFound := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range fixtures {
		repo := accessTestRepository(t)
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results[index], errorsFound[index] = accessTestResolve(uows[index], repo, fixtures[index])
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	if results[0].AccountID != results[1].AccountID || results[0].Created == results[1].Created || results[0].AccountID == uuid.Nil {
		t.Fatal("phone race created two identities or granted a second signup")
	}
	accountID := results[0].AccountID
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE phone_e164=?", phone) != 1 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sync_streams WHERE account_id=?", accountID) != 1 {
		t.Fatal("account and stream were not created exactly once")
	}
	for _, table := range []string{"devices", "sessions", "recovery_keys"} {
		if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", accountID) != 0 {
			t.Fatal("binding authorized a candidate or issued credentials")
		}
	}
	for _, f := range fixtures {
		if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_challenges WHERE id=? AND account_id IS NULL AND status='verified' AND consumed_at IS NULL", f.attempt) != 1 {
			t.Fatal("binding mutated the attempt aggregate")
		}
	}
}

func TestIdentityAccessPostgresRollsBackCreationAndScopesReads(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := accessTestRepository(t)
	f := accessTestFixture(t, owner, "", uuid.Nil)
	otherID := accessTestExisting(t, owner, accessTestPhone(), "active", true)
	failure := errors.New("synthetic device persistence failure")
	err := uow.Command(accessTestContext(f), func(tx shared.Transaction) error {
		bound, err := repo.ResolveVerifiedAccess(tx, f.attempt)
		if err != nil {
			return err
		}
		if !bound.Created || bound.AccountID != f.proposed {
			t.Fatal("wrong proposed account binding")
		}
		connection := tx.(*shared.GormTransaction).Tx
		var count int
		if err = connection.Raw("SELECT count(*) FROM mancuerna.accounts").Scan(&count).Error; err != nil || count != 1 {
			t.Fatal("bound auth scope does not see only its account")
		}
		if err = connection.Raw("SELECT count(*) FROM mancuerna.accounts WHERE id=?", otherID).Scan(&count).Error; err != nil || count != 0 {
			t.Fatal("bound auth scope exposed another account")
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=?", f.proposed) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sync_streams WHERE account_id=?", f.proposed) != 0 {
		t.Fatal("failed command retained new identity")
	}
	bound, err := accessTestResolve(uow, repo, f)
	if err != nil || !bound.Created {
		t.Fatalf("rolled back binding could not be retried: %v", err)
	}
}

func TestIdentityAccessPostgresRejectsInvalidOrMixedScopes(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	auth := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	uow := budgetTestUoW(t, auth)
	repo := accessTestRepository(t)
	f := accessTestFixture(t, owner, "", uuid.Nil)
	existing := accessTestExisting(t, owner, accessTestPhone(), "active", true)
	for _, ctx := range []context.Context{shared.WithPersonalAccount(accessTestContext(f), existing), shared.WithPersonalAttempt(context.Background(), f.attempt, sha256.Sum256([]byte("wrong synthetic secret")))} {
		err := uow.Command(ctx, func(tx shared.Transaction) error {
			_, err := repo.ResolveVerifiedAccess(tx, f.attempt)
			return err
		})
		if !errors.Is(err, personalErrors.ErrAccessIdentityScope) && !errors.Is(err, personalErrors.ErrAccessIdentityUnavailable) {
			t.Fatalf("invalid initial scope accepted: %v", err)
		}
	}
	err := uow.Command(accessTestContext(f), func(tx shared.Transaction) error {
		if err := tx.(*shared.GormTransaction).Tx.Exec("SELECT set_config('tinta.gym_id', ?, true)", uuid.NewString()).Error; err != nil {
			return err
		}
		_, err := repo.ResolveVerifiedAccess(tx, f.attempt)
		return err
	})
	if !errors.Is(err, personalErrors.ErrAccessIdentityScope) {
		t.Fatal("mixed gym SQL scope accepted")
	}
	err = uow.Command(accessTestContext(f), func(tx shared.Transaction) error {
		_, err := repo.ResolveVerifiedAccess(tx, uuid.New())
		return err
	})
	if !errors.Is(err, personalErrors.ErrAccessIdentityScope) {
		t.Fatal("different attempt argument accepted")
	}
	err = shared.ReadSnapshot(accessTestContext(f), uow, func(tx shared.Transaction) error {
		_, err := repo.ResolveVerifiedAccess(tx, f.attempt)
		return err
	})
	if !errors.Is(err, personalErrors.ErrAccessIdentityScope) {
		t.Fatal("read snapshot allowed auth mutation")
	}
	if _, err := repo.ResolveVerifiedAccess(&shared.GormTransaction{Tx: auth}, f.attempt); !errors.Is(err, personalErrors.ErrAccessIdentityScope) {
		t.Fatal("autocommit handle accepted")
	}
	// Even an owner transaction with forged settings is not this capability's
	// allowed runtime. This check does not rely on RLS owner behavior.
	ownerTx := owner.Begin()
	defer ownerTx.Rollback()
	if err = ownerTx.Exec("SELECT set_config('mancuerna.attempt_id', ?, true), set_config('mancuerna.attempt_hash', ?, true)", f.attempt.String(), fmt.Sprintf("%x", f.hash)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ResolveVerifiedAccess(&shared.GormTransaction{Tx: ownerTx}, f.attempt); !errors.Is(err, personalErrors.ErrAccessIdentityScope) {
		t.Fatal("privileged pool accepted as auth runtime")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=?", f.proposed) != 0 {
		t.Fatal("invalid scope created an account")
	}
}

func TestIdentityAccessPostgresRejectsUnavailableAndOccupiedCandidate(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := accessTestRepository(t)
	for _, status := range []string{"deletion_pending", "deleted", "missing_stream"} {
		t.Run(status, func(t *testing.T) {
			phone := accessTestPhone()
			state, stream := status, true
			if status == "missing_stream" {
				state, stream = "active", false
			}
			id := accessTestExisting(t, owner, phone, state, stream)
			f := accessTestFixture(t, owner, phone, uuid.Nil)
			if _, err := accessTestResolve(uow, repo, f); !errors.Is(err, personalErrors.ErrAccessIdentityUnavailable) {
				t.Fatalf("unavailable identity accepted: %v", err)
			}
			if !stream && accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sync_streams WHERE account_id=?", id) != 0 {
				t.Fatal("missing stream was silently recreated")
			}
		})
	}
	occupied := accessTestExisting(t, owner, accessTestPhone(), "active", true)
	f := accessTestFixture(t, owner, "", occupied)
	if _, err := accessTestResolve(uow, repo, f); !errors.Is(err, personalErrors.ErrAccessIdentityUnavailable) {
		t.Fatalf("occupied proposed UUID adopted another account: %v", err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE phone_e164=?", f.phone) != 0 {
		t.Fatal("proposed UUID collision created an identity")
	}
	for name, mutation := range map[string]string{
		"pending":                   "status='pending',verified_at=NULL",
		"expired":                   "expires_at=clock_timestamp()-interval '1 second'",
		"consumed":                  "consumed_at=clock_timestamp()",
		"wrong purpose":             "purpose='phone_change'",
		"future verification":       "verified_at=clock_timestamp()+interval '1 day'",
		"missing provider evidence": "provider_reference=NULL",
	} {
		t.Run(name, func(t *testing.T) {
			f := accessTestFixture(t, owner, "", uuid.Nil)
			if err := owner.Exec("UPDATE mancuerna.auth_challenges SET "+mutation+" WHERE id=?", f.attempt).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := accessTestResolve(uow, repo, f); !errors.Is(err, personalErrors.ErrAccessIdentityUnavailable) {
				t.Fatalf("ineligible challenge accepted: %v", err)
			}
			if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=?", f.proposed) != 0 {
				t.Fatal("ineligible challenge created account")
			}
		})
	}
}

func TestIdentityAccessPostgresRevalidatesAfterPhoneLock(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uowA := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	uowB := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := accessTestRepository(t)
	f := accessTestFixture(t, owner, "", uuid.Nil)
	held, err := uowA.Begin(accessTestContext(f))
	if err != nil {
		t.Fatal(err)
	}
	defer uowA.Rollback(held)
	if err = held.(*shared.GormTransaction).Tx.Exec("SELECT pg_advisory_xact_lock(?)", repo.phoneLockKey(f.phone)).Error; err != nil {
		t.Fatal(err)
	}
	pid := make(chan int, 1)
	finished := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(accessTestContext(f), 5*time.Second)
		defer cancel()
		finished <- uowB.Command(ctx, func(tx shared.Transaction) error {
			var backend int
			if err := tx.(*shared.GormTransaction).Tx.Raw("SELECT pg_backend_pid()").Scan(&backend).Error; err != nil {
				return err
			}
			pid <- backend
			_, err := repo.ResolveVerifiedAccess(tx, f.attempt)
			return err
		})
	}()
	var backend int
	select {
	case backend = <-pid:
	case err := <-finished:
		t.Fatalf("contender did not begin: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("contender did not begin")
	}
	deadline := time.Now().Add(2 * time.Second)
	for accessTestCount(t, owner, "SELECT count(*) FROM pg_locks WHERE pid=? AND locktype='advisory' AND NOT granted", backend) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("contender did not wait on phone lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = owner.Exec("UPDATE mancuerna.auth_challenges SET status='cancelled' WHERE id=?", f.attempt).Error; err != nil {
		t.Fatal(err)
	}
	if err = uowA.Commit(held); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; !errors.Is(err, personalErrors.ErrAccessIdentityUnavailable) {
		t.Fatalf("challenge changed during wait but was accepted: %v", err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=?", f.proposed) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sync_streams WHERE account_id=?", f.proposed) != 0 {
		t.Fatal("failed revalidation retained new account")
	}
}
