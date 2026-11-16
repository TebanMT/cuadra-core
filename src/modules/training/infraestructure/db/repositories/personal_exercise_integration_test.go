//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	authRepos "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/repositories"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	commandRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func trainingTestPool(t *testing.T, key string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(key)
	if dsn == "" {
		t.Skip(key + " required for isolated PostgreSQL training tests")
	}
	c, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid isolated database configuration")
	}
	local := func(host string) bool {
		if filepath.IsAbs(host) || host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	if !local(c.Host) {
		t.Fatal("training tests require local PostgreSQL")
	}
	for _, fallback := range c.Fallbacks {
		if !local(fallback.Host) {
			t.Fatal("training tests require local PostgreSQL fallbacks")
		}
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("isolated PostgreSQL unavailable")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal("isolated PostgreSQL pool unavailable")
	}
	pool.SetMaxOpenConns(8)
	pool.SetMaxIdleConns(8)
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

func trainingFixture(t *testing.T) (*gorm.DB, security.AccessClaims, trainingApp.CreatePersonalExerciseDependencies) {
	t.Helper()
	owner := trainingTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	a := security.AccessClaims{AccountID: uuid.New(), DeviceID: uuid.New(), SessionID: uuid.New(), IssuedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	phone := fmt.Sprintf("+1999%011d", binary.BigEndian.Uint64(a.AccountID[:8])%100000000000)
	refresh, ticket := sha256.Sum256([]byte(uuid.NewString())), sha256.Sum256([]byte(uuid.NewString()))
	if err := owner.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("INSERT INTO mancuerna.accounts(id,phone_e164,phone_verified_at) VALUES(?,?,clock_timestamp())", a.AccountID, phone).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO mancuerna.sync_streams(account_id,epoch) VALUES(?,?)", a.AccountID, uuid.New()).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO mancuerna.devices(id,account_id,platform,label,public_key,authorized_at,authorized_by_method) VALUES(?,?,'ios','Synthetic training',?,clock_timestamp()-interval '1 minute','signup')", a.DeviceID, a.AccountID, refresh[:]).Error; err != nil {
			return err
		}
		return tx.Exec("INSERT INTO mancuerna.sessions(id,account_id,device_id,refresh_hash,refresh_family_id,expires_at,revocation_ticket_hash) VALUES(?,?,?,?,?,clock_timestamp()+interval '1 day',?)", a.SessionID, a.AccountID, a.DeviceID, refresh[:], uuid.New(), ticket[:]).Error
	}); err != nil {
		t.Fatal("synthetic training account setup failed", err)
	}
	uow, err := shared.NewPersonalPostgresUnitOfWork(trainingTestPool(t, "MANCUERNA_TEST_DATA_DSN"), shared.PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	return owner, a, trainingApp.CreatePersonalExerciseDependencies{UoW: uow, Exercises: NewPersonalExercisePostgresRepository(), Commands: commandRepos.NewCommands(), Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now}
}
func trainingUseCase(t *testing.T, d trainingApp.CreatePersonalExerciseDependencies) *trainingApp.CreatePersonalExercise {
	t.Helper()
	u, err := trainingApp.NewCreatePersonalExercise(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func trainingCommand(actor security.AccessClaims) exercise.CreateCommand {
	zero := "0"
	return exercise.CreateCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), Type: exercise.CreateCommandType, EntityID: uuid.NewString(), Base: protocol.Base{Revision: &zero}, CapturedAt: "2026-10-19T10:00:00Z", Payload: exercise.CreatePayload{Name: "  Press Élite 漢  ", LoadMode: exercise.External, Notes: "Nota\nsegunda"}}
}
func trainingCount(t *testing.T, owner *gorm.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := owner.Raw(query, args...).Row().Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func trainingRead(t *testing.T, d trainingApp.CreatePersonalExerciseDependencies, ownerID, id uuid.UUID) (*exercise.Exercise, error) {
	t.Helper()
	tx, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), ownerID))
	if err != nil {
		return nil, err
	}
	defer shared.CloseQuery(tx)
	return d.Exercises.GetByID(tx, ownerID, id)
}
func trainingAssertEmpty(t *testing.T, owner *gorm.DB, id uuid.UUID) {
	t.Helper()
	for _, table := range []string{"personal_exercises", "operation_receipts", "record_revisions", "sync_changes"} {
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", id) != 0 {
			t.Fatal("effect escaped rollback", table)
		}
	}
	if trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", id) != 0 {
		t.Fatal("sequence escaped rollback")
	}
}

