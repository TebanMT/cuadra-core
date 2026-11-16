//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	syncApp "github.com/cuadra/cuadra-core/src/shared/sync/personal/app"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func journalBoundaryContext(t *testing.T, actor security.AccessClaims) context.Context {
	t.Helper()
	ctx, err := shared.WithAuthenticatedPersonalAccount(context.Background(), actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func journalBoundaryReceipt(actor security.AccessClaims, operation uuid.UUID) protocol.Receipt {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return protocol.Receipt{OperationID: operation, AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID,
		OriginDeviceID: actor.DeviceID, RequestHash: sha256.Sum256([]byte(operation.String())), CommandType: "journal.fixture",
		CapturedAt:  protocol.Timestamp(now),
		CommittedAt: now, Result: protocol.OperationResult{OperationID: operation.String(), Outcome: "accepted", CommittedAt: protocol.Timestamp(now)}}
}

func journalBoundaryCanonical(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := (crypto.JSONCanonicalizer{}).Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

// Synthetic protocol records isolate byte accounting. They are deliberately
// not evidence that a product producer accepts a large exercise or routine.
func journalBoundaryChanges(t *testing.T, operation uuid.UUID, sequence int64, target int, prefix string, items []int) []protocol.Change {
	t.Helper()
	id := uuid.NewString()
	data := map[string]any{"id": id, "revision": "1", "text": prefix, "padding": "", "items": items}
	changes := []protocol.Change{{EntityType: "personal_exercise", EntityID: id, Revision: "1", Data: journalBoundaryCanonical(t, data)}}
	group := protocol.ChangeGroup{Sequence: strconv.FormatInt(sequence, 10), OperationID: operation.String(), Changes: changes}
	padding := target - len(journalBoundaryCanonical(t, group))
	if padding < 0 {
		t.Fatal("boundary fixture exceeds target before padding")
	}
	data["padding"] = strings.Repeat("x", padding)
	changes[0].Data = journalBoundaryCanonical(t, data)
	group.Changes = changes
	if got := len(journalBoundaryCanonical(t, group)); got != target {
		t.Fatalf("boundary fixture size %d != %d", got, target)
	}
	return changes
}

func journalBoundaryEffects(t *testing.T, owner *gorm.DB, account uuid.UUID, sequence int64, receipts, revisions, groups int64) {
	t.Helper()
	for table, want := range map[string]int64{"operation_receipts": receipts, "record_revisions": revisions, "sync_changes": groups} {
		if got := accessTestCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", account); int64(got) != want {
			t.Fatalf("unexpected %s effects: %d != %d", table, got, want)
		}
	}
	var got int64
	if err := owner.Raw("SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", account).Row().Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != sequence {
		t.Fatalf("stream sequence changed: %d != %d", got, sequence)
	}
}

func TestSyncCommandsPostgresCanonicalGroupLimitAndSequenceWidth(t *testing.T) {
	owner, actor, pull, cursors, binding := pullFixture(t)
	d := profileCommandDependencies(t)
	ctx := journalBoundaryContext(t, actor)
	// The omitted prefix is compacted fixture history; the tested groups are 9/10.
	if err := owner.Exec("UPDATE mancuerna.sync_streams SET last_sequence=8,compacted_through=8 WHERE account_id=?", actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	op9 := uuid.New()
	prefix := strings.Repeat("<&>\u2028é漢🙂", 20000)
	changes9 := journalBoundaryChanges(t, op9, 9, protocol.PullPageBytes, prefix, []int{})
	escaped, err := json.Marshal(changes9)
	if err != nil || len(escaped) <= protocol.PullPageBytes {
		t.Fatal("fixture must exceed the old escaped changes limit", len(escaped), err)
	}
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := d.Commands.CheckChanges(tx, actor.AccountID, op9, changes9); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal("exact canonical group rejected", err)
	}
	journalBoundaryEffects(t, owner, actor.AccountID, 8, 0, 0, 0)
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		return d.Commands.Commit(tx, journalBoundaryReceipt(actor, op9), changes9)
	})
	if err != nil {
		t.Fatal("exact canonical group could not commit", err)
	}
	page, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: issuePull(t, cursors, binding, "8", time.Now())})
	if err != nil || len(page.Groups) != 1 || page.Groups[0].Sequence != "9" || len(journalBoundaryCanonical(t, page.Groups[0])) != protocol.PullPageBytes {
		t.Fatal("committed boundary group cannot be downloaded intact", err)
	}
	if string(journalBoundaryCanonical(t, page.Groups[0].Changes)) != string(journalBoundaryCanonical(t, changes9)) {
		t.Fatal("canonical storage changed Unicode/text/record values")
	}
	op10 := uuid.New()
	tooBig := journalBoundaryChanges(t, op10, 9, protocol.PullPageBytes, "", []int{})
	if got := len(journalBoundaryCanonical(t, protocol.ChangeGroup{Sequence: "10", OperationID: op10.String(), Changes: tooBig})); got != protocol.PullPageBytes+1 {
		t.Fatal("sequence width did not produce exact +1 fixture", got)
	}
	for _, commit := range []bool{false, true} {
		err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
			if commit {
				return d.Commands.Commit(tx, journalBoundaryReceipt(actor, op10), tooBig)
			}
			return d.Commands.CheckChanges(tx, actor.AccountID, op10, tooBig)
		})
		if !errors.Is(err, protocol.ErrCommandLimit) {
			t.Fatal("full group +1 was not rejected before writing", commit, err)
		}
		journalBoundaryEffects(t, owner, actor.AccountID, 9, 1, 1, 1)
	}
	exact10 := journalBoundaryChanges(t, op10, 10, protocol.PullPageBytes, "", []int{})
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		return d.Commands.Commit(tx, journalBoundaryReceipt(actor, op10), exact10)
	})
	if err != nil {
		t.Fatal("exact boundary at sequence 10 rejected", err)
	}
	second, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: page.NextCursor})
	if err != nil || len(second.Groups) != 1 || second.Groups[0].Sequence != "10" || second.HasMore {
		t.Fatal("sequence 10 could not be downloaded", err)
	}
	journalBoundaryEffects(t, owner, actor.AccountID, 10, 2, 2, 2)
}

