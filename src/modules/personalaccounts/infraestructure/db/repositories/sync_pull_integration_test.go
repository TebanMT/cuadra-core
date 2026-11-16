//go:build server && integration

package repositories

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/controllers"
	personalMiddleware "github.com/cuadra/cuadra-core/src/modules/personalaccounts/interfaces/middleware"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	syncApp "github.com/cuadra/cuadra-core/src/shared/sync/personal/app"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	streamRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	cursorCrypto "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func pullFixture(t *testing.T) (*gorm.DB, security.AccessClaims, *syncApp.Pull, protocol.CursorSigner, protocol.CursorBinding) {
	t.Helper()
	owner, actor, _ := meTestFixture(t, finalizationDependencies(t))
	d := profileCommandDependencies(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cursors, err := cursorCrypto.NewCursorSigner("fixture", map[string][]byte{"fixture": key})
	if err != nil {
		t.Fatal(err)
	}
	pull, err := syncApp.NewPull(syncApp.PullDependencies{UoW: d.UoW, Commands: d.Commands, Stream: streamRepos.NewStream(), Cursors: cursors, Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	var epoch uuid.UUID
	if err = owner.Raw("SELECT epoch FROM mancuerna.sync_streams WHERE account_id=?", actor.AccountID).Row().Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return owner, actor, pull, cursors, protocol.PullBinding(actor.AccountID, epoch)
}
func pullActor(actor security.AccessClaims) protocol.Actor {
	return protocol.Actor{AccountID: actor.AccountID, SessionID: actor.SessionID, DeviceID: actor.DeviceID}
}
func issuePull(t *testing.T, c protocol.CursorSigner, b protocol.CursorBinding, seq string, at time.Time) string {
	t.Helper()
	token, err := c.Issue(b, protocol.CursorPosition{Sequence: seq}, at)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func TestSyncPullPostgresWholeGroupsPaginationAndLostResponse(t *testing.T) {
	_, actor, pull, cursors, binding := pullFixture(t)
	update := profileCommandUseCase(t, profileCommandDependencies(t))
	for i := 1; i <= 3; i++ {
		base := strconv.Itoa(i)
		if i == 3 {
			base = "1"
		}
		result, err := update.Execute(context.Background(), actor, profileCommand(actor, base, "Perfil "+strconv.Itoa(i)))
		if err != nil || (i < 3 && result.Outcome != "accepted") || (i == 3 && result.Outcome != "conflict") {
			t.Fatal("command fixture", err)
		}
	}
	cursor := issuePull(t, cursors, binding, "0", time.Now())
	first, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: cursor, Limit: 2})
	if err != nil || len(first.Groups) != 2 || !first.HasMore || first.Groups[0].Sequence != "1" || first.Groups[1].Sequence != "2" {
		t.Fatal("first page", err)
	}
	retry, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: cursor, Limit: 2})
	if err != nil || !reflect.DeepEqual(first.Groups, retry.Groups) {
		t.Fatal("lost response changed groups", err)
	}
	second, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: first.NextCursor, Limit: 2})
	if err != nil || len(second.Groups) != 1 || second.HasMore || second.Groups[0].Changes[0].EntityType != "conflict" {
		t.Fatal("conflict omitted", err)
	}
	empty, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: second.NextCursor})
	if err != nil || empty.Groups == nil || len(empty.Groups) != 0 || empty.HasMore {
		t.Fatal("caught up page", err)
	}
	if _, err = update.Execute(context.Background(), actor, profileCommand(actor, "3", "Después")); err != nil {
		t.Fatal(err)
	}
	more, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: empty.NextCursor})
	if err != nil || len(more.Groups) != 1 || more.Groups[0].Sequence != "4" {
		t.Fatal("post-page commit lost", err)
	}
}

// This fixture targets the journal storage boundary, not the publication use
// case. It supplies complete version-shaped records with bounded nested fields
// through Commands.Commit and never bypasses the restricted data transaction.
func pullLargeVersion(t *testing.T, id uuid.UUID) json.RawMessage {
	t.Helper()
	days := make([]any, 14)
	for day := range days {
		exercises := make([]any, 50)
		for position := range exercises {
			exercises[position] = map[string]any{
				"id": uuid.New().String(), "exercise_ref": map[string]any{"source": "catalog", "id": uuid.New().String()},
				"name_snapshot": "Ejercicio", "load_mode_snapshot": "external", "position": position,
				"prescribed_sets": 3, "prescribed_reps": "8", "rest_seconds": nil, "notes": strings.Repeat("n", 650),
			}
		}
		days[day] = map[string]any{"id": uuid.New().String(), "label": "Día", "position": day, "exercises": exercises}
	}
	content, err := json.Marshal(days)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	raw, err := json.Marshal(map[string]any{
		"id": id.String(), "routine_id": uuid.New().String(), "revision": "1", "version_number": 1,
		"title_snapshot": "Versión grande", "format_version": 1, "source_draft_revision": "1",
		"content_hash": hex.EncodeToString(digest[:]), "published_at": protocol.Timestamp(time.Now()), "days": days,
	})
	if err != nil || len(raw) <= protocol.PullPageBytes/2 || len(raw) >= protocol.PullPageBytes {
		t.Fatal("large fixture outside required byte range", len(raw), err)
	}
	return raw
}