func TestPersonalExercisePostgresConcurrentReplayAndCanonicalJournal(t *testing.T) {
	owner, actor, d := trainingFixture(t)
	u, c := trainingUseCase(t, d), trainingCommand(actor)
	const count = 8
	results, errs := make([]protocol.OperationResult, count), make([]error, count)
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
			t.Fatal("concurrent retry diverged", errs[i])
		}
	}
	if len(results[0].Affected) != 1 || results[0].Affected[0].EntityID != c.EntityID || results[0].Affected[0].Revision != "1" {
		t.Fatal("wrong affected revision")
	}
	for _, table := range []string{"personal_exercises", "operation_receipts", "record_revisions", "sync_changes"} {
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", actor.AccountID) != 1 {
			t.Fatal("duplicate effect", table)
		}
	}
	if trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", actor.AccountID) != 1 {
		t.Fatal("duplicate sequence")
	}
	e, err := trainingRead(t, d, actor.AccountID, uuid.MustParse(c.EntityID))
	if err != nil || e.Name != c.Payload.Name || e.Notes != c.Payload.Notes || e.LoadMode != c.Payload.LoadMode || e.Revision != 1 || e.ArchivedAt != nil || e.DeletedAt != nil {
		t.Fatal("stored exercise changed", err)
	}
	var projection, group, storedHash []byte
	if err = owner.Raw("SELECT canonical FROM mancuerna.record_revisions WHERE account_id=? AND entity_type='personal_exercise' AND entity_id=? AND revision=1", actor.AccountID, c.EntityID).Row().Scan(&projection); err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if json.Unmarshal(projection, &view) != nil || len(view) != 10 || view["source"] != "personal" || view["name"] != c.Payload.Name || view["notes"] != c.Payload.Notes || view["id"] != c.EntityID || view["revision"] != "1" || view["deleted_at"] != nil || view["archived_at"] != nil || view["created_at"] != results[0].CommittedAt || view["updated_at"] != results[0].CommittedAt {
		t.Fatal("canonical Exercise projection changed")
	}
	if err = owner.Raw("SELECT changes FROM mancuerna.sync_changes WHERE account_id=?", actor.AccountID).Row().Scan(&group); err != nil {
		t.Fatal(err)
	}
	var changes []protocol.Change
	if json.Unmarshal(group, &changes) != nil || len(changes) != 1 || changes[0].EntityType != "personal_exercise" || changes[0].EntityID != c.EntityID {
		t.Fatal("wrong complete change group")
	}
	canonicalProjection, _ := d.Canonicalizer.Canonicalize(projection)
	canonicalGroupProjection, _ := d.Canonicalizer.Canonicalize(changes[0].Data)
	if string(canonicalProjection) != string(canonicalGroupProjection) {
		t.Fatal("journal diverged from revision")
	}
	raw, _ := json.Marshal(c)
	canonical, _ := d.Canonicalizer.Canonicalize(raw)
	expectedHash := sha256.Sum256(canonical)
	if err = owner.Raw("SELECT request_hash FROM mancuerna.operation_receipts WHERE operation_id=?", c.OperationID).Row().Scan(&storedHash); err != nil || !reflect.DeepEqual(storedHash, expectedHash[:]) {
		t.Fatal("command hash lost original strings", err)
	}
	var search string
	if err = owner.Raw("SELECT search_key FROM mancuerna.personal_exercises WHERE id=?", c.EntityID).Row().Scan(&search); err != nil || search != "press elite 漢" {
		t.Fatal("derived search key changed", err)
	}
}