func TestSyncCommandsPostgresJSONBPreflightAllowsDurableRejection(t *testing.T) {
	owner, actor, _ := meTestFixture(t, finalizationDependencies(t))
	d := profileCommandDependencies(t)
	ctx := journalBoundaryContext(t, actor)
	op := uuid.New()
	changes := journalBoundaryChanges(t, op, 1, protocol.PullPageBytes-1000, "", make([]int, 10000))
	encoded := journalBoundaryCanonical(t, changes)
	var size int
	if err := owner.Raw("SELECT octet_length((?::jsonb)::text)", string(encoded)).Row().Scan(&size); err != nil || size <= protocol.PullPageBytes {
		t.Fatal("fixture must exceed only conservative JSONB bound", size, err)
	}
	err := d.UoW.Command(ctx, func(tx shared.Transaction) error {
		return d.Commands.Commit(tx, journalBoundaryReceipt(actor, op), changes)
	})
	if !errors.Is(err, protocol.ErrCommandLimit) {
		t.Fatal("JSONB limit did not return a controlled error", err)
	}
	journalBoundaryEffects(t, owner, actor.AccountID, 0, 0, 0, 0)
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := d.Commands.CheckChanges(tx, actor.AccountID, op, changes); !errors.Is(err, protocol.ErrCommandLimit) {
			return errors.New("preflight did not identify the JSONB limit")
		}
		// The check must leave the transaction usable for a terminal receipt.
		receipt := journalBoundaryReceipt(actor, op)
		receipt.Result = protocol.Failure(op, "rejected", "LIMIT_EXCEEDED", "Reduce el contenido.", "correct", false)
		return d.Commands.Commit(tx, receipt, nil)
	})
	if err != nil {
		t.Fatal("limit preflight aborted the transaction before its rejection receipt", err)
	}
	journalBoundaryEffects(t, owner, actor.AccountID, 0, 1, 0, 0)
}

