//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type RefundSQLiteRepository struct{}

func NewRefundSQLiteRepository() *RefundSQLiteRepository { return &RefundSQLiteRepository{} }

type refundSQLiteRow struct {
	ID                     string         `db:"id"`
	GymID                  string         `db:"gym_id"`
	Version                int            `db:"version"`
	CreatedAt              int64          `db:"created_at"`
	UpdatedAt              int64          `db:"updated_at"`
	DeletedAt              sql.NullInt64  `db:"deleted_at"`
	SyncedAt               sql.NullInt64  `db:"synced_at"`
	RootPaymentID          string         `db:"root_payment_id"`
	RefundPaymentID        sql.NullString `db:"refund_payment_id"`
	SaleID                 sql.NullString `db:"sale_id"`
	Amount                 int64          `db:"amount"`
	Method                 sql.NullString `db:"method"`
	RefundedOn             string         `db:"refunded_on"`
	Reason                 string         `db:"reason"`
	Kind                   string         `db:"kind"`
	BalanceCancelled       int64          `db:"balance_cancelled"`
	LegacyIncomplete       int            `db:"legacy_incomplete"`
	CorrectionID           sql.NullString `db:"correction_id"`
	IdempotencyKey         string         `db:"idempotency_key"`
	IdempotencyFingerprint string         `db:"idempotency_fingerprint"`
	IdempotencyResult      sql.NullString `db:"idempotency_result"`
	CreatedBy              string         `db:"created_by"`
}

const refundColumns = `id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
root_payment_id,refund_payment_id,sale_id,amount,method,refunded_on,reason,kind,
balance_cancelled,legacy_incomplete,correction_id,idempotency_key,idempotency_fingerprint,idempotency_result,created_by`

func (r *RefundSQLiteRepository) Create(tx sharedDomain.Transaction, aggregate *refundDomain.Refund) (*refundDomain.Refund, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := refundToSQLite(aggregate)
	stmt := `INSERT INTO refunds(` + refundColumns + `) VALUES(
		:id,:gym_id,:version,:created_at,:updated_at,:deleted_at,:synced_at,
		:root_payment_id,:refund_payment_id,:sale_id,:amount,:method,:refunded_on,:reason,:kind,
		:balance_cancelled,:legacy_incomplete,:correction_id,:idempotency_key,:idempotency_fingerprint,:idempotency_result,:created_by)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueRefund(stx, aggregate); err != nil {
		return nil, err
	}
	for _, item := range aggregate.Items {
		if _, err := stx.Exec(context.Background(), `
			INSERT INTO refund_items(id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
			 refund_id,sale_item_id,quantity,amount,disposition)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			item.ID.String(), item.GymID.String(), item.Version, item.CreatedAt.UnixMilli(),
			item.UpdatedAt.UnixMilli(), nil, nil, item.RefundID.String(), item.SaleItemID.String(),
			item.Quantity, toCents(item.Amount), item.Disposition); err != nil {
			return nil, err
		}
		if err := enqueueRefundItem(stx, item); err != nil {
			return nil, err
		}
	}
	if err := tagRefundSyncGraph(stx, aggregate); err != nil {
		return nil, err
	}
	return aggregate, nil
}

func (r *RefundSQLiteRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*refundDomain.Refund, error) {
	var row refundSQLiteRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row,
		`SELECT `+refundColumns+` FROM refunds WHERE gym_id=? AND idempotency_key=? AND deleted_at IS NULL`,
		gymID.String(), key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return refundFromSQLite(row), nil
}

