//go:build server && integration

package domain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPersonalPostgresPreparedAuthScopeAndAtomicity(t *testing.T) {
	authDB := personalTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	dataDB := personalTestPool(t, "MANCUERNA_TEST_DATA_DSN")
	auth, err := NewPersonalPostgresUnitOfWork(authDB, PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	data, err := NewPersonalPostgresUnitOfWork(dataDB, PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	u := auth.(PersonalAuthPreparedUnitOfWork)
	id := personalTestAccount(t, authDB)
	ctx := WithPersonalAccount(context.Background(), id)
	called := false
	callback := func(Transaction) error { called = true; return nil }
	for _, bad := range []context.Context{nil, context.Background(), WithPersonalAttempt(ctx, uuid.New(), [32]byte{1}), WithPersonalAccount(ctx, uuid.New()), WithTenant(ctx, uuid.New()), WithPersonalAccount(WithTenant(context.Background(), uuid.New()), id)} {
		if err = u.PreparedCommand(bad, callback, callback); !errors.Is(err, ErrPersonalScope) {
			t.Fatalf("invalid scope accepted: %v", err)
		}
	}
	if err = data.(PersonalAuthPreparedUnitOfWork).PreparedCommand(ctx, callback, callback); !errors.Is(err, ErrPersonalScope) {
		t.Fatal("data pool accepted preparation")
	}
	if err = u.PreparedCommand(ctx, nil, callback); !errors.Is(err, ErrPersonalScope) {
		t.Fatal("nil preparation accepted")
	}
	if called {
		t.Fatal("invalid boundary reached a callback")
	}
	failure := errors.New("synthetic preparation failure")
	write := func(tx Transaction) error {
		var isolation, readOnly, account, attempt string
		db := tx.(*GormTransaction).Tx
		if err := db.Raw(`SELECT current_setting('transaction_isolation'),current_setting('transaction_read_only'),current_setting('mancuerna.account_id'),current_setting('mancuerna.attempt_id')`).Row().Scan(&isolation, &readOnly, &account, &attempt); err != nil {
			return err
		}
		if isolation != "read committed" || readOnly != "off" || account != id.String() || attempt != "" {
			t.Error("incorrect prepared transaction scope")
		}
		return db.Exec(`UPDATE mancuerna.accounts SET security_version=security_version+1 WHERE id=?`, id).Error
	}
	if err = u.PreparedCommand(ctx, func(tx Transaction) error {
		if err := write(tx); err != nil {
			return err
		}
		return failure
	}, callback); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if called {
		t.Fatal("apply ran after prepare error")
	}
	if err = u.PreparedCommand(ctx, write, func(Transaction) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		_ = u.PreparedCommand(ctx, write, func(Transaction) error { panic("synthetic apply panic") })
	}()
	q, err := auth.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	err = q.(*GormTransaction).Tx.Raw(`SELECT security_version FROM mancuerna.accounts WHERE id=?`, id).Row().Scan(&version)
	CloseQuery(q)
	if err != nil || version != 1 {
		t.Fatalf("failed prepared command persisted: version=%d err=%v", version, err)
	}
	if err = u.PreparedCommand(ctx, write, callback); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("successful command omitted apply")
	}
	if err = u.PreparedCommand(WithPersonalAccount(context.Background(), uuid.New()), func(Transaction) error { return nil }, func(Transaction) error { t.Error("missing stream reached apply"); return nil }); !errors.Is(err, ErrPersonalStreamMissing) {
		t.Fatal(err)
	}
}

func TestPersonalPostgresPreparedRunsBeforeAccountMutex(t *testing.T) {
	authDB := personalTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	dataDB := personalTestPool(t, "MANCUERNA_TEST_DATA_DSN")
	auth, err := NewPersonalPostgresUnitOfWork(authDB, PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	data, err := NewPersonalPostgresUnitOfWork(dataDB, PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	id := personalTestAccount(t, authDB)
	ctx := WithPersonalAccount(context.Background(), id)
	held, err := data.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Rollback(held)
	prepared, applied := false, false
	deadline, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	err = auth.(PersonalAuthPreparedUnitOfWork).PreparedCommand(deadline, func(tx Transaction) error {
		prepared = true
		return tx.(*GormTransaction).Tx.Exec(`SELECT pg_advisory_xact_lock(?,?)`, int32(0x4d435450), int32(1)).Error
	}, func(Transaction) error { applied = true; return nil })
	if err == nil || !prepared || applied {
		t.Fatal("prepared command did not respect preparation then mutex")
	}
	if err = data.Commit(held); err != nil {
		t.Fatal(err)
	}
	if err = auth.Command(ctx, func(Transaction) error { return nil }); err != nil {
		t.Fatal("connection/lock leaked after cancellation")
	}
}
