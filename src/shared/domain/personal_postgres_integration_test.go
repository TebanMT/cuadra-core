//go:build server && integration

package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This helper never reads DATABASE_URL, .env, or the developer's database.
// The disposable harness supplies explicit local, restricted-role DSNs.
func personalTestPool(t *testing.T, key string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(key)
	if dsn == "" {
		t.Skip(key + " not configured; run the disposable Mancuerna harness")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid personal test database configuration")
	}
	local := func(host string) bool {
		if filepath.IsAbs(host) || host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	if !local(config.Host) {
		t.Fatal("personal integration tests require a local database")
	}
	for _, fallback := range config.Fallbacks {
		if !local(fallback.Host) {
			t.Fatal("personal integration tests require local fallbacks")
		}
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("required isolated personal database unavailable")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

func personalTestAccount(t *testing.T, auth *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	phone := fmt.Sprintf("+5255%09d", binary.BigEndian.Uint32(id[:4])%1000000000)
	// Fixture setup is not the signup implementation. It uses the restricted
	// auth role and explicitly scopes the account before populating both rows.
	err := auth.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT set_config('tinta.gym_id','',true),set_config('mancuerna.account_id',?,true),set_config('mancuerna.attempt_id','',true),set_config('mancuerna.attempt_hash','',true)`, id.String()).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO mancuerna.accounts(id,phone_e164,phone_verified_at,display_name) VALUES(?,?,now(),'Initial')`, id, phone).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO mancuerna.sync_streams(account_id,epoch) VALUES(?,?)`, id, uuid.New()).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPersonalPostgresScopesRollbackAndPooling(t *testing.T) {
	dataDB := personalTestPool(t, "MANCUERNA_TEST_DATA_DSN")
	authDB := personalTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	data, err := NewPersonalPostgresUnitOfWork(dataDB, PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewPersonalPostgresUnitOfWork(authDB, PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersonalPostgresUnitOfWork(dataDB, PersonalAuthRole); err == nil {
		t.Fatal("data login accepted as auth")
	}
	if _, err := NewPersonalPostgresUnitOfWork(authDB, PersonalDataRole); err == nil {
		t.Fatal("auth login accepted as data")
	}
	a, b := personalTestAccount(t, authDB), personalTestAccount(t, authDB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	aCtx, bCtx := WithPersonalAccount(ctx, a), WithPersonalAccount(ctx, b)
	read := func(scope context.Context, want uuid.UUID) {
		t.Helper()
		tx, err := data.Query(scope)
		if err != nil {
			t.Fatal(err)
		}
		defer CloseQuery(tx)
		var ids []uuid.UUID
		if err := tx.(*GormTransaction).Tx.Raw(`SELECT id FROM mancuerna.accounts`).Scan(&ids).Error; err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0] != want {
			t.Fatalf("unfiltered query exposed unexpected accounts: count=%d", len(ids))
		}
	}
	for i := 0; i < 5; i++ {
		read(aCtx, a)
		read(bCtx, b)
	}
	for _, bad := range []context.Context{ctx, WithPersonalAccount(aCtx, b), WithTenant(aCtx, b), WithPersonalAccount(WithTenant(ctx, b), a)} {
		if _, err := data.Query(bad); !errors.Is(err, ErrPersonalScope) {
			t.Fatalf("scope accepted: %v", err)
		}
	}
	if err := data.Command(WithPersonalAccount(ctx, uuid.New()), func(Transaction) error { return nil }); !errors.Is(err, ErrPersonalStreamMissing) {
		t.Fatalf("missing stream accepted: %v", err)
	}
	if err := data.Command(aCtx, func(tx Transaction) error {
		result := tx.(*GormTransaction).Tx.Exec(`UPDATE mancuerna.accounts SET display_name='Foreign' WHERE id=?`, b)
		if result.RowsAffected != 0 {
			t.Fatal("foreign update succeeded")
		}
		return result.Error
	}); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("synthetic rollback")
	if err := data.Command(aCtx, func(tx Transaction) error {
		if err := tx.(*GormTransaction).Tx.Exec(`UPDATE mancuerna.accounts SET display_name='Rollback' WHERE id=?`, a).Error; err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		_ = data.Command(aCtx, func(tx Transaction) error {
			if err := tx.(*GormTransaction).Tx.Exec(`UPDATE mancuerna.accounts SET display_name='Panic' WHERE id=?`, a).Error; err != nil {
				t.Fatal(err)
			}
			panic("synthetic panic")
		})
	}()
	tx, err := data.Query(aCtx)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err = tx.(*GormTransaction).Tx.Raw(`SELECT display_name FROM mancuerna.accounts WHERE id=?`, a).Row().Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Initial" {
		t.Fatal("failed command committed")
	}
	if err = tx.(*GormTransaction).Tx.Exec(`UPDATE mancuerna.accounts SET display_name='Read write' WHERE id=?`, a).Error; err == nil {
		t.Fatal("Query allowed mutation")
	}
	CloseQuery(tx)
	read(bCtx, b)
	var count int
	if err = dataDB.Raw(`SELECT count(*) FROM mancuerna.accounts`).Row().Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("pooled connection retained account scope")
	}
	if err = auth.Command(aCtx, func(Transaction) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestPersonalPostgresMutexAndConsistentSnapshot(t *testing.T) {
	dataDB := personalTestPool(t, "MANCUERNA_TEST_DATA_DSN")
	authDB := personalTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	data, err := NewPersonalPostgresUnitOfWork(dataDB, PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewPersonalPostgresUnitOfWork(authDB, PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	a := personalTestAccount(t, authDB)
	ctx, cancel := context.WithTimeout(WithPersonalAccount(context.Background(), a), 10*time.Second)
	defer cancel()
	held, err := data.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	called := false
	err = auth.Command(waitCtx, func(Transaction) error { called = true; return nil })
	stop()
	if err == nil || called {
		_ = data.Rollback(held)
		t.Fatal("auth mutation bypassed data mutex")
	}
	if err = data.Commit(held); err != nil {
		t.Fatal(err)
	}
	if err = auth.Command(ctx, func(Transaction) error { return nil }); err != nil {
		t.Fatal(err)
	}
	err = ReadSnapshot(ctx, data, func(tx Transaction) error {
		var before, after, isolation, readOnly string
		db := tx.(*GormTransaction).Tx
		if err := db.Raw(`SELECT current_setting('transaction_isolation'),current_setting('transaction_read_only')`).Row().Scan(&isolation, &readOnly); err != nil {
			return err
		}
		if isolation != "repeatable read" || readOnly != "on" {
			return fmt.Errorf("incorrect snapshot transaction")
		}
		if err := db.Raw(`SELECT display_name FROM mancuerna.accounts WHERE id=?`, a).Row().Scan(&before); err != nil {
			return err
		}
		if err := auth.Command(ctx, func(other Transaction) error {
			return other.(*GormTransaction).Tx.Exec(`UPDATE mancuerna.accounts SET display_name='Concurrent' WHERE id=?`, a).Error
		}); err != nil {
			return err
		}
		if err := db.Raw(`SELECT display_name FROM mancuerna.accounts WHERE id=?`, a).Row().Scan(&after); err != nil {
			return err
		}
		if before != after {
			return fmt.Errorf("snapshot changed during concurrent commit")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPersonalPostgresAuthAttemptIsolation(t *testing.T) {
	authDB := personalTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	auth, err := NewPersonalPostgresUnitOfWork(authDB, PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	hash := sha256.Sum256([]byte(uuid.NewString()))
	ctx, cancel := context.WithTimeout(WithPersonalAttempt(context.Background(), id, hash), 10*time.Second)
	defer cancel()
	err = auth.Command(ctx, func(tx Transaction) error {
		return tx.(*GormTransaction).Tx.Exec(`INSERT INTO mancuerna.auth_challenges(id,proposed_account_id,candidate_device_id,nonce,candidate_public_key,candidate_platform,candidate_label,purpose,destination_e164,channel,status,continuation_hash,request_hash,expires_at) VALUES(?,?,?,decode(repeat('a',64),'hex'),decode(repeat('b',64),'hex'),'android','Synthetic','access','+525500000099','sms','pending',decode(?,'hex'),decode(repeat('c',64),'hex'),now()+interval '10 minutes')`, id, uuid.New(), uuid.New(), hex.EncodeToString(hash[:])).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		scope context.Context
		want  int
	}{
		{ctx, 1},
		{WithPersonalAttempt(context.Background(), id, sha256.Sum256([]byte("wrong"))), 0},
		{WithPersonalAttempt(context.Background(), uuid.New(), hash), 0},
	} {
		tx, err := auth.Query(test.scope)
		if err != nil {
			t.Fatal(err)
		}
		var count int
		err = tx.(*GormTransaction).Tx.Raw(`SELECT count(*) FROM mancuerna.auth_challenges`).Row().Scan(&count)
		CloseQuery(tx)
		if err != nil {
			t.Fatal(err)
		}
		if count != test.want {
			t.Fatalf("attempt query got %d rows, want %d", count, test.want)
		}
	}
}

func TestPersonalPostgresMixedAttemptFailsAtRLS(t *testing.T) {
	authDB := personalTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	auth, err := NewPersonalPostgresUnitOfWork(authDB, PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	id, receiptID := uuid.New(), uuid.New()
	hash := sha256.Sum256([]byte(uuid.NewString()))
	ctx := WithPersonalAttempt(context.Background(), id, hash)
	err = auth.Command(ctx, func(tx Transaction) error {
		db := tx.(*GormTransaction).Tx
		if err := db.Exec(`INSERT INTO mancuerna.auth_challenges(id,proposed_account_id,candidate_device_id,nonce,candidate_public_key,candidate_platform,candidate_label,purpose,destination_e164,channel,status,continuation_hash,request_hash,expires_at) VALUES(?,?,?,decode(repeat('a',64),'hex'),decode(repeat('b',64),'hex'),'android','Synthetic','access','+525500000098','sms','pending',decode(?,'hex'),decode(repeat('c',64),'hex'),now()+interval '10 minutes')`, id, uuid.New(), uuid.New(), hex.EncodeToString(hash[:])).Error; err != nil {
			return err
		}
		return db.Exec(`INSERT INTO mancuerna.auth_receipts(operation_id,attempt_id,purpose,request_hash,response_expires_at) VALUES(?,?,'signup',decode(repeat('c',64),'hex'),now()+interval '10 minutes')`, receiptID, id).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bypass the Go scope guard deliberately: RLS must also reject a mixed
	// connection even with a valid attempt ID and continuation hash.
	err = authDB.Transaction(func(db *gorm.DB) error {
		if err := db.Exec(`SELECT set_config('mancuerna.account_id','',true),set_config('mancuerna.attempt_id',?,true),set_config('mancuerna.attempt_hash',?,true),set_config('tinta.gym_id',?,true)`, id.String(), hex.EncodeToString(hash[:]), uuid.NewString()).Error; err != nil {
			return err
		}
		for _, table := range []string{"auth_challenges", "auth_receipts"} {
			var count int
			if err := db.Raw(`SELECT count(*) FROM mancuerna.` + table).Row().Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("mixed context exposed %s", table)
			}
		}
		update := db.Exec(`UPDATE mancuerna.auth_challenges SET candidate_label='Mixed' WHERE id=?`, id)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 0 {
			return errors.New("mixed context updated attempt")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = authDB.Transaction(func(db *gorm.DB) error {
		if err := db.Exec(`SELECT set_config('mancuerna.account_id','',true),set_config('mancuerna.attempt_id',?,true),set_config('mancuerna.attempt_hash',?,true),set_config('tinta.gym_id',?,true)`, id.String(), hex.EncodeToString(hash[:]), uuid.NewString()).Error; err != nil {
			return err
		}
		return db.Exec(`INSERT INTO mancuerna.auth_receipts(operation_id,attempt_id,purpose,request_hash,response_expires_at) VALUES(?,?,'signup',decode(repeat('c',64),'hex'),now()+interval '10 minutes')`, uuid.New(), id).Error
	})
	if err == nil {
		t.Fatal("mixed context inserted auth receipt")
	}
}

func TestPersonalPostgresRejectsUnsafePrivileges(t *testing.T) {
	owner := personalTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	data := personalTestPool(t, "MANCUERNA_TEST_DATA_DSN")
	if _, err := NewPersonalPostgresUnitOfWork(owner, PersonalDataRole); err == nil {
		t.Fatal("migration owner accepted as runtime")
	}
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	role := "mancuerna_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedRole := quote(role)
	if err := owner.Exec(`CREATE ROLE ` + quotedRole + ` LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Exec(`DROP OWNED BY ` + quotedRole); owner.Exec(`DROP ROLE ` + quotedRole) })
	if err := owner.Exec(`GRANT mancuerna_data TO ` + quotedRole).Error; err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("MANCUERNA_TEST_DATA_DSN"))
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	config.User = role
	config.Password = ""
	probeSQL := sql.OpenDB(stdlib.GetConnector(*config))
	probe, err := gorm.Open(postgres.New(postgres.Config{Conn: probeSQL}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("unable to open isolated probe role")
	}
	pool, _ := probe.DB()
	t.Cleanup(func() { pool.Close() })
	if _, err = NewPersonalPostgresUnitOfWork(probe, PersonalDataRole); err != nil {
		t.Fatal(err)
	}
	var database string
	if err = owner.Raw(`SELECT current_database()`).Row().Scan(&database); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, grant, revoke string }{
		{"schema create", `GRANT CREATE ON SCHEMA mancuerna TO `, `REVOKE CREATE ON SCHEMA mancuerna FROM `},
		{"database create", `GRANT CREATE ON DATABASE ` + quote(database) + ` TO `, `REVOKE CREATE ON DATABASE ` + quote(database) + ` FROM `},
		{"other pool", `GRANT mancuerna_auth TO `, `REVOKE mancuerna_auth FROM `},
		{"identity owner", `GRANT mancuerna_identity TO `, `REVOKE mancuerna_identity FROM `},
		{"gym data", `GRANT SELECT ON public.gyms TO `, `REVOKE SELECT ON public.gyms FROM `},
		{"secrets", `GRANT SELECT ON mancuerna.sessions TO `, `REVOKE SELECT ON mancuerna.sessions FROM `},
		{"hard delete", `GRANT DELETE ON mancuerna.accounts TO `, `REVOKE DELETE ON mancuerna.accounts FROM `},
		{"phone update", `GRANT UPDATE(phone_e164) ON mancuerna.accounts TO `, `REVOKE UPDATE(phone_e164) ON mancuerna.accounts FROM `},
		{"identity function", `GRANT EXECUTE ON FUNCTION mancuerna_private.resolve_identity(text,text) TO `, `REVOKE EXECUTE ON FUNCTION mancuerna_private.resolve_identity(text,text) FROM `},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := owner.Exec(test.grant + quotedRole).Error; err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := owner.Exec(test.revoke + quotedRole).Error; err != nil {
					t.Fatal(err)
				}
			}()
			if _, err := NewPersonalPostgresUnitOfWork(probe, PersonalDataRole); err == nil {
				t.Fatal("unsafe runtime privilege accepted")
			}
		})
	}
	t.Run("force RLS required", func(t *testing.T) {
		table := `mancuerna.` + quote("integration_policy_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
		if err := owner.Exec(`CREATE TABLE ` + table + `(account_id uuid); ALTER TABLE ` + table + ` ENABLE ROW LEVEL SECURITY; CREATE POLICY personal_owner ON ` + table + ` USING(account_id=mancuerna_private.account_id())`).Error; err != nil {
			t.Fatal(err)
		}
		defer owner.Exec(`DROP TABLE ` + table)
		if _, err := NewPersonalPostgresUnitOfWork(data, PersonalDataRole); err == nil {
			t.Fatal("non-forced private RLS accepted")
		}
		if err := owner.Exec(`ALTER TABLE ` + table + ` FORCE ROW LEVEL SECURITY`).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := NewPersonalPostgresUnitOfWork(data, PersonalDataRole); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPersonalPostgresTenantUoWRejectsPersonalContext(t *testing.T) {
	db := personalTestPool(t, "MANCUERNA_TEST_DATA_DSN")
	// The strict implementation is the same one constructed by the Tinta
	// runtime. No table access is needed to prove its context boundary.
	uow := &tenantUnitOfWork{gormUnitOfWork: &gormUnitOfWork{db: db, strict: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, gym := uuid.New(), uuid.New()
	for _, scope := range []context.Context{
		WithPersonalAccount(ctx, a), WithPersonalAccount(ctx, uuid.Nil),
		WithTenant(WithPersonalAccount(ctx, a), gym), WithPersonalAccount(WithTenant(ctx, gym), a),
	} {
		if tx, err := uow.Begin(scope); !errors.Is(err, ErrTenantScope) {
			if tx != nil {
				_ = uow.Rollback(tx)
			}
			t.Fatalf("Begin accepted personal context: %v", err)
		}
		if tx, err := uow.Query(scope); !errors.Is(err, ErrTenantScope) {
			CloseQuery(tx)
			t.Fatalf("Query accepted personal context: %v", err)
		}
		if err := ReadSnapshot(scope, uow, func(Transaction) error { t.Fatal("snapshot callback received personal context"); return nil }); !errors.Is(err, ErrTenantScope) {
			t.Fatalf("snapshot accepted personal context: %v", err)
		}
	}
	tx, err := uow.Query(WithTenant(ctx, gym))
	if err != nil {
		t.Fatal(err)
	}
	CloseQuery(tx)
}
