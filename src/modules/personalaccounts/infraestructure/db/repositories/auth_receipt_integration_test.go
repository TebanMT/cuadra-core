//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func TestAuthReceiptPostgresIsolationExpiryAndRollback(t *testing.T) {
	auth := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	uow := budgetTestUoW(t, auth)
	a, b := profileFixture(t, auth), profileFixture(t, auth)
	ctx := shared.WithPersonalAccount(context.Background(), a)
	other := shared.WithPersonalAccount(context.Background(), b)
	repo := NewAuthReceiptPostgresRepository()
	hash := sha256.Sum256([]byte("synthetic request"))
	r, err := receipt.New(uuid.New(), receipt.Owner{AccountID: a}, "recovery_key", hash, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	envelopes, _ := crypto.NewEnvelopes("test", map[string][]byte{"test": make([]byte, 32)})
	sealed, err := envelopes.SealReceipt(r.Context(), []byte(`{"secret":"synthetic protected result"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.SetResponse(sealed, r.CompletedAt.Add(10*time.Minute), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(ctx, func(tx shared.Transaction) error { return repo.Create(tx, r) }); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(other, func(tx shared.Transaction) error {
		for _, id := range []uuid.UUID{r.OperationID, uuid.New()} {
			if _, err := repo.Get(tx, r.Owner, id); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
				t.Fatal("foreign/unknown receipt distinguishable")
			}
		}
		if err := repo.SaveResponse(tx, r); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			t.Fatal("foreign write accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(ctx, func(tx shared.Transaction) error {
		stored, err := repo.Get(tx, r.Owner, r.OperationID)
		if err != nil {
			return err
		}
		e, err := stored.ReadResponse(r.CompletedAt)
		if err != nil {
			return err
		}
		plain, err := envelopes.OpenReceipt(stored.Context(), e)
		if err != nil || string(plain) != `{"secret":"synthetic protected result"}` {
			t.Fatal("protected result changed")
		}
		wrong := stored.Context()
		wrong.AccountID = b
		if _, err = envelopes.OpenReceipt(wrong, e); err == nil {
			t.Fatal("cross-account decryption accepted")
		}
		if _, err = stored.ReadResponse(stored.ResponseExpiresAt); !errors.Is(err, personalErrors.ErrResultExpired) {
			t.Fatal("expired result accepted")
		}
		stored.Response = security.Envelope{} // Simulate expiry cleanup, preserving receipt.
		return repo.SaveResponse(tx, stored)
	}); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(ctx, func(tx shared.Transaction) error {
		stored, err := repo.Get(tx, r.Owner, r.OperationID)
		if err != nil {
			return err
		}
		if err = stored.Match(r.Purpose, hash); err != nil {
			return err
		}
		if _, err = stored.ReadResponse(r.CompletedAt); !errors.Is(err, personalErrors.ErrResultExpired) {
			t.Fatal("cleared result usable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(ctx, func(tx shared.Transaction) error { return repo.Create(tx, r) }); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("cleared receipt allowed another effect")
	}
	rolled, _ := receipt.New(uuid.New(), r.Owner, "auth_check", hash, time.Now())
	failure := errors.New("synthetic later failure")
	if err = uow.Command(ctx, func(tx shared.Transaction) error {
		if err := repo.Create(tx, rolled); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	q, err := uow.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	if _, err = repo.Get(q, rolled.Owner, rolled.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
		t.Fatal("receipt survived rollback")
	}
}

func TestAuthReceiptPostgresAttemptSecretRequired(t *testing.T) {
	auth := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	uow := budgetTestUoW(t, auth)
	id, secret := uuid.New(), sha256.Sum256([]byte(uuid.NewString()))
	ctx := shared.WithPersonalAttempt(context.Background(), id, secret)
	r, _ := receipt.New(uuid.New(), receipt.Owner{AttemptID: id}, "auth_start", sha256.Sum256([]byte("start")), time.Now())
	repo := NewAuthReceiptPostgresRepository()
	if err := uow.Command(ctx, func(tx shared.Transaction) error {
		db := tx.(*shared.GormTransaction).Tx
		if err := db.Exec(`INSERT INTO mancuerna.auth_challenges(id,proposed_account_id,candidate_device_id,nonce,candidate_public_key,candidate_platform,candidate_label,purpose,destination_e164,channel,status,continuation_hash,request_hash,expires_at) VALUES(?,?,?,? ,?,'android','Synthetic','access','+525555555555','sms','uncertain',?,?,?)`, id, uuid.New(), uuid.New(), make([]byte, 32), make([]byte, 32), secret[:], r.RequestHash[:], time.Now().Add(time.Minute)).Error; err != nil {
			return err
		}
		return repo.Create(tx, r)
	}); err != nil {
		t.Fatal(err)
	}
	for _, attemptCtx := range []context.Context{shared.WithPersonalAttempt(context.Background(), id, sha256.Sum256([]byte("wrong"))), shared.WithPersonalAttempt(context.Background(), uuid.New(), secret)} {
		q, err := uow.Query(attemptCtx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = repo.Get(q, r.Owner, r.OperationID)
		shared.CloseQuery(q)
		if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
			t.Fatal("attempt receipt did not require exact continuation")
		}
	}
	q, err := uow.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	if _, err = repo.Get(q, r.Owner, r.OperationID); err != nil {
		t.Fatal(err)
	}
}