func (r *RefundSQLiteRepository) RefundedQuantitiesBySale(tx sharedDomain.Transaction, gymID, saleID uuid.UUID) (map[uuid.UUID]int, error) {
	type row struct {
		SaleItemID string `db:"sale_item_id"`
		Quantity   int    `db:"quantity"`
	}
	var rows []row
	err := tx.(*sharedDomain.SqlxTransaction).Select(context.Background(), &rows, `
		SELECT ri.sale_item_id, SUM(ri.quantity) AS quantity
		FROM refund_items ri
		JOIN refunds r ON r.id=ri.refund_id AND r.deleted_at IS NULL
		WHERE ri.gym_id=? AND r.sale_id=? AND ri.deleted_at IS NULL
		GROUP BY ri.sale_item_id`, gymID.String(), saleID.String())
	out := make(map[uuid.UUID]int, len(rows))
	for _, x := range rows {
		id, _ := uuid.Parse(x.SaleItemID)
		out[id] = x.Quantity
	}
	return out, err
}

func refundToSQLite(r *refundDomain.Refund) refundSQLiteRow {
	row := refundSQLiteRow{
		ID: r.ID.String(), GymID: r.GymID.String(), Version: r.Version,
		CreatedAt: r.CreatedAt.UnixMilli(), UpdatedAt: r.UpdatedAt.UnixMilli(),
		RootPaymentID: r.RootPaymentID.String(),
		Amount:        toCents(r.Amount), RefundedOn: r.RefundedOn.Format(dateLayout),
		Reason: r.Reason, Kind: r.Kind, BalanceCancelled: toCents(r.BalanceCancelled),
		IdempotencyKey: r.IdempotencyKey, IdempotencyFingerprint: r.IdempotencyFingerprint,
		IdempotencyResult: sql.NullString{String: string(r.IdempotencyResult), Valid: len(r.IdempotencyResult) > 0},
		CreatedBy:         r.CreatedBy.String(),
	}
	if r.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: r.DeletedAt.UnixMilli(), Valid: true}
	}
	if r.RefundPaymentID != nil {
		row.RefundPaymentID = sql.NullString{String: r.RefundPaymentID.String(), Valid: true}
	}
	if r.Method != "" {
		row.Method = sql.NullString{String: r.Method, Valid: true}
	}
	if r.SaleID != nil {
		row.SaleID = sql.NullString{String: r.SaleID.String(), Valid: true}
	}
	if r.CorrectionID != nil {
		row.CorrectionID = sql.NullString{String: r.CorrectionID.String(), Valid: true}
	}
	if r.LegacyIncomplete {
		row.LegacyIncomplete = 1
	}
	return row
}

