//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestCommandCapturePostgresExactMetadataReplayAndIndependentServerTime(t *testing.T) {
	auth := finalizationDependencies(t)
	owner, actor, _ := meTestFixture(t, auth)
	d := profileCommandDependencies(t)
	u := profileCommandUseCase(t, d)
	ctx := journalBoundaryContext(t, actor)
	base := "1"
	for _, captured := range []string{"2000-02-29T12:34:56Z", "2099-01-01T00:00:00.000Z"} {
		command := profileCommand(actor, base, "Captura independiente")
		command.CapturedAt = captured
		result, err := u.Execute(context.Background(), actor, command)
		if err != nil || result.Outcome != "accepted" {
			t.Fatal("capture incorrectly ordered or rejected", err)
		}
		if err = func() error {
			tx, err := d.UoW.Query(ctx)
			if err != nil {
				return err
			}
			defer d.UoW.Rollback(tx)
			r, err := d.Commands.GetReceipt(tx, actor.AccountID, uuid.MustParse(command.OperationID))
			if err != nil {
				return err
			}
			client, _ := protocol.ParseCapturedAt(captured)
			if r.CapturedAt != captured || r.CommittedAt.Equal(client) || protocol.Timestamp(r.CommittedAt) != result.CommittedAt {
				t.Fatal("capture text rewritten or confused with server commit")
			}
			return nil
		}(); err != nil {
			t.Fatal(err)
		}
		replay, err := u.Execute(context.Background(), actor, command)
		if err != nil || !reflect.DeepEqual(result, replay) {
			t.Fatal("capture replay changed", err)
		}
		changed := command
		changed.CapturedAt = "2001-01-01T00:00:00Z"
		if _, err = u.Execute(context.Background(), actor, changed); !errors.Is(err, protocol.ErrOperationMismatch) {
			t.Fatal("changed capture reused receipt", err)
		}
		base = result.Affected[0].Revision
	}
	journalBoundaryEffects(t, owner, actor.AccountID, 2, 2, 2, 2)
}

func TestCommandCapturePostgresNewRowsRequiredLegacyNullAndRollback(t *testing.T) {
	auth := finalizationDependencies(t)
	owner, actor, _ := meTestFixture(t, auth)
	d := profileCommandDependencies(t)
	ctx := journalBoundaryContext(t, actor)
	for _, value := range []string{"", "2026-02-30T10:00:00Z", "2026-01-01T24:00:00Z", "2026-01-01T00:00:00.0000Z", "2026-01-01T00:00:00+00:00"} {
		r := journalBoundaryReceipt(actor, uuid.New())
		r.CapturedAt = value
		r.Result = protocol.Failure(r.OperationID, "rejected", "VALIDATION_ERROR", "Fixture", "none", false)
		err := d.UoW.Command(ctx, func(tx shared.Transaction) error { return d.Commands.Commit(tx, r, nil) })
		if !errors.Is(err, protocol.ErrCommandInvalid) {
			t.Fatal("invalid capture reached storage", err)
		}
	}
	r := journalBoundaryReceipt(actor, uuid.New())
	r.Result = protocol.Failure(r.OperationID, "rejected", "VALIDATION_ERROR", "Fixture", "none", false)
	raw, _ := json.Marshal(r.Result)
	insert := `INSERT INTO mancuerna.operation_receipts(operation_id,account_id,actor_device_id,origin_device_id,request_hash,command_type,outcome,result,committed_at,captured_at) VALUES(?,?,?,?,?,?,?,?::jsonb,?,?)`
	for _, value := range []any{nil, "", "2026-02-30T10:00:00Z", "2026-01-01T24:00:00Z", "2026-01-01T00:00:60Z"} {
		if err := owner.Exec(insert, r.OperationID, r.AccountID, r.ActorDeviceID, r.OriginDeviceID, r.RequestHash[:], r.CommandType, r.Result.Outcome, string(raw), r.CommittedAt, value).Error; err == nil {
			t.Fatal("database accepted absent or invalid capture")
		}
	}
	// Model an existing receipt from schema 5 inside a transaction. Restore the
	// actual NOT VALID constraint, read through the restricted data role, then
	// roll back ALL fixture DDL and rows. Never backfill an invented timestamp.
	rolledBack := errors.New("fixture legacy rollback")
	err := owner.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE mancuerna.operation_receipts DROP CONSTRAINT command_capture_required").Error; err != nil {
			return err
		}
		if err := tx.Exec(insert, r.OperationID, r.AccountID, r.ActorDeviceID, r.OriginDeviceID, r.RequestHash[:], r.CommandType, r.Result.Outcome, string(raw), r.CommittedAt, nil).Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE mancuerna.operation_receipts ADD CONSTRAINT command_capture_required CHECK(captured_at IS NOT NULL) NOT VALID").Error; err != nil {
			return err
		}
		if err := tx.Exec("SET LOCAL ROLE mancuerna_data").Error; err != nil {
			return err
		}
		if err := tx.Exec("SELECT set_config('mancuerna.account_id',?,true)", actor.AccountID.String()).Error; err != nil {
			return err
		}
		stored, err := d.Commands.GetReceipt(&shared.GormTransaction{Tx: tx}, actor.AccountID, r.OperationID)
		if err != nil {
			return err
		}
		if stored.CapturedAt != "" || !reflect.DeepEqual(stored.Result, r.Result) {
			t.Fatal("legacy capture fabricated or result changed")
		}
		if err := stored.Match(r.RequestHash, r.CommandType, r.OriginDeviceID); err != nil {
			return err
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) {
		t.Fatal("legacy metadata compatibility failed", err)
	}
	journalBoundaryEffects(t, owner, actor.AccountID, 0, 0, 0, 0)
}
