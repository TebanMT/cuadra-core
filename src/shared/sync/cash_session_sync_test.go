//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	cashRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

func seedCashSessionSyncActor(t *testing.T, db *sqlx.DB, gymID uuid.UUID) uuid.UUID {
	t.Helper()
	actorID := uuid.New()
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`INSERT INTO users(
		id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role,active)
		VALUES(?,?,1,?,?,'cash@gym.test','x','Cash Operator','operator',1)`,
		actorID, gymID, now, now); err != nil {
		t.Fatalf("seed cash actor: %v", err)
	}
	return actorID
}

func TestApplyPullCashSession_NormalizesLegacyOpeningToOperationalDayStart(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	actorID := seedCashSessionSyncActor(t, db, gymID)
	entityID := uuid.New()
	created := time.Date(2026, 8, 24, 22, 30, 0, 0, time.UTC)
	payload, _ := json.Marshal(map[string]any{
		"id": entityID.String(), "gym_id": gymID.String(), "version": 2,
		"created_at": created.UnixMilli(), "updated_at": created.UnixMilli(), "deleted_at": nil,
		"close_date": "2026-08-24", "calculated_cash": 100.0, "counted_cash": 100.0,
		"discrepancy_reason": nil, "closed_by": actorID.String(),
	})
	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, syncpkg.PullChange{
			EntityType: "cash_close_events", EntityID: entityID.String(), Version: 2,
			Payload: payload, ServerUpdatedAt: created,
		})
	})
	if err != nil {
		t.Fatalf("apply legacy session: %v", err)
	}
	var row struct {
		OpenedAt         int64  `db:"opened_at"`
		OpeningCashKnown int    `db:"opening_cash_known"`
		Status           string `db:"status"`
		Sequence         int    `db:"sequence"`
	}
	if err := db.Get(&row, `SELECT opened_at,opening_cash_known,status,sequence
		FROM cash_close_events WHERE id=?`, entityID.String()); err != nil {
		t.Fatal(err)
	}
	wantStart := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC).UnixMilli()
	if row.OpenedAt != wantStart || row.OpeningCashKnown != 0 ||
		row.Status != cashCloseDomain.StatusReconciled || row.Sequence != 1 {
		t.Fatalf("normalized legacy row = %+v, want day-start/unknown/reconciled/seq1", row)
	}
}

func TestApplyPullCashSession_RekeysNewerPendingNaturalSlotToCanonicalID(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	actorID := seedCashSessionSyncActor(t, db, gymID)
	day := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	localTime := day.Add(12 * time.Hour)
	localID := uuid.New()
	counted := 50.0
	local := cashCloseDomain.New(localID, gymID, actorID, day, 50, &counted, nil, localTime)
	repo := cashRepoLite.NewCashCloseEventSQLiteRepository()
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := repo.Create(tx, local)
		return err
	}); err != nil {
		t.Fatalf("seed pending session: %v", err)
	}

	canonicalID := cashCloseDomain.DeterministicSessionID(gymID, gymID, day, 1)
	incomingTime := localTime.Add(-time.Hour)
	payload, _ := json.Marshal(map[string]any{
		"id": canonicalID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": day.UnixMilli(), "updated_at": incomingTime.UnixMilli(), "deleted_at": nil,
		"drawer_id": gymID.String(), "drawer_code": "main", "operational_date": "2026-08-24",
		"sequence": 1, "status": cashCloseDomain.StatusReconciled,
		"close_date": "2026-08-24", "opening_cash": 0.0, "opening_cash_known": true,
		"activity_cash": 40.0, "calculated_cash": 40.0, "counted_cash": 40.0,
		"adjusted_after_withdrawal": false, "opened_at": day.UnixMilli(), "opened_by": actorID.String(),
		"closed_at": incomingTime.UnixMilli(), "closed_by": actorID.String(),
		"reconciled_at": incomingTime.UnixMilli(), "reconciled_by": actorID.String(),
	})
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, syncpkg.PullChange{
			EntityType: "cash_close_events", EntityID: canonicalID.String(), Version: 1,
			Payload: payload, ServerUpdatedAt: incomingTime,
		})
	}); err != nil {
		t.Fatalf("apply colliding canonical session: %v", err)
	}

	var live struct {
		ID             string `db:"id"`
		Version        int    `db:"version"`
		CalculatedCash int64  `db:"calculated_cash"`
	}
	if err := db.Get(&live, `SELECT id,version,calculated_cash FROM cash_close_events
		WHERE gym_id=? AND drawer_id=? AND operational_date='2026-08-24'
		  AND sequence=1 AND deleted_at IS NULL`, gymID.String(), gymID.String()); err != nil {
		t.Fatal(err)
	}
	if live.ID != canonicalID.String() || live.Version != 4 || live.CalculatedCash != 5000 {
		t.Fatalf("rekeyed live session = %+v, want canonical ID preserving newer local $50", live)
	}
	var queuedID, payloadID string
	if err := db.QueryRow(`SELECT entity_id,json_extract(payload,'$.id') FROM sync_queue
		WHERE entity_type='cash_close_events' AND synced_at IS NULL`).Scan(&queuedID, &payloadID); err != nil {
		t.Fatal(err)
	}
	if queuedID != canonicalID.String() || payloadID != canonicalID.String() {
		t.Fatalf("queued canonical IDs = %q/%q", queuedID, payloadID)
	}
}

