//go:build server && integration

package repositories

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Only explicit disposable harness DSNs are accepted. DATABASE_URL, .env and
// any remote host/fallback are deliberately ignored, including for fixtures.
func profileTestPool(t *testing.T, environment string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(environment)
	if dsn == "" {
		t.Skip(environment + " not configured; run tests/mancuerna/profile_run.py")
	}
	configuration, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid isolated profile database configuration")
	}
	local := func(host string) bool {
		if filepath.IsAbs(host) || host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	if !local(configuration.Host) || !strings.HasPrefix(configuration.Database, "mancuerna_") || !strings.HasSuffix(configuration.Database, "test") {
		t.Fatal("profile tests require an explicit local Mancuerna test database")
	}
	for _, fallback := range configuration.Fallbacks {
		if !local(fallback.Host) {
			t.Fatal("profile tests require local fallback hosts")
		}
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("isolated profile database unavailable")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(4)
	pool.SetMaxIdleConns(4)
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

var persistedAt = time.Date(2026, 10, 19, 1, 0, 0, 123000000, time.UTC)

func profileFixture(t *testing.T, authDB *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	phone := fmt.Sprintf("+5255%09d", binary.BigEndian.Uint32(id[:4])%1000000000)
	err := authDB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT set_config('tinta.gym_id','',true),set_config('mancuerna.account_id',?,true),set_config('mancuerna.attempt_id','',true),set_config('mancuerna.attempt_hash','',true)`, id.String()).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO mancuerna.accounts(id,phone_e164,phone_verified_at,display_name,weight_unit,security_version,created_at,updated_at) VALUES(?,?,?,'Initial','kg',7,?,?)`, id, phone, persistedAt, persistedAt, persistedAt).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO mancuerna.sync_streams(account_id,epoch) VALUES(?,?)`, id, uuid.New()).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func profileRead(t *testing.T, uow sharedDomain.UnitOfWork, owner uuid.UUID) *accountDomain.Account {
	t.Helper()
	tx, err := uow.Query(sharedDomain.WithPersonalAccount(context.Background(), owner))
	if err != nil {
		t.Fatal(err)
	}
	defer sharedDomain.CloseQuery(tx)
	a, err := NewAccountPostgresRepository().GetByID(tx, owner)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestProfilePostgresCASIsolationAndRollback(t *testing.T) {
	dataDB := profileTestPool(t, "MANCUERNA_TEST_DATA_DSN")
	authDB := profileTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	data, err := sharedDomain.NewPersonalPostgresUnitOfWork(dataDB, sharedDomain.PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sharedDomain.NewPersonalPostgresUnitOfWork(authDB, sharedDomain.PersonalAuthRole); err != nil {
		t.Fatal(err)
	}
	a, b := profileFixture(t, authDB), profileFixture(t, authDB)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repository := NewAccountPostgresRepository()
	query, err := data.Query(sharedDomain.WithPersonalAccount(ctx, a))
	if err != nil {
		t.Fatal(err)
	}
	_, foreignErr := repository.GetByID(query, b)
	_, missingErr := repository.GetByID(query, uuid.New())
	sharedDomain.CloseQuery(query)
	if !errors.Is(foreignErr, personalErrors.ErrAccountNotFound) || !errors.Is(missingErr, personalErrors.ErrAccountNotFound) || foreignErr.Error() != missingErr.Error() {
		t.Fatal("foreign and missing profile were distinguishable", foreignErr, missingErr)
	}
	first := profileRead(t, data, a)
	stale := *first
	if err := first.UpdateProfile("  Ana  ", accountDomain.Pounds, 1, persistedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Mutating non-editable aggregate metadata must not widen the SQL update.
	first.CreatedAt = persistedAt.Add(-time.Hour)
	var saved *accountDomain.Account
	err = data.Command(sharedDomain.WithPersonalAccount(ctx, a), func(tx sharedDomain.Transaction) error {
		var err error
		saved, err = repository.UpdateProfile(tx, a, first, 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.DisplayName != "Ana" || saved.WeightUnit != accountDomain.Pounds || saved.Revision != 2 || !saved.CreatedAt.Equal(persistedAt) || saved.UpdatedAt.Location() != time.UTC {
		t.Fatal("CAS lost profile data or changed creation metadata", saved)
	}
	if err := stale.UpdateProfile("Stale", accountDomain.Kilograms, 1, persistedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	err = data.Command(sharedDomain.WithPersonalAccount(ctx, a), func(tx sharedDomain.Transaction) error {
		_, err := repository.UpdateProfile(tx, a, &stale, 1)
		return err
	})
	if !errors.Is(err, personalErrors.ErrProfileRevisionConflict) {
		t.Fatal("stale CAS accepted", err)
	}
	for _, test := range []struct{ scope, owner uuid.UUID }{{b, a}, {a, b}} {
		err = data.Command(sharedDomain.WithPersonalAccount(ctx, test.scope), func(tx sharedDomain.Transaction) error {
			_, err := repository.UpdateProfile(tx, test.owner, &stale, 1)
			return err
		})
		if !errors.Is(err, personalErrors.ErrAccountNotFound) {
			t.Fatal("foreign write was not hidden/rejected", err)
		}
	}
	rollback := errors.New("synthetic later journal failure")
	proposal := *saved
	if err := proposal.UpdateProfile("Must roll back", accountDomain.Kilograms, 2, persistedAt.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	err = data.Command(sharedDomain.WithPersonalAccount(ctx, a), func(tx sharedDomain.Transaction) error {
		if _, err := repository.UpdateProfile(tx, a, &proposal, 2); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	current := profileRead(t, data, a)
	if current.DisplayName != "Ana" || current.Revision != 2 || current.WeightUnit != accountDomain.Pounds {
		t.Fatal("repository committed outside caller transaction", current)
	}
	if other := profileRead(t, data, b); other.DisplayName != "Initial" || other.Revision != 1 || other.WeightUnit != accountDomain.Kilograms {
		t.Fatal("other account changed", other)
	}
	// The successful write ran as the restricted data login. Read auth metadata
	// through the separate restricted auth login, not a schema owner shortcut.
	err = authDB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT set_config('tinta.gym_id','',true),set_config('mancuerna.account_id',?,true)`, a.String()).Error; err != nil {
			return err
		}
		var unchanged bool
		expectedPhone := fmt.Sprintf("+5255%09d", binary.BigEndian.Uint32(a[:4])%1000000000)
		if err := tx.Raw(`SELECT phone_e164=? AND phone_verified_at=? AND security_version=7 AND status='active' AND deleted_at IS NULL AND created_at=? FROM mancuerna.accounts WHERE id=?`, expectedPhone, persistedAt, persistedAt, a).Row().Scan(&unchanged); err != nil {
			return err
		}
		if !unchanged {
			return errors.New("profile write changed identity/security metadata")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProfilePostgresConcurrentWritersOnlyOneWins(t *testing.T) {
	dataDB := profileTestPool(t, "MANCUERNA_TEST_DATA_DSN")
	authDB := profileTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	data, err := sharedDomain.NewPersonalPostgresUnitOfWork(dataDB, sharedDomain.PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	id := profileFixture(t, authDB)
	original := profileRead(t, data, id)
	ctx, cancel := context.WithTimeout(sharedDomain.WithPersonalAccount(context.Background(), id), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	done := make(chan error, 2)
	repository := NewAccountPostgresRepository()
	for _, name := range []string{"Writer A", "Writer B"} {
		proposal := *original
		if err := proposal.UpdateProfile(name, accountDomain.Pounds, 1, persistedAt.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		go func(proposal accountDomain.Account) {
			<-start
			done <- data.Command(ctx, func(tx sharedDomain.Transaction) error {
				_, err := repository.UpdateProfile(tx, id, &proposal, 1)
				return err
			})
		}(proposal)
	}
	close(start)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-done
		if err == nil {
			wins++
		} else if errors.Is(err, personalErrors.ErrProfileRevisionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", wins, conflicts)
	}
	current := profileRead(t, data, id)
	if current.Revision != 2 || (current.DisplayName != "Writer A" && current.DisplayName != "Writer B") {
		t.Fatal("concurrent profile lost CAS", current)
	}
}

func TestProfilePostgresInactiveAndSoftDeletedCannotWrite(t *testing.T) {
	dataDB := profileTestPool(t, "MANCUERNA_TEST_DATA_DSN")
	authDB := profileTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	data, err := sharedDomain.NewPersonalPostgresUnitOfWork(dataDB, sharedDomain.PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := sharedDomain.NewPersonalPostgresUnitOfWork(authDB, sharedDomain.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	for _, softDelete := range []bool{false, true} {
		id := profileFixture(t, authDB)
		proposal := profileRead(t, data, id)
		if err := proposal.UpdateProfile("Stale active state", accountDomain.Pounds, 1, persistedAt.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		ctx := sharedDomain.WithPersonalAccount(context.Background(), id)
		err = auth.Command(ctx, func(tx sharedDomain.Transaction) error {
			statement := `UPDATE mancuerna.accounts SET status='deletion_pending',deletion_requested_at=now() WHERE id=?`
			if softDelete {
				statement = `UPDATE mancuerna.accounts SET deleted_at=now() WHERE id=?`
			}
			return tx.(*sharedDomain.GormTransaction).Tx.Exec(statement, id).Error
		})
		if err != nil {
			t.Fatal(err)
		}
		err = data.Command(ctx, func(tx sharedDomain.Transaction) error {
			_, err := NewAccountPostgresRepository().UpdateProfile(tx, id, proposal, 1)
			return err
		})
		want := personalErrors.ErrAccountUnavailable
		if softDelete {
			want = personalErrors.ErrAccountNotFound
		}
		if !errors.Is(err, want) {
			t.Fatal("stale active aggregate bypassed persisted status", err)
		}
	}
}
