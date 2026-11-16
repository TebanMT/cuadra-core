//go:build server && integration

package repositories

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSnapshotCommandPostgresWaitedRevocationRejectsStaleView(t *testing.T) {
	auth := finalizationDependencies(t)
	owner, actor, _ := meTestFixture(t, auth)
	data := profileCommandDependencies(t).UoW
	snapshots, ok := data.(shared.PersonalSnapshotUnitOfWork)
	if !ok {
		t.Fatal("strict snapshot capability missing")
	}
	ctx, cancel := context.WithTimeout(shared.WithPersonalAccount(context.Background(), actor.AccountID), 8*time.Second)
	defer cancel()
	held, err := auth.UoW.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer auth.UoW.Rollback(held)
	if err = auth.Sessions.RevokeSession(held, actor.AccountID, actor.SessionID, time.Now()); err != nil {
		t.Fatal(err)
	}
	var called atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- snapshots.SnapshotCommand(ctx, func(shared.Transaction) error { called.Store(true); return nil })
	}()
	// Wait for the actual database lock, not a scheduling assumption. The RR
	// view already exists when this UPDATE waits behind the revocation.
	deadline := time.Now().Add(4 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		if err = owner.Raw("SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename='mc_data' AND wait_event_type='Lock' AND query LIKE 'UPDATE mancuerna.sync_streams SET last_sequence=last_sequence%')").Scan(&waiting).Error; err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("snapshot did not reach account mutex")
	}
	if err = auth.UoW.Commit(held); err != nil {
		t.Fatal(err)
	}
	err = <-done
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "40001" || called.Load() {
		t.Fatalf("stale RR view reached materializer: %v called=%v", err, called.Load())
	}
	// A fresh retry sees the committed revocation. Never rerun only the failed
	// SQL statement inside the old view or expose pages from that transaction.
	err = snapshots.SnapshotCommand(ctx, func(tx shared.Transaction) error {
		var active bool
		if err := tx.(*shared.GormTransaction).Tx.Raw("SELECT mancuerna_private.session_active(?,?)", actor.SessionID, actor.DeviceID).Scan(&active).Error; err != nil {
			return err
		}
		if active {
			t.Error("fresh snapshot saw revoked authority")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq := accessTestCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", actor.AccountID); seq != 0 {
		t.Fatal("mutex touch advanced journal", seq)
	}
}
func TestSnapshotCommandPostgresStrictScopeRollbackAndConsistentView(t *testing.T) {
	auth := finalizationDependencies(t)
	owner, actor, _ := meTestFixture(t, auth)
	data := profileCommandDependencies(t).UoW
	snapshots := data.(shared.PersonalSnapshotUnitOfWork)
	for _, ctx := range []context.Context{context.Background(), shared.WithTenant(context.Background(), uuid.New()), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1})} {
		if err := snapshots.SnapshotCommand(ctx, func(shared.Transaction) error { t.Error("invalid scope reached materializer"); return nil }); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("scope not rejected", err)
		}
	}
	ctx := shared.WithPersonalAccount(context.Background(), actor.AccountID)
	if err := auth.UoW.(shared.PersonalSnapshotUnitOfWork).SnapshotCommand(ctx, func(shared.Transaction) error { t.Error("auth pool materialized content"); return nil }); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("auth snapshot accepted", err)
	}
	failure := errors.New("synthetic materialization failure")
	err := snapshots.SnapshotCommand(ctx, func(tx shared.Transaction) error {
		db := tx.(*shared.GormTransaction).Tx
		var isolation, readOnly string
		if err := db.Raw("SELECT current_setting('transaction_isolation'),current_setting('transaction_read_only')").Row().Scan(&isolation, &readOnly); err != nil {
			return err
		}
		if isolation != "repeatable read" || readOnly != "off" {
			t.Error("snapshot downgraded")
		}
		if err := db.Exec("UPDATE mancuerna.accounts SET display_name='must rollback' WHERE id=?", actor.AccountID).Error; err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal("materialization error lost", err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=? AND display_name='must rollback'", actor.AccountID) != 0 {
		t.Fatal("partial snapshot committed")
	}
}