func TestPersonalExercisePostgresIdempotencyAndCrossAccountCollisions(t *testing.T) {
	owner, a, d := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	u, c := trainingUseCase(t, d), trainingCommand(a)
	if _, err := u.Execute(context.Background(), a, c); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*exercise.CreateCommand){func(c *exercise.CreateCommand) { c.Payload.Name = "Changed" }, func(c *exercise.CreateCommand) { c.CapturedAt = "2026-10-19T10:00:00.000Z" }, func(c *exercise.CreateCommand) { c.EntityID = uuid.NewString() }} {
		changed := c
		edit(&changed)
		if _, err := u.Execute(context.Background(), a, changed); !errors.Is(err, protocol.ErrOperationMismatch) {
			t.Fatal("changed retry accepted", err)
		}
	}
	for _, actor := range []security.AccessClaims{a, b} {
		collision := trainingCommand(actor)
		collision.EntityID = c.EntityID
		result, err := u.Execute(context.Background(), actor, collision)
		if err != nil || result.Outcome != "rejected" || result.Error.Code != "REVISION_CONFLICT" || strings.Contains(result.Error.Message, a.AccountID.String()) {
			t.Fatal("entity collision leaked or overwrote", err)
		}
		if retry, err := u.Execute(context.Background(), actor, collision); err != nil || !reflect.DeepEqual(result, retry) {
			t.Fatal("collision receipt was not durable", err)
		}
	}
	foreign := trainingCommand(b)
	foreign.OperationID = c.OperationID
	if _, err := u.Execute(context.Background(), b, foreign); !errors.Is(err, protocol.ErrOperationMismatch) {
		t.Fatal("global operation collision accepted", err)
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.personal_exercises WHERE account_id=?", b.AccountID) != 0 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", b.AccountID) != 0 {
		t.Fatal("foreign operation collision committed data")
	}
	if _, err := trainingRead(t, d, b.AccountID, uuid.MustParse(c.EntityID)); !errors.Is(err, trainingErrors.ErrExerciseNotFound) {
		t.Fatal("B read A exercise", err)
	}
	e, err := trainingRead(t, d, a.AccountID, uuid.MustParse(c.EntityID))
	if err != nil || e.Name != c.Payload.Name || e.Revision != 1 {
		t.Fatal("existing exercise overwritten", err)
	}
	// Same name with a new identity is a legitimate independent exercise.
	next := trainingCommand(a)
	if got, err := u.Execute(context.Background(), a, next); err != nil || got.Outcome != "accepted" {
		t.Fatal("name used as deduplication identity", err)
	}
}

type trainingFailCommit struct{ protocolRepository.Commands }

func (r trainingFailCommit) Commit(tx shared.Transaction, receipt protocol.Receipt, changes []protocol.Change) error {
	if err := r.Commands.Commit(tx, receipt, changes); err != nil {
		return err
	}
	return errors.New("synthetic failure after complete journal")
}

func TestPersonalExercisePostgresRollbackAndDurableValidation(t *testing.T) {
	owner, a, d := trainingFixture(t)
	failing := d
	failing.Commands = trainingFailCommit{d.Commands}
	c := trainingCommand(a)
	if _, err := trainingUseCase(t, failing).Execute(context.Background(), a, c); err == nil {
		t.Fatal("post-journal failure ignored")
	}
	trainingAssertEmpty(t, owner, a.AccountID)
	u := trainingUseCase(t, d)
	if got, err := u.Execute(context.Background(), a, c); err != nil || got.Outcome != "accepted" {
		t.Fatal("rollback consumed identity", err)
	}
	for _, base := range []protocol.Base{{Revision: ptr("1")}, {AfterOperationID: ptr(uuid.NewString())}} {
		invalid := trainingCommand(a)
		invalid.Base = base
		got, err := u.Execute(context.Background(), a, invalid)
		if err != nil || got.Outcome != "rejected" || got.Error.Code != "VALIDATION_ERROR" {
			t.Fatal("nonzero creation/dependency accepted", err)
		}
		if retry, err := u.Execute(context.Background(), a, invalid); err != nil || !reflect.DeepEqual(got, retry) {
			t.Fatal("validation receipt changed", err)
		}
	}
	invalid := trainingCommand(a)
	invalid.Payload.Name = " \n "
	if got, err := u.Execute(context.Background(), a, invalid); err != nil || got.Outcome != "rejected" {
		t.Fatal("blank business name accepted", err)
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.personal_exercises WHERE account_id=?", a.AccountID) != 1 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE account_id=?", a.AccountID) != 4 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 1 {
		t.Fatal("rejected operation emitted a data effect")
	}
}
func ptr(value string) *string { return &value }