func TestSyncPullPostgresBoundsMaterializedBytesWithoutSkippingLargeGroups(t *testing.T) {
	owner, actor, pull, cursors, binding := pullFixture(t)
	d := profileCommandDependencies(t)
	ctx, err := shared.WithAuthenticatedPersonalAccount(context.Background(), actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	operations := make([]string, 3)
	for i := range operations {
		operationID, entityID := uuid.New(), uuid.New()
		operations[i] = operationID.String()
		data := pullLargeVersion(t, entityID)
		now := time.Now().UTC()
		err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
			if err := d.Commands.RequireActor(tx, pullActor(actor), actor.DeviceID); err != nil {
				return err
			}
			return d.Commands.Commit(tx, protocol.Receipt{
				OperationID: operationID, AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OriginDeviceID: actor.DeviceID,
				RequestHash: sha256.Sum256(data), CommandType: "routine.publish", CapturedAt: protocol.Timestamp(now), CommittedAt: now,
				Result: protocol.OperationResult{OperationID: operationID.String(), Outcome: "accepted", CommittedAt: protocol.Timestamp(now)},
			}, []protocol.Change{{EntityType: "routine_version", EntityID: entityID.String(), Revision: "1", Data: data}})
		})
		if err != nil {
			t.Fatal("large journal fixture", err)
		}
	}
	var minimum, maximum int
	if err = owner.Raw("SELECT min(octet_length(changes::text)),max(octet_length(changes::text)) FROM mancuerna.sync_changes WHERE account_id=?", actor.AccountID).Row().Scan(&minimum, &maximum); err != nil || minimum <= protocol.PullPageBytes/2 || maximum > protocol.PullPageBytes {
		t.Fatal("stored groups do not exercise two-row byte cut", minimum, maximum, err)
	}
	stream := streamRepos.NewStream()
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := d.Commands.RequireActor(tx, pullActor(actor), actor.DeviceID); err != nil {
			return err
		}
		groups, err := stream.Groups(tx, actor.AccountID, 0, 101)
		if err != nil {
			return err
		}
		if len(groups) != 2 || groups[0].OperationID != operations[0] || groups[1].OperationID != operations[1] {
			t.Errorf("repository did not stop after the crossing whole group: count=%d", len(groups))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cursor := issuePull(t, cursors, binding, "0", time.Now())
	for i, operationID := range operations {
		page, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: cursor, Limit: 100})
		if err != nil || len(page.Groups) != 1 || page.Groups[0].OperationID != operationID || page.Groups[0].Sequence != strconv.Itoa(i+1) || page.HasMore != (i < 2) {
			t.Fatalf("page %d omitted/repeated a group or advanced past the byte cut: %v", i, err)
		}
		position, err := cursors.Verify(page.NextCursor, binding, time.Now())
		if err != nil || position.Sequence != strconv.Itoa(i+1) {
			t.Fatal("cursor advanced through an omitted group", err)
		}
		cursor = page.NextCursor
	}
	last, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: cursor, Limit: 100})
	if err != nil || len(last.Groups) != 0 || last.HasMore {
		t.Fatal("stream did not finish after all three groups", err)
	}
}
func TestSyncPullPostgresCursorIsolationCompactionEpochAndBigInt(t *testing.T) {
	owner, actor, pull, cursors, binding := pullFixture(t)
	base := int64(9007199254740992)
	if err := owner.Exec("UPDATE mancuerna.sync_streams SET last_sequence=?,compacted_through=? WHERE account_id=?", base, base, actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	update := profileCommandUseCase(t, profileCommandDependencies(t))
	if _, err := update.Execute(context.Background(), actor, profileCommand(actor, "1", "Grande")); err != nil {
		t.Fatal(err)
	}
	cursor := issuePull(t, cursors, binding, strconv.FormatInt(base, 10), time.Now())
	page, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: cursor})
	if err != nil || len(page.Groups) != 1 || page.Groups[0].Sequence != strconv.FormatInt(base+1, 10) {
		t.Fatal("BIGINT rounded", err)
	}
	cases := []struct {
		token string
		want  error
	}{
		{"", protocol.ErrCursorExpired},
		{cursor + "x", protocol.ErrCursorInvalid},
		{issuePull(t, cursors, binding, "0", time.Now()), protocol.ErrCursorExpired},
		{issuePull(t, cursors, binding, strconv.FormatInt(base+2, 10), time.Now()), protocol.ErrCursorInvalid},
		{issuePull(t, cursors, binding, strconv.FormatInt(base, 10), time.Now().Add(-31*24*time.Hour)), protocol.ErrCursorExpired},
	}
	for i, c := range cases {
		if _, err = pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: c.token}); !errors.Is(err, c.want) {
			t.Fatalf("cursor case %d: %v", i, err)
		}
	}
	_, other, _ := meTestFixture(t, finalizationDependencies(t))
	if _, err = pull.Execute(context.Background(), pullActor(other), syncApp.PullInput{Cursor: cursor}); !errors.Is(err, protocol.ErrCursorInvalid) {
		t.Fatal("foreign cursor revealed epoch", err)
	}
	if err = owner.Exec("UPDATE mancuerna.sync_streams SET epoch=? WHERE account_id=?", uuid.New(), actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: cursor}); !errors.Is(err, protocol.ErrCursorEpochChanged) {
		t.Fatal("old epoch accepted", err)
	}
}
func TestSyncPullPostgresRevocationWinsBeforeReadAndMixedScope(t *testing.T) {
	owner, actor, pull, cursors, binding := pullFixture(t)
	cursor := issuePull(t, cursors, binding, "0", time.Now())
	for _, ctx := range []context.Context{shared.WithTenant(context.Background(), uuid.New()), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1})} {
		if _, err := pull.Execute(ctx, pullActor(actor), syncApp.PullInput{Cursor: cursor}); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed scope accepted", err)
		}
	}
	tx := owner.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("SELECT 1 FROM mancuerna.sync_streams WHERE account_id=? FOR UPDATE", actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", actor.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := pull.Execute(context.Background(), pullActor(actor), syncApp.PullInput{Cursor: cursor})
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("read escaped stream mutex: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, protocol.ErrCommandActor) {
			t.Fatal("revoked actor read page", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("pull remained blocked")
	}
}
func TestSyncPullHTTPPostgresContractAndRevokedJWT(t *testing.T) {
	d := finalizationDependencies(t)
	_, in, _ := verifiedAccessFixture(t, d, accessTestPhone(), nil)
	finalize, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	signup, err := finalize.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := d.Tokens.Verify(signup.Authorized.Session.AccessToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pd := profileCommandDependencies(t)
	key := make([]byte, 32)
	rand.Read(key)
	cursors, _ := cursorCrypto.NewCursorSigner("fixture", map[string][]byte{"fixture": key})
	pull, err := syncApp.NewPull(syncApp.PullDependencies{UoW: pd.UoW, Commands: pd.Commands, Stream: streamRepos.NewStream(), Cursors: cursors, Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	var epoch uuid.UUID
	if err = owner.Raw("SELECT epoch FROM mancuerna.sync_streams WHERE account_id=?", actor.AccountID).Row().Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	cursor := issuePull(t, cursors, protocol.PullBinding(actor.AccountID, epoch), "0", time.Now())
	if _, err = profileCommandUseCase(t, pd).Execute(context.Background(), actor, profileCommand(actor, "1", "HTTP")); err != nil {
		t.Fatal(err)
	}
	h, _ := controllers.NewSyncPullController(pull)
	middleware, _ := personalMiddleware.NewPersonal(d.Tokens, time.Now)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/mancuerna/v1/sync/pull", middleware, h.Get)
	call := func(query string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/mancuerna/v1/sync/pull"+query, nil)
		req.Header.Set("Authorization", "Bearer "+signup.Authorized.Session.AccessToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("page cacheable")
		}
		var body map[string]any
		if json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatal("invalid response")
		}
		return w.Code, body
	}
	if code, body := call(""); code != 410 || body["code"] != "CURSOR_EXPIRED" {
		t.Fatal("missing cursor", code, body)
	}
	query := "?cursor=" + url.QueryEscape(cursor) + "&limit=1"
	if code, body := call(query); code != 200 || len(body) != 5 || len(body["groups"].([]any)) != 1 {
		t.Fatal("public page", code)
	}
	if code, _ := call(query + "&limit=2"); code != 400 {
		t.Fatal("duplicate query accepted", code)
	}
	if err = owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", actor.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if code, body := call(query); code != 401 || body["code"] != "SESSION_REVOKED" {
		t.Fatal("revoked JWT read", code, body)
	}
}
