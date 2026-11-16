//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	commandRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	"github.com/google/uuid"
)

func profileCommandDependencies(t *testing.T) personalApp.UpdateProfileDependencies {
	t.Helper()
	uow, err := shared.NewPersonalPostgresUnitOfWork(budgetTestPool(t, "MANCUERNA_TEST_DATA_DSN"), shared.PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	return personalApp.UpdateProfileDependencies{UoW: uow, Accounts: NewAccountPostgresRepository(), Commands: commandRepos.NewCommands(), Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now}
}
func profileCommandUseCase(t *testing.T, d personalApp.UpdateProfileDependencies) *personalApp.UpdateProfile {
	t.Helper()
	u, err := personalApp.NewUpdateProfile(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func profileCommand(actor security.AccessClaims, base, name string) account.ProfileCommand {
	c := account.ProfileCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), EntityID: actor.AccountID.String(), Type: "profile.update", Base: protocol.Base{Revision: &base}, CapturedAt: protocol.Timestamp(time.Now())}
	c.Payload.DisplayName = name
	c.Payload.WeightUnit = account.Kilograms
	return c
}
func TestProfileCommandPostgresConcurrentReplayAndAtomicJournal(t *testing.T) {
	auth := finalizationDependencies(t)
	owner, actor, _ := meTestFixture(t, auth)
	d := profileCommandDependencies(t)
	u := profileCommandUseCase(t, d)
	c := profileCommand(actor, "1", "María <&> 漢")
	const n = 8
	results := make([]protocol.OperationResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; results[i], errs[i] = u.Execute(context.Background(), actor, c) }(i)
	}
	close(start)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Outcome != "accepted" || !reflect.DeepEqual(results[i], results[0]) {
			t.Fatalf("same operation diverged: %v", errs[i])
		}
	}
	if results[0].Affected[0].Revision != "2" {
		t.Fatal("revision was not advanced once")
	}
	for _, table := range []string{"operation_receipts", "record_revisions", "sync_changes"} {
		if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", actor.AccountID) != 1 {
			t.Fatal("duplicated protocol effects", table)
		}
	}
	if accessTestCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", actor.AccountID) != 1 {
		t.Fatal("sequence duplicated")
	}
	current := profileRead(t, d.UoW, actor.AccountID)
	if current.DisplayName != c.Payload.DisplayName || current.Revision != 2 || current.WeightUnit != account.Kilograms {
		t.Fatal("profile content changed")
	}
	changed := c
	changed.Payload.DisplayName = "Modified retry"
	if _, err := u.Execute(context.Background(), actor, changed); !errors.Is(err, protocol.ErrOperationMismatch) {
		t.Fatal("changed retry accepted", err)
	}
	next := profileCommand(actor, "0", "Segundo cambio")
	next.Base = protocol.Base{AfterOperationID: &c.OperationID}
	next.Payload.WeightUnit = account.Pounds
	result, err := u.Execute(context.Background(), actor, next)
	if err != nil || result.Outcome != "accepted" || result.Affected[0].Revision != "3" {
		t.Fatal("dependency did not resolve exact accepted revision", err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_e164 IS NOT NULL AND security_version=1 AND status='active'", actor.AccountID) != 1 {
		t.Fatal("profile modified protected columns")
	}
}
func TestProfileCommandPostgresConflictPreservesProposalAndDependencies(t *testing.T) {
	auth := finalizationDependencies(t)
	owner, actor, _ := meTestFixture(t, auth)
	d := profileCommandDependencies(t)
	u := profileCommandUseCase(t, d)
	first := profileCommand(actor, "1", "Primero")
	if _, err := u.Execute(context.Background(), actor, first); err != nil {
		t.Fatal(err)
	}
	stale := profileCommand(actor, "1", "Propuesta sin perder")
	result, err := u.Execute(context.Background(), actor, stale)
	if err != nil || result.Outcome != "conflict" || result.ConflictID == "" {
		t.Fatal("stale write not preserved", err)
	}
	if profileRead(t, d.UoW, actor.AccountID).DisplayName != "Primero" {
		t.Fatal("conflict overwrote current profile")
	}
	var proposal []byte
	if err = owner.Raw("SELECT proposal FROM mancuerna.sync_conflicts WHERE id=?", result.ConflictID).Row().Scan(&proposal); err != nil {
		t.Fatal(err)
	}
	var preserved account.ProfileCommand
	if json.Unmarshal(proposal, &preserved) != nil || !reflect.DeepEqual(preserved, stale) {
		t.Fatal("proposal lost original command")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sync_changes WHERE account_id=? AND changes->0->>'entity_type'='conflict'", actor.AccountID) != 1 {
		t.Fatal("conflict omitted from journal")
	}
	if retry, err := u.Execute(context.Background(), actor, stale); err != nil || !reflect.DeepEqual(retry, result) {
		t.Fatal("conflict duplicated on retry", err)
	}
	missingID := uuid.NewString()
	missing := profileCommand(actor, "0", "Espera")
	missing.Base = protocol.Base{AfterOperationID: &missingID}
	if pending, err := u.Execute(context.Background(), actor, missing); err != nil || pending.Outcome != "dependency_pending" {
		t.Fatal("missing dependency consumed", err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", missing.OperationID) != 0 {
		t.Fatal("pending dependency became terminal")
	}
	blocked := profileCommand(actor, "0", "Bloqueado")
	blocked.Base = protocol.Base{AfterOperationID: &stale.OperationID}
	if got, err := u.Execute(context.Background(), actor, blocked); err != nil || got.Outcome != "dependency_blocked" {
		t.Fatal("conflicted parent accepted", err)
	}
	// The accepted receipt still says revision2 even after later updates; never
	// replace it with the current revision when resolving a dependency.
	next := profileCommand(actor, "2", "Tercero")
	if _, err = u.Execute(context.Background(), actor, next); err != nil {
		t.Fatal(err)
	}
	oldParent := profileCommand(actor, "0", "Conservar")
	oldParent.Base = protocol.Base{AfterOperationID: &first.OperationID}
	if got, err := u.Execute(context.Background(), actor, oldParent); err != nil || got.Outcome != "conflict" {
		t.Fatal("dependency silently used current revision", err)
	}
}

type profileFailCommit struct{ protocolRepository.Commands }

func (p profileFailCommit) Commit(tx shared.Transaction, r protocol.Receipt, changes []protocol.Change) error {
	if err := p.Commands.Commit(tx, r, changes); err != nil {
		return err
	}
	return errors.New("synthetic failure after journal")
}
func TestProfileCommandPostgresRollbackAndCrossAccountIDCollision(t *testing.T) {
	auth := finalizationDependencies(t)
	owner, a, _ := meTestFixture(t, auth)
	_, b, _ := meTestFixture(t, auth)
	d := profileCommandDependencies(t)
	failing := d
	failing.Commands = profileFailCommit{d.Commands}
	c := profileCommand(a, "1", "Rollback")
	before := profileRead(t, d.UoW, a.AccountID)
	if _, err := profileCommandUseCase(t, failing).Execute(context.Background(), a, c); err == nil {
		t.Fatal("injected commit failure ignored")
	}
	if !reflect.DeepEqual(before, profileRead(t, d.UoW, a.AccountID)) {
		t.Fatal("profile escaped rollback")
	}
	for _, table := range []string{"operation_receipts", "record_revisions", "sync_changes"} {
		if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", a.AccountID) != 0 {
			t.Fatal("protocol effect escaped rollback", table)
		}
	}
	if accessTestCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 0 {
		t.Fatal("sequence escaped rollback")
	}
	u := profileCommandUseCase(t, d)
	if _, err := u.Execute(context.Background(), a, c); err != nil {
		t.Fatal(err)
	}
	foreign := profileCommand(b, "1", "No copiar A")
	foreign.OperationID = c.OperationID
	if _, err := u.Execute(context.Background(), b, foreign); !errors.Is(err, protocol.ErrOperationMismatch) {
		t.Fatal("global operation collision leaked or accepted", err)
	}
	if profileRead(t, d.UoW, b.AccountID).Revision != 1 {
		t.Fatal("foreign operation collision committed B change")
	}
}
func TestProfileCommandPostgresActorOriginAndReceiptIsolation(t *testing.T) {
	auth := finalizationDependencies(t)
	owner, a, _ := meTestFixture(t, auth)
	_, b, _ := meTestFixture(t, auth)
	d := profileCommandDependencies(t)
	u := profileCommandUseCase(t, d)
	c := profileCommand(a, "1", "Autorizada")
	foreign := c
	foreign.OriginDeviceID = b.DeviceID.String()
	if _, err := u.Execute(context.Background(), a, foreign); !errors.Is(err, protocol.ErrCommandOrigin) {
		t.Fatal("foreign origin accepted", err)
	}
	if _, err := u.Execute(shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), a, c); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("attempt capability mixed with data", err)
	}
	if _, err := u.Execute(context.Background(), a, c); err != nil {
		t.Fatal(err)
	}
	if err := auth.UoW.Command(shared.WithPersonalAccount(context.Background(), a.AccountID), func(tx shared.Transaction) error {
		return auth.Sessions.RevokeSession(tx, a.AccountID, a.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), a, c); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("revoked actor retrieved receipt", err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE account_id=?", a.AccountID) != 1 {
		t.Fatal("revoked retry changed receipt")
	}
	if err := auth.UoW.Command(shared.WithPersonalAccount(context.Background(), b.AccountID), func(tx shared.Transaction) error {
		return d.Commands.RequireActor(tx, protocol.Actor{AccountID: b.AccountID, SessionID: b.SessionID, DeviceID: b.DeviceID}, b.DeviceID)
	}); !errors.Is(err, protocol.ErrCommandScope) {
		t.Fatal("auth role accepted for data protocol", err)
	}
}