func TestPersonalExercisePostgresActorOriginAndPureDataScope(t *testing.T) {
	owner, a, d := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	u, c := trainingUseCase(t, d), trainingCommand(a)
	foreign := c
	foreign.OriginDeviceID = b.DeviceID.String()
	if _, err := u.Execute(context.Background(), a, foreign); !errors.Is(err, protocol.ErrCommandOrigin) {
		t.Fatal("foreign origin accepted", err)
	}
	badActor := a
	badActor.AccountID = b.AccountID
	if _, err := u.Execute(context.Background(), badActor, c); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("mixed actor owner accepted", err)
	}
	for _, ctx := range []context.Context{shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithPersonalAccount(context.Background(), b.AccountID), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := u.Execute(ctx, a, c); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed scope accepted", err)
		}
	}
	trainingAssertEmpty(t, owner, a.AccountID)
	oldOrigin := uuid.New()
	if err := owner.Exec("INSERT INTO mancuerna.devices(id,account_id,platform,label,public_key,authorized_at,authorized_by_method,revoked_at,deleted_at) VALUES(?,?,'android','Old origin',?,clock_timestamp()-interval '1 day','device',clock_timestamp(),clock_timestamp())", oldOrigin, a.AccountID, make([]byte, 32)).Error; err != nil {
		t.Fatal(err)
	}
	c.OriginDeviceID = oldOrigin.String()
	if got, err := u.Execute(context.Background(), a, c); err != nil || got.Outcome != "accepted" {
		t.Fatal("historical own origin rejected", err)
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE account_id=? AND actor_device_id=? AND origin_device_id=?", a.AccountID, a.DeviceID, oldOrigin) != 1 {
		t.Fatal("actor/origin audit conflated")
	}
	auth, err := shared.NewPersonalPostgresUnitOfWork(trainingTestPool(t, "MANCUERNA_TEST_AUTH_DSN"), shared.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	if err = auth.Command(shared.WithPersonalAccount(context.Background(), a.AccountID), func(tx shared.Transaction) error {
		return authRepos.NewAuthSessionRepository().RevokeSession(tx, a.AccountID, a.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = u.Execute(context.Background(), a, c); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("revoked actor retrieved receipt", err)
	}
	if err = auth.Command(shared.WithPersonalAccount(context.Background(), b.AccountID), func(tx shared.Transaction) error {
		_, err := d.Exercises.GetByID(tx, b.AccountID, uuid.MustParse(c.EntityID))
		return err
	}); !errors.Is(err, protocol.ErrCommandScope) {
		t.Fatal("auth pool used training repository", err)
	}
	query, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), b.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(query)
	newExercise, _ := exercise.NewPersonal(uuid.New(), b.AccountID, "Readonly", exercise.None, "", time.Now())
	if _, err = d.Exercises.Create(query, b.AccountID, newExercise); !errors.Is(err, protocol.ErrCommandScope) {
		t.Fatal("readonly transaction wrote training", err)
	}
	if _, err = d.Exercises.GetByID(query, a.AccountID, uuid.MustParse(c.EntityID)); !errors.Is(err, protocol.ErrCommandScope) {
		t.Fatal("explicit foreign owner accepted", err)
	}
}

func TestPersonalExercisePostgresRepositoryNeverOverwritesAndRedactsErrors(t *testing.T) {
	owner, a, d := trainingFixture(t)
	ctx := shared.WithPersonalAccount(context.Background(), a.AccountID)
	e, _ := exercise.NewPersonal(uuid.New(), a.AccountID, "Never expose SQL content", exercise.None, "Private synthetic note", time.Now())
	if err := d.UoW.Command(ctx, func(tx shared.Transaction) error {
		created, err := d.Exercises.Create(tx, a.AccountID, e)
		if err != nil || !created {
			return errors.New("first create failed")
		}
		copy := *e
		copy.Name = "Replacement"
		created, err = d.Exercises.Create(tx, a.AccountID, &copy)
		if err != nil || created {
			return errors.New("second create overwrote")
		}
		copy.Revision = 2
		if _, err = d.Exercises.Create(tx, a.AccountID, &copy); !errors.Is(err, trainingErrors.ErrInvalidExercise) {
			return errors.New("noninitial creation accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read, err := trainingRead(t, d, a.AccountID, e.ID)
	if err != nil || read.Name != e.Name {
		t.Fatal("repository overwrote initial value", err)
	}
	// A transactional local constraint forces an infrastructure failure without
	// changing shared schema or exposing statement/row details through the port.
	if err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		g := tx.(*shared.GormTransaction)
		if err := g.Tx.Exec("SET LOCAL statement_timeout='1ms'").Error; err != nil {
			return err
		}
		if err := g.Tx.Exec("SELECT pg_sleep(0.02)").Error; err == nil {
			return errors.New("synthetic timeout did not abort transaction")
		}
		_, err := d.Exercises.GetByID(tx, a.AccountID, e.ID)
		if !errors.Is(err, trainingErrors.ErrTrainingPersistence) || err.Error() != trainingErrors.ErrTrainingPersistence.Error() {
			return errors.New("SQL diagnostic escaped repository")
		}
		return err
	}); !errors.Is(err, trainingErrors.ErrTrainingPersistence) {
		t.Fatal("persistence error not redacted", err)
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.personal_exercises WHERE account_id=?", a.AccountID) != 1 {
		t.Fatal("failed transaction changed data")
	}
}
