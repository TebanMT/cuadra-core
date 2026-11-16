//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	syncApp "github.com/cuadra/cuadra-core/src/shared/sync/personal/app"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	streamRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	cursorCrypto "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type snapshotStorageTestFixture struct {
	owner  *gorm.DB
	actor  security.AccessClaims
	data   personalApp.UpdateProfileDependencies
	create syncApp.CreateSnapshotDependencies
	page   syncApp.GetSnapshotPageDependencies
}

func snapshotStorageTestNew(t *testing.T) snapshotStorageTestFixture {
	t.Helper()
	owner, actor, _ := meTestFixture(t, finalizationDependencies(t))
	d := profileCommandDependencies(t)
	repo, err := streamRepos.NewSnapshots(crypto.JSONCanonicalizer{})
	if err != nil {
		t.Fatal(err)
	}
	cursors, err := cursorCrypto.NewCursorSigner("first", map[string][]byte{"first": bytes.Repeat([]byte{0x41}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	capabilities := protocol.Capabilities{ProtocolVersion: 1, MinimumProtocolVersion: 1, MinimumAppVersion: "0.0.0", MaxPushOperations: 50, MaxRequestBytes: 2097152, MaxGroupBytes: 1048576, Features: []string{"personal_sync"}}
	return snapshotStorageTestFixture{owner: owner, actor: actor, data: d,
		create: syncApp.CreateSnapshotDependencies{UoW: d.UoW.(shared.PersonalSnapshotUnitOfWork), Snapshots: repo, Cursors: cursors, Capabilities: capabilities, Now: time.Now},
		page:   syncApp.GetSnapshotPageDependencies{UoW: d.UoW, Snapshots: repo, Now: time.Now}}
}
func snapshotStorageTestCreate(t *testing.T, d syncApp.CreateSnapshotDependencies) *syncApp.CreateSnapshot {
	t.Helper()
	u, err := syncApp.NewCreateSnapshot(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func snapshotStorageTestPage(t *testing.T, d syncApp.GetSnapshotPageDependencies) *syncApp.GetSnapshotPage {
	t.Helper()
	u, err := syncApp.NewGetSnapshotPage(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func snapshotStorageTestHash(t *testing.T, page protocol.SnapshotPage) string {
	t.Helper()
	raw, err := json.Marshal(page.Records)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := (crypto.JSONCanonicalizer{}).Canonicalize(raw)
	if err != nil || len(canonical) > protocol.PullPageBytes || len(page.Records) > protocol.SnapshotPageRecords {
		t.Fatal("page exceeded wire bounds", err)
	}
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:])
}

func TestSnapshotPostgresInitialProfileReplayIsolationAndExpiry(t *testing.T) {
	f := snapshotStorageTestNew(t)
	id := uuid.New()
	u := snapshotStorageTestCreate(t, f.create)
	s, err := u.Execute(context.Background(), pullActor(f.actor), id)
	if err != nil || s.ThroughSequence != "0" || s.PageCount != 1 || len(s.PageHashes) != 1 {
		t.Fatal("initial profile snapshot", err)
	}
	pageUC := snapshotStorageTestPage(t, f.page)
	p, err := pageUC.Execute(context.Background(), pullActor(f.actor), id, 0)
	if err != nil || len(p.Records) != 1 || p.Records[0].EntityType != "profile" || p.Records[0].EntityID != f.actor.AccountID.String() || p.Records[0].Revision != "1" || p.Hash != snapshotStorageTestHash(t, p) || p.Hash != s.PageHashes[0] {
		t.Fatal("initial profile omitted or hash covered wrong shape", err)
	}
	var data map[string]any
	if json.Unmarshal(p.Records[0].Data, &data) != nil || len(data) != 4 || data["id"] != f.actor.AccountID.String() {
		t.Fatal("private account data in snapshot")
	}
	if _, err = pageUC.Execute(context.Background(), pullActor(f.actor), id, 1); !errors.Is(err, protocol.ErrSnapshotMissing) {
		t.Fatal("out-of-range page", err)
	}
	rotated := f.create
	rotated.Cursors, err = cursorCrypto.NewCursorSigner("next", map[string][]byte{"first": bytes.Repeat([]byte{0x41}, 32), "next": bytes.Repeat([]byte{0x42}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	rotated.Capabilities.MinimumAppVersion = "1.1.0"
	rotated.Now = func() time.Time { return time.Now().Add(time.Hour) }
	replay, err := snapshotStorageTestCreate(t, rotated).Execute(context.Background(), pullActor(f.actor), id)
	if err != nil || !reflect.DeepEqual(replay, s) {
		t.Fatal("replay changed manifest after clock/config/key rotation", err)
	}
	position, err := rotated.Cursors.Verify(s.Cursor, protocol.PullBinding(f.actor.AccountID, uuid.MustParse(s.Epoch)), time.Now())
	if err != nil || position.Sequence != "0" {
		t.Fatal("snapshot cursor not exact H", err)
	}
	_, other, _ := meTestFixture(t, finalizationDependencies(t))
	if got, err := pageUC.Execute(context.Background(), pullActor(other), id, 0); !errors.Is(err, protocol.ErrSnapshotMissing) || got.Records != nil {
		t.Fatal("A/B page leak", err)
	}
	if _, err = u.Execute(context.Background(), pullActor(other), id); !errors.Is(err, protocol.ErrSnapshotConflict) {
		t.Fatal("global snapshot ID was overwritten", err)
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sync_snapshot_pages WHERE account_id=?", other.AccountID) != 0 {
		t.Fatal("foreign collision left partial pages")
	}
	expires, err := time.Parse(time.RFC3339Nano, s.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	var created time.Time
	if err = f.owner.Raw("SELECT created_at FROM mancuerna.sync_snapshots WHERE account_id=? AND id=?", f.actor.AccountID, id).Row().Scan(&created); err != nil || expires.Sub(created) != 24*time.Hour {
		t.Fatal("TTL not 24h from original creation", err)
	}
	expired := f.create
	expired.Now = func() time.Time { return expires }
	if got, err := snapshotStorageTestCreate(t, expired).Execute(context.Background(), pullActor(f.actor), id); !errors.Is(err, protocol.ErrSnapshotExpired) || got.ID != "" {
		t.Fatal("expired ID recreated", err)
	}
	expiredPage := f.page
	expiredPage.Now = expired.Now
	if got, err := snapshotStorageTestPage(t, expiredPage).Execute(context.Background(), pullActor(f.actor), id, 0); !errors.Is(err, protocol.ErrSnapshotExpired) || got.Records != nil {
		t.Fatal("expired page available", err)
	}
	for _, ctx := range []context.Context{shared.WithTenant(context.Background(), uuid.New()), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1})} {
		if _, err = u.Execute(ctx, pullActor(f.actor), uuid.New()); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed snapshot scope", err)
		}
	}
	if err = f.owner.Exec("UPDATE mancuerna.sync_streams SET epoch=? WHERE account_id=?", uuid.New(), f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = pageUC.Execute(context.Background(), pullActor(f.actor), id, 0); !errors.Is(err, protocol.ErrCursorEpochChanged) {
		t.Fatal("old epoch page available", err)
	}
	if err = f.owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE account_id=? AND id=?", f.actor.AccountID, f.actor.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = pageUC.Execute(context.Background(), pullActor(f.actor), id, 0); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("revoked actor saw epoch/page", err)
	}
	if _, err = u.Execute(context.Background(), pullActor(f.actor), uuid.New()); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("revoked actor materialized account", err)
	}
}

type snapshotStorageTestGate struct {
	protocolRepository.Snapshots
	ready   chan struct{}
	release chan struct{}
	failure error
}

func (g snapshotStorageTestGate) Materialize(tx shared.Transaction, accountID, id uuid.UUID) ([]string, error) {
	hashes, err := g.Snapshots.Materialize(tx, accountID, id)
	if err != nil {
		return nil, err
	}
	if g.ready != nil {
		close(g.ready)
		<-g.release
	}
	if g.failure != nil {
		return nil, g.failure
	}
	return hashes, nil
}
func TestSnapshotPostgresConsistentCutAndAtomicRollback(t *testing.T) {
	f := snapshotStorageTestNew(t)
	gate := snapshotStorageTestGate{Snapshots: f.create.Snapshots, ready: make(chan struct{}), release: make(chan struct{})}
	d := f.create
	d.Snapshots = gate
	create := snapshotStorageTestCreate(t, d)
	release := func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}
	defer release()
	id := uuid.New()
	type result struct {
		s   protocol.Snapshot
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := create.Execute(context.Background(), pullActor(f.actor), id)
		done <- result{s, err}
	}()
	select {
	case <-gate.ready:
	case early := <-done:
		t.Fatalf("snapshot failed before materializing: %v", early.err)
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not materialize")
	}
	writer := make(chan error, 1)
	update := profileCommandUseCase(t, f.data)
	command := profileCommand(f.actor, "1", "Después del corte")
	go func() { _, err := update.Execute(context.Background(), f.actor, command); writer <- err }()
	deadline := time.Now().Add(2 * time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		select {
		case err := <-writer:
			t.Fatalf("writer escaped snapshot mutex: %v", err)
		default:
		}
		if err := f.owner.Raw("SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'UPDATE mancuerna.sync_streams SET last_sequence=last_sequence%')").Scan(&waiting).Error; err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("writer never reached the account mutex")
	}
	release()
	created := <-done
	if created.err != nil || created.s.ThroughSequence != "0" {
		t.Fatal("snapshot H changed", created.err)
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	p, err := snapshotStorageTestPage(t, f.page).Execute(context.Background(), pullActor(f.actor), id, 0)
	if err != nil || len(p.Records) != 1 || p.Records[0].Revision != "1" || strings.Contains(string(p.Records[0].Data), "Después del corte") {
		t.Fatal("snapshot mixed later profile with old H", err)
	}
	pull, err := syncApp.NewPull(syncApp.PullDependencies{UoW: f.data.UoW, Commands: f.data.Commands, Stream: streamRepos.NewStream(), Cursors: f.create.Cursors, Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := pull.Execute(context.Background(), pullActor(f.actor), syncApp.PullInput{Cursor: created.s.Cursor})
	if err != nil || len(delta.Groups) != 1 || delta.Groups[0].Sequence != "1" || delta.Groups[0].Changes[0].Revision != "2" {
		t.Fatal("pull did not resume strictly after H", err)
	}
	failure := errors.New("synthetic failure after pages before manifest")
	d = f.create
	d.Snapshots = snapshotStorageTestGate{Snapshots: d.Snapshots, failure: failure}
	failedID := uuid.New()
	if got, err := snapshotStorageTestCreate(t, d).Execute(context.Background(), pullActor(f.actor), failedID); !errors.Is(err, failure) || got.ID != "" {
		t.Fatal("failed snapshot exposed manifest", err)
	}
	for _, table := range []string{"sync_snapshots", "sync_snapshot_pages"} {
		column := "snapshot_id"
		if table == "sync_snapshots" {
			column = "id"
		}
		if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=? AND "+column+"=?", f.actor.AccountID, failedID) != 0 {
			t.Fatal("partial snapshot committed", table)
		}
	}
	if accessTestCount(t, f.owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", f.actor.AccountID) != 1 {
		t.Fatal("snapshot changed journal sequence")
	}
}

// Infrastructure fixture only: complete exercise rows and their canonical
// projections commit together under the real data role. This does not claim
// that the future exercise command's validation/business rules are complete.
func snapshotStorageTestExerciseBatch(t *testing.T, f snapshotStorageTestFixture, ids []uuid.UUID, notes string, revision int, deleted bool) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	err := f.data.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		if err := f.data.Commands.RequireActor(tx, pullActor(f.actor), f.actor.DeviceID); err != nil {
			return err
		}
		db := tx.(*shared.GormTransaction).Tx
		changes := make([]protocol.Change, 0, len(ids))
		for _, id := range ids {
			var deletedAt any
			var deletedStamp any
			if deleted {
				deletedAt = now
				deletedStamp = protocol.Timestamp(now)
			}
			var created time.Time
			if revision == 1 {
				created = now
				if err := db.Exec("INSERT INTO mancuerna.personal_exercises(id,account_id,name,search_key,load_mode,notes,revision,created_at,updated_at,deleted_at) VALUES(?,?,'Ejercicio','ejercicio','external',?,?,?,?,?)", id, f.actor.AccountID, notes, revision, created, now, deletedAt).Error; err != nil {
					return err
				}
			} else {
				if err := db.Raw("UPDATE mancuerna.personal_exercises SET notes=?,revision=?,updated_at=?,deleted_at=? WHERE account_id=? AND id=? RETURNING created_at", notes, revision, now, deletedAt, f.actor.AccountID, id).Row().Scan(&created); err != nil {
					return err
				}
			}
			data, err := json.Marshal(map[string]any{"id": id.String(), "revision": strconv.Itoa(revision), "created_at": protocol.Timestamp(created), "updated_at": protocol.Timestamp(now), "deleted_at": deletedStamp, "source": "personal", "name": "Ejercicio", "load_mode": "external", "notes": notes, "archived_at": nil})
			if err != nil {
				return err
			}
			changes = append(changes, protocol.Change{EntityType: "personal_exercise", EntityID: id.String(), Revision: strconv.Itoa(revision), Data: data})
		}
		op := uuid.New()
		return f.data.Commands.Commit(tx, protocol.Receipt{OperationID: op, AccountID: f.actor.AccountID, ActorDeviceID: f.actor.DeviceID, OriginDeviceID: f.actor.DeviceID, RequestHash: sha256.Sum256([]byte(op.String())), CommandType: "personal_exercise.fixture", CapturedAt: protocol.Timestamp(now), CommittedAt: now, Result: protocol.OperationResult{OperationID: op.String(), Outcome: "accepted", CommittedAt: protocol.Timestamp(now)}}, changes)
	})
	if err != nil {
		t.Fatal("complete exercise fixture", err)
	}
}

func TestSnapshotPostgresPagesLatestTombstonesAndDrift(t *testing.T) {
	f := snapshotStorageTestNew(t)
	ids := make([]uuid.UUID, 1001)
	for i := range ids {
		ids[i] = uuid.New()
	}
	// Small records hit 1000-record page cap; batches respect group cardinality.
	snapshotStorageTestExerciseBatch(t, f, ids[:600], "", 1, false)
	snapshotStorageTestExerciseBatch(t, f, ids[600:], "", 1, false)
	snapshotStorageTestExerciseBatch(t, f, ids[:1], "Tombstone definitivo", 2, true)
	id := uuid.New()
	s, err := snapshotStorageTestCreate(t, f.create).Execute(context.Background(), pullActor(f.actor), id)
	if err != nil || s.PageCount != 2 || s.ThroughSequence != "3" {
		t.Fatal("record-count pagination", err)
	}
	pageUC := snapshotStorageTestPage(t, f.page)
	seen := map[string]bool{}
	var ordered []string
	for number := 0; number < s.PageCount; number++ {
		p, err := pageUC.Execute(context.Background(), pullActor(f.actor), id, number)
		if err != nil || p.Hash != snapshotStorageTestHash(t, p) || p.Hash != s.PageHashes[number] {
			t.Fatal("page integrity", err)
		}
		if number == 0 && len(p.Records) != 1000 {
			t.Fatal("count cut failed")
		}
		for _, c := range p.Records {
			key := c.EntityType + ":" + c.EntityID
			if seen[key] {
				t.Fatal("entity repeated across snapshot pages")
			}
			seen[key] = true
			ordered = append(ordered, key)
			if c.EntityID == ids[0].String() && (c.Revision != "2" || !strings.Contains(string(c.Data), "Tombstone definitivo") || strings.Contains(string(c.Data), `"deleted_at": null`)) {
				t.Fatal("latest tombstone missing")
			}
		}
	}
	if len(seen) != 1002 || !sort.StringsAreSorted(ordered) {
		t.Fatal("snapshot omitted root or stable order")
	}
	// A metadata revision without its canonical effect must abort publication.
	if err = f.owner.Exec("UPDATE mancuerna.personal_exercises SET revision=revision+1 WHERE account_id=? AND id=?", f.actor.AccountID, ids[1]).Error; err != nil {
		t.Fatal(err)
	}
	driftID := uuid.New()
	if got, err := snapshotStorageTestCreate(t, f.create).Execute(context.Background(), pullActor(f.actor), driftID); !errors.Is(err, protocol.ErrCommandPersistence) || got.ID != "" {
		t.Fatal("unpublished revision silently snapshotted", err)
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sync_snapshot_pages WHERE account_id=? AND snapshot_id=?", f.actor.AccountID, driftID) != 0 {
		t.Fatal("drift left pages")
	}
}

func TestSnapshotPostgresBytePagesAndCompaction(t *testing.T) {
	f := snapshotStorageTestNew(t)
	ids := make([]uuid.UUID, 1001)
	for i := range ids {
		ids[i] = uuid.New()
	}
	// Valid 1000-rune Unicode notes occupy 2000 bytes; size cuts before count.
	notes := strings.Repeat("á", 1000)
	for first := 0; first < len(ids); first += 300 {
		last := first + 300
		if last > len(ids) {
			last = len(ids)
		}
		snapshotStorageTestExerciseBatch(t, f, ids[first:last], notes, 1, false)
	}
	id := uuid.New()
	s, err := snapshotStorageTestCreate(t, f.create).Execute(context.Background(), pullActor(f.actor), id)
	if err != nil || s.PageCount < 3 {
		t.Fatal("byte page cut did not apply", err)
	}
	pageUC := snapshotStorageTestPage(t, f.page)
	total := 0
	for number := 0; number < s.PageCount; number++ {
		p, err := pageUC.Execute(context.Background(), pullActor(f.actor), id, number)
		if err != nil || p.Hash != snapshotStorageTestHash(t, p) || len(p.Records) >= 1000 {
			t.Fatal("byte cap lost integrity", err)
		}
		total += len(p.Records)
	}
	if total != 1002 {
		t.Fatal("byte pages omitted records")
	}
	var maximum int
	if err = f.owner.Raw("SELECT max(octet_length(data::text)) FROM mancuerna.sync_snapshot_pages WHERE account_id=? AND snapshot_id=?", f.actor.AccountID, id).Row().Scan(&maximum); err != nil || maximum > protocol.PullPageBytes {
		t.Fatal("JSONB storage bytes overflow", err)
	}
	if _, err = profileCommandUseCase(t, f.data).Execute(context.Background(), f.actor, profileCommand(f.actor, "1", "Tras snapshot")); err != nil {
		t.Fatal(err)
	}
	if err = f.owner.Exec("UPDATE mancuerna.sync_streams SET compacted_through=last_sequence WHERE account_id=?", f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = pageUC.Execute(context.Background(), pullActor(f.actor), id, 0); !errors.Is(err, protocol.ErrSnapshotExpired) {
		t.Fatal("compacted snapshot still available", err)
	}
}