func TestApplyPullCashSession_RekeysPendingWithdrawalTransferWithSession(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	actorID := seedCashSessionSyncActor(t, db, gymID)
	day := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	localTime := day.Add(12 * time.Hour)

	local, err := cashCloseDomain.Open(gymID, gymID, actorID, day, 1, 0, day)
	if err != nil {
		t.Fatal(err)
	}
	local.ID = uuid.New() // simulates a pre-deterministic/offline session
	if err := local.Close(50, actorID, localTime.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := local.Reconcile(50, nil, actorID, localTime.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := local.Withdraw(5, cashCloseDomain.DestinationGymFund, actorID, localTime); err != nil {
		t.Fatal(err)
	}
	transfer := cashCloseDomain.NewTransfer(local)
	repo := cashRepoLite.NewCashCloseEventSQLiteRepository()
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		if _, createErr := repo.Create(tx, local); createErr != nil {
			return createErr
		}
		_, createErr := repo.CreateTransfer(tx, transfer)
		return createErr
	}); err != nil {
		t.Fatalf("seed pending withdrawal: %v", err)
	}

	canonicalID := cashCloseDomain.DeterministicSessionID(gymID, gymID, day, 1)
	incomingTime := localTime.Add(-time.Hour)
	payload, _ := json.Marshal(map[string]any{
		"id": canonicalID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": day.UnixMilli(), "updated_at": incomingTime.UnixMilli(), "deleted_at": nil,
		"drawer_id": gymID.String(), "drawer_code": "main", "operational_date": "2026-08-24",
		"sequence": 1, "status": cashCloseDomain.StatusReconciled,
		"close_date": "2026-08-24", "opening_cash": 0.0, "opening_cash_known": true,
		"activity_cash": 40.0, "calculated_cash": 40.0, "counted_cash": 40.0,
		"adjusted_after_withdrawal": false, "opened_at": day.UnixMilli(), "opened_by": actorID.String(),
		"closed_at": incomingTime.UnixMilli(), "closed_by": actorID.String(),
		"reconciled_at": incomingTime.UnixMilli(), "reconciled_by": actorID.String(),
	})
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, syncpkg.PullChange{
			EntityType: "cash_close_events", EntityID: canonicalID.String(), Version: 1,
			Payload: payload, ServerUpdatedAt: incomingTime,
		})
	}); err != nil {
		t.Fatalf("apply colliding canonical withdrawal: %v", err)
	}

	wantTransferID := cashCloseDomain.DeterministicTransferID(canonicalID).String()
	var row struct {
		ID        string `db:"id"`
		SessionID string `db:"session_id"`
		Version   int    `db:"version"`
	}
	if err := db.Get(&row, `SELECT id,session_id,version FROM cash_transfers
		WHERE gym_id=? AND deleted_at IS NULL`, gymID.String()); err != nil {
		t.Fatal(err)
	}
	if row.ID != wantTransferID || row.SessionID != canonicalID.String() || row.Version != 2 {
		t.Fatalf("rekeyed transfer = %+v, want id=%s session=%s version=2", row, wantTransferID, canonicalID)
	}

	var queued struct {
		EntityID  string `db:"entity_id"`
		PayloadID string `db:"payload_id"`
		SessionID string `db:"session_id"`
		Version   int    `db:"version"`
	}
	if err := db.Get(&queued, `SELECT entity_id,
		json_extract(payload,'$.id') AS payload_id,
		json_extract(payload,'$.session_id') AS session_id,
		json_extract(payload,'$.version') AS version
		FROM sync_queue WHERE entity_type='cash_transfers' AND synced_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if queued.EntityID != wantTransferID || queued.PayloadID != wantTransferID ||
		queued.SessionID != canonicalID.String() || queued.Version != 2 {
		t.Fatalf("rekeyed queued transfer = %+v", queued)
	}
}