func TestSyncCommandsPostgresInvalidGroupsAndSequenceExhaustionLeaveNoEffects(t *testing.T) {
	owner, actor, _ := meTestFixture(t, finalizationDependencies(t))
	d := profileCommandDependencies(t)
	ctx := journalBoundaryContext(t, actor)
	change := protocol.Change{EntityType: "personal_exercise", EntityID: uuid.NewString(), Revision: "1", Data: json.RawMessage(`{"text":"valid"}`)}
	mutations := []func([]protocol.Change) []protocol.Change{
		func(v []protocol.Change) []protocol.Change { v[0].Data = json.RawMessage(`{"x":1,"x":2}`); return v },
		func(v []protocol.Change) []protocol.Change { v[0].Data = json.RawMessage(`{"x":1.25}`); return v },
		func(v []protocol.Change) []protocol.Change { v[0].Data = json.RawMessage(`{"x":`); return v },
		func(v []protocol.Change) []protocol.Change { v[0].EntityType = "unknown"; return v },
		func(v []protocol.Change) []protocol.Change { v[0].Revision = "01"; return v },
		func(v []protocol.Change) []protocol.Change { v[0].EntityID = uuid.Nil.String(); return v },
		func(v []protocol.Change) []protocol.Change { next := v[0]; next.Revision = "2"; return append(v, next) },
	}
	for i, mutate := range mutations {
		op := uuid.New()
		changes := mutate([]protocol.Change{change})
		err := d.UoW.Command(ctx, func(tx shared.Transaction) error {
			return d.Commands.Commit(tx, journalBoundaryReceipt(actor, op), changes)
		})
		if !errors.Is(err, protocol.ErrCommandInvalid) {
			t.Fatalf("invalid group %d accepted or unclassified: %v", i, err)
		}
		journalBoundaryEffects(t, owner, actor.AccountID, 0, 0, 0, 0)
	}
	if err := owner.Exec("UPDATE mancuerna.sync_streams SET last_sequence=?,compacted_through=? WHERE account_id=?", int64(math.MaxInt64), int64(math.MaxInt64), actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	for _, commit := range []bool{false, true} {
		op := uuid.New()
		err := d.UoW.Command(ctx, func(tx shared.Transaction) error {
			if commit {
				return d.Commands.Commit(tx, journalBoundaryReceipt(actor, op), []protocol.Change{change})
			}
			return d.Commands.CheckChanges(tx, actor.AccountID, op, []protocol.Change{change})
		})
		if !errors.Is(err, protocol.ErrCommandPersistence) {
			t.Fatal("exhausted sequence was not rejected", err)
		}
		journalBoundaryEffects(t, owner, actor.AccountID, math.MaxInt64, 0, 0, 0)
	}
}

func TestSyncCommandsPostgresCommitRechecksSequenceAndRollsBackEarlierEffects(t *testing.T) {
	owner, actor, _ := meTestFixture(t, finalizationDependencies(t))
	d := profileCommandDependencies(t)
	ctx := journalBoundaryContext(t, actor)
	if err := owner.Exec("UPDATE mancuerna.sync_streams SET last_sequence=8,compacted_through=8 WHERE account_id=?", actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	op := uuid.New()
	changes := journalBoundaryChanges(t, op, 9, protocol.PullPageBytes, "", []int{})
	err := d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := d.Commands.CheckChanges(tx, actor.AccountID, op, changes); err != nil {
			return err
		}
		other := uuid.New()
		small := []protocol.Change{{EntityType: "personal_exercise", EntityID: uuid.NewString(), Revision: "1", Data: json.RawMessage(`{"text":"first"}`)}}
		if err := d.Commands.Commit(tx, journalBoundaryReceipt(actor, other), small); err != nil {
			return err
		}
		return d.Commands.Commit(tx, journalBoundaryReceipt(actor, op), changes)
	})
	if !errors.Is(err, protocol.ErrCommandLimit) {
		t.Fatal("Commit trusted a preflight for an obsolete next sequence", err)
	}
	journalBoundaryEffects(t, owner, actor.AccountID, 8, 0, 0, 0)
}