func refundFromSQLite(x refundSQLiteRow) *refundDomain.Refund {
	id, _ := uuid.Parse(x.ID)
	gymID, _ := uuid.Parse(x.GymID)
	rootID, _ := uuid.Parse(x.RootPaymentID)
	createdBy, _ := uuid.Parse(x.CreatedBy)
	day, _ := time.Parse(dateLayout, x.RefundedOn)
	r := &refundDomain.Refund{
		ID: id, GymID: gymID, Version: x.Version, RootPaymentID: rootID,
		Amount: fromCents(x.Amount), Method: x.Method.String,
		RefundedOn: day, Reason: x.Reason, Kind: x.Kind, BalanceCancelled: fromCents(x.BalanceCancelled),
		LegacyIncomplete: x.LegacyIncomplete != 0, IdempotencyKey: x.IdempotencyKey,
		IdempotencyFingerprint: x.IdempotencyFingerprint,
		CreatedBy:              createdBy, CreatedAt: time.UnixMilli(x.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(x.UpdatedAt).UTC(),
	}
	if x.IdempotencyResult.Valid {
		r.IdempotencyResult = append(json.RawMessage(nil), x.IdempotencyResult.String...)
	}
	if x.DeletedAt.Valid {
		v := time.UnixMilli(x.DeletedAt.Int64).UTC()
		r.DeletedAt = &v
	}
	if x.RefundPaymentID.Valid {
		v, _ := uuid.Parse(x.RefundPaymentID.String)
		r.RefundPaymentID = &v
	}
	if x.SaleID.Valid {
		v, _ := uuid.Parse(x.SaleID.String)
		r.SaleID = &v
	}
	if x.CorrectionID.Valid {
		v, _ := uuid.Parse(x.CorrectionID.String)
		r.CorrectionID = &v
	}
	return r
}

func enqueueRefund(stx *sharedDomain.SqlxTransaction, r *refundDomain.Refund) error {
	if stx.Queue == nil {
		return nil
	}
	var saleID, correctionID, refundPaymentID any
	if r.SaleID != nil {
		saleID = r.SaleID.String()
	}
	if r.CorrectionID != nil {
		correctionID = r.CorrectionID.String()
	}
	if r.RefundPaymentID != nil {
		refundPaymentID = r.RefundPaymentID.String()
	}
	payload, err := json.Marshal(map[string]any{
		"id": r.ID.String(), "gym_id": r.GymID.String(), "version": r.Version,
		"created_at": r.CreatedAt.UnixMilli(), "updated_at": r.UpdatedAt.UnixMilli(),
		"deleted_at": nil, "root_payment_id": r.RootPaymentID.String(),
		"refund_payment_id": refundPaymentID, "sale_id": saleID,
		"amount": r.Amount, "method": nullableRefundMethod(r.Method), "refunded_on": r.RefundedOn.Format(dateLayout),
		"reason": r.Reason, "kind": r.Kind, "balance_cancelled": r.BalanceCancelled,
		"legacy_incomplete": r.LegacyIncomplete, "correction_id": correctionID,
		"idempotency_key": r.IdempotencyKey, "idempotency_fingerprint": r.IdempotencyFingerprint,
		"idempotency_result": json.RawMessage(r.IdempotencyResult), "created_by": r.CreatedBy.String(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "refunds", r.ID.String(), "upsert", payload, r.Version)
}

func nullableRefundMethod(method string) any {
	if method == "" {
		return nil
	}
	return method
}

func enqueueRefundItem(stx *sharedDomain.SqlxTransaction, item *refundDomain.Item) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id": item.ID.String(), "gym_id": item.GymID.String(), "version": item.Version,
		"created_at": item.CreatedAt.UnixMilli(), "updated_at": item.UpdatedAt.UnixMilli(),
		"deleted_at": nil, "refund_id": item.RefundID.String(),
		"sale_item_id": item.SaleItemID.String(), "quantity": item.Quantity,
		"amount": item.Amount, "disposition": item.Disposition,
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "refund_items", item.ID.String(), "upsert", payload, item.Version)
}

// tagRefundSyncGraph correlates every financial queue snapshot produced by a
// refund command. The agent uses this metadata to expand a split batch and the
// cloud commits the connected component atomically. Unknown JSON keys are
// deliberately ignored by the row projectors and survive retries/full-sync.
func tagRefundSyncGraph(stx *sharedDomain.SqlxTransaction, aggregate *refundDomain.Refund) error {
	if stx.Queue == nil || aggregate == nil {
		return nil
	}
	const graphIDsKey = "_sync_graph_ids"
	const expectedItemsKey = "_sync_graph_expected_items"
	type entityKey struct{ kind, id string }
	targets := map[entityKey]bool{
		{kind: "payments", id: aggregate.RootPaymentID.String()}: true,
		{kind: "refunds", id: aggregate.ID.String()}:             true,
	}
	required := map[entityKey]bool{
		{kind: "payments", id: aggregate.RootPaymentID.String()}: false,
		{kind: "refunds", id: aggregate.ID.String()}:             false,
	}
	correctionSaleID := ""
	if aggregate.CorrectionID != nil {
		var correction struct {
			SaleID   string        `db:"sale_id"`
			SyncedAt sql.NullInt64 `db:"synced_at"`
		}
		if err := stx.Get(context.Background(), &correction, `SELECT sale_id,synced_at
			FROM sale_corrections WHERE gym_id=? AND id=? AND deleted_at IS NULL`,
			aggregate.GymID.String(), aggregate.CorrectionID.String()); err != nil {
			return fmt.Errorf("resolve refund sale correction: %w", err)
		}
		if !correction.SyncedAt.Valid {
			correctionSaleID = correction.SaleID
			for _, key := range []entityKey{
				{kind: "sale_corrections", id: aggregate.CorrectionID.String()},
				{kind: "sales", id: correction.SaleID},
			} {
				targets[key], required[key] = true, false
			}
		}
	}
	for _, item := range aggregate.Items {
		key := entityKey{kind: "refund_items", id: item.ID.String()}
		targets[key], required[key] = true, false
	}
	if aggregate.RefundPaymentID != nil {
		refundPaymentKey := entityKey{kind: "payments", id: aggregate.RefundPaymentID.String()}
		targets[refundPaymentKey], required[refundPaymentKey] = true, false
		var source struct {
			ID       string        `db:"id"`
			SyncedAt sql.NullInt64 `db:"synced_at"`
		}
		if err := stx.Get(context.Background(), &source, `
			SELECT parent.id,parent.synced_at
			FROM payments refund_payment
			JOIN payments parent ON parent.gym_id=refund_payment.gym_id
			 AND parent.id=refund_payment.parent_payment_id
			WHERE refund_payment.gym_id=? AND refund_payment.id=?`,
			aggregate.GymID.String(), aggregate.RefundPaymentID.String()); err != nil {
			return fmt.Errorf("resolve refund sync source: %w", err)
		}
		if !source.SyncedAt.Valid {
			sourceKey := entityKey{kind: "payments", id: source.ID}
			targets[sourceKey], required[sourceKey] = true, false
		}
	}

	type queueRow struct {
		QueueID    string `db:"id"`
		EntityType string `db:"entity_type"`
		EntityID   string `db:"entity_id"`
		Payload    string `db:"payload"`
	}
	var rows []queueRow
	if err := stx.Select(context.Background(), &rows, `
		SELECT id,entity_type,entity_id,payload FROM sync_queue
		WHERE synced_at IS NULL AND json_extract(payload,'$.gym_id')=?
		ORDER BY rowid`, aggregate.GymID.String()); err != nil {
		return fmt.Errorf("load refund sync graph: %w", err)
	}
	graphID := aggregate.ID.String()
	for _, row := range rows {
		key := entityKey{kind: row.EntityType, id: row.EntityID}
		var payload map[string]any
		if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
			return fmt.Errorf("decode refund graph queue row %s: %w", row.QueueID, err)
		}
		if !targets[key] {
			belongsToCorrectionChain := correctionSaleID != "" && payload["sale_id"] == correctionSaleID &&
				(row.EntityType == "sale_items" || row.EntityType == "sale_corrections")
			if !belongsToCorrectionChain {
				continue
			}
			targets[key] = true
		}
		seen := make(map[string]bool)
		var graphIDs []string
		if values, ok := payload[graphIDsKey].([]any); ok {
			for _, value := range values {
				if id, ok := value.(string); ok && id != "" && !seen[id] {
					seen[id] = true
					graphIDs = append(graphIDs, id)
				}
			}
		}
		if !seen[graphID] {
			graphIDs = append(graphIDs, graphID)
		}
		payload[graphIDsKey] = graphIDs
		if row.EntityType == "refunds" && row.EntityID == graphID {
			payload[expectedItemsKey] = len(aggregate.Items)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode refund graph queue row %s: %w", row.QueueID, err)
		}
		if _, err := stx.Exec(context.Background(), `
			UPDATE sync_queue SET payload=? WHERE id=? AND synced_at IS NULL`, string(encoded), row.QueueID); err != nil {
			return fmt.Errorf("tag refund graph queue row %s: %w", row.QueueID, err)
		}
		required[key] = true
	}
	for key, found := range required {
		if !found {
			return fmt.Errorf("refund sync graph %s is missing pending %s/%s", graphID, key.kind, key.id)
		}
	}
	return nil
}
