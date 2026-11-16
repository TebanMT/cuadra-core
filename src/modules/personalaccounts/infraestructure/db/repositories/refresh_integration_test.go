//go:build server && integration

package repositories

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/refresh"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func refreshTestDependencies(t *testing.T) personalApp.RefreshSessionDependencies {
	t.Helper()
	d := finalizationDependencies(t)
	resolver, err := NewRefreshIdentityResolver(budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	return personalApp.RefreshSessionDependencies{UoW: d.UoW, Identity: resolver, Rotations: NewRefreshSessionRepository(), Sessions: d.Sessions, Events: d.Events, Tokens: d.Tokens, Proofs: d.Proofs, Envelopes: d.Envelopes, Now: time.Now}
}
func refreshTestFixture(t *testing.T) (personalApp.RefreshSessionDependencies, *gorm.DB, personalApp.SessionTokens, ed25519.PrivateKey) {
	t.Helper()
	signup := finalizationDependencies(t)
	_, in, key := verifiedAccessFixture(t, signup, accessTestPhone(), nil)
	u, err := personalApp.NewFinalizeAccess(signup)
	if err != nil {
		t.Fatal(err)
	}
	result, err := u.Execute(context.Background(), in)
	if err != nil || result.Authorized == nil {
		t.Fatalf("signup fixture failed: %v", err)
	}
	return refreshTestDependencies(t), budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN"), result.Authorized.Session, key
}
func refreshTestInput(t *testing.T, s personalApp.SessionTokens, key ed25519.PrivateKey, operationID uuid.UUID) personalApp.RefreshSessionInput {
	t.Helper()
	generation, err := strconv.ParseInt(s.Generation, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(s.RefreshToken))
	payload, err := crypto.RefreshProofPayload(security.RefreshProof{OperationID: operationID, SessionID: uuid.MustParse(s.SessionID), Generation: generation, RefreshHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
	return personalApp.RefreshSessionInput{OperationID: operationID, RequestHash: sha256.Sum256([]byte(s.RefreshToken + "\x00" + signature)), RefreshToken: s.RefreshToken, DeviceSignature: signature}
}
func refreshTestUseCase(t *testing.T, d personalApp.RefreshSessionDependencies) *personalApp.RefreshSession {
	t.Helper()
	u, err := personalApp.NewRefreshSession(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func refreshTestActive(t *testing.T, d personalApp.RefreshSessionDependencies, s personalApp.SessionTokens) bool {
	return authSessionTestActive(t, d.UoW, NewAuthSessionRepository(), uuid.MustParse(s.AccountID), uuid.MustParse(s.SessionID), uuid.MustParse(s.DeviceID))
}

func TestRefreshSessionPostgresConcurrentReplayRotatesOnceAndPreservesLogout(t *testing.T) {
	d, owner, old, key := refreshTestFixture(t)
	var family uuid.UUID
	if err := owner.Raw("SELECT refresh_family_id FROM mancuerna.sessions WHERE id=?", old.SessionID).Row().Scan(&family); err != nil {
		t.Fatal(err)
	}
	u := refreshTestUseCase(t, d)
	in := refreshTestInput(t, old, key, uuid.New())
	const count = 8
	results := make([]personalApp.SessionTokens, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; results[i], errs[i] = u.Execute(context.Background(), in) }(i)
	}
	close(start)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !reflect.DeepEqual(results[i], results[0]) {
			t.Fatalf("identical refresh diverged: %v", errs[i])
		}
	}
	next := results[0]
	if next.SessionID != old.SessionID || next.AccountID != old.AccountID || next.DeviceID != old.DeviceID || next.Generation != "2" || next.RefreshToken == old.RefreshToken || next.RevocationTicket == old.RevocationTicket || !refreshTestActive(t, d, next) {
		t.Fatal("rotation changed identity or retained credentials")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.session_rotations WHERE session_id=?", old.SessionID) != 1 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE account_id=? AND event_type='session_refreshed'", old.AccountID) != 1 {
		t.Fatal("concurrent refresh duplicated effects")
	}
	oldHash := sha256.Sum256([]byte(old.RefreshToken))
	newHash := sha256.Sum256([]byte(next.RefreshToken))
	oldTicket := sha256.Sum256([]byte(old.RevocationTicket))
	newTicket := sha256.Sum256([]byte(next.RevocationTicket))
	for _, hash := range [][32]byte{oldHash, newHash} {
		id, err := d.Identity.Resolve(context.Background(), hash)
		if err != nil || id.String() != old.AccountID {
			t.Fatal("old/current refresh did not resolve same account")
		}
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.session_rotations WHERE session_id=? AND generation=1 AND old_refresh_hash=? AND old_revocation_ticket_hash=?", old.SessionID, oldHash[:], oldTicket[:]) != 1 {
		t.Fatal("consumed credential metadata not retained")
	}
	authDB := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	for _, ticket := range [][32]byte{oldTicket, newTicket} {
		var id uuid.UUID
		if err := authDB.Raw("SELECT mancuerna_private.resolve_identity('logout',?)", hex.EncodeToString(ticket[:])).Row().Scan(&id); err != nil || id.String() != old.AccountID {
			t.Fatalf("lost-response logout permission lost: %v", err)
		}
	}
	q, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), uuid.MustParse(old.AccountID)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := d.Sessions.GetSession(q, uuid.MustParse(old.AccountID), uuid.MustParse(old.SessionID))
	shared.CloseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	if s.RefreshFamilyID != family || s.RefreshHash != newHash || s.RevocationTicketHash != newTicket || s.LastUsedAt == nil || !s.ExpiresAt.Equal(s.LastUsedAt.Add(90*24*time.Hour)) {
		t.Fatal("rotation state does not match result")
	}
	latest, err := u.Execute(context.Background(), refreshTestInput(t, next, key, uuid.New()))
	if err != nil || latest.Generation != "3" || latest.SessionID != old.SessionID {
		t.Fatalf("new generation not usable: %v", err)
	}
}

func TestRefreshSessionPostgresRejectsReuseAndInvalidProofWithoutRevocation(t *testing.T) {
	d, owner, old, key := refreshTestFixture(t)
	u := refreshTestUseCase(t, d)
	in := refreshTestInput(t, old, key, uuid.New())
	next, err := u.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	reuse := refreshTestInput(t, old, key, uuid.New())
	if _, err = u.Execute(context.Background(), reuse); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatalf("old token reused with new ID: %v", err)
	}
	changed := in
	changed.RequestHash[0]++
	if _, err = u.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("old operation content changed")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE account_id=? AND event_type='refresh_replay_rejected'", old.AccountID) != 2 {
		t.Fatal("signed reuse rejection not audited")
	}
	unsigned := reuse
	unsigned.DeviceSignature = base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	if _, err = u.Execute(context.Background(), unsigned); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("unsigned request accepted")
	}
	wrongGen := old
	wrongGen.Generation = "2"
	if _, err = u.Execute(context.Background(), refreshTestInput(t, wrongGen, key, uuid.New())); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("proof for wrong consumed generation accepted")
	}
	if !refreshTestActive(t, d, next) || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.session_rotations WHERE session_id=?", old.SessionID) != 1 {
		t.Fatal("rejected reuse revoked or rotated family")
	}
	unknown := in
	unknown.RefreshToken = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if _, err = u.Execute(context.Background(), unknown); !errors.Is(err, personalErrors.ErrInvalidRefresh) {
		t.Fatalf("unknown refresh leaked identity: %v", err)
	}
}

func TestRefreshSessionPostgresExpiryRevocationAndRollback(t *testing.T) {
	d, owner, old, key := refreshTestFixture(t)
	in := refreshTestInput(t, old, key, uuid.New())
	failing := d
	failing.Events = failingSecurityEvent{}
	if result, err := refreshTestUseCase(t, failing).Execute(context.Background(), in); err == nil || result != (personalApp.SessionTokens{}) {
		t.Fatal("audit failure returned refresh credentials")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.session_rotations WHERE session_id=?", old.SessionID) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sessions WHERE id=? AND generation=1", old.SessionID) != 1 {
		t.Fatal("audit failure retained rotation")
	}
	u := refreshTestUseCase(t, d)
	next, err := u.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var created time.Time
	if err = owner.Raw("SELECT created_at FROM mancuerna.session_rotations WHERE operation_id=?", in.OperationID).Row().Scan(&created); err != nil {
		t.Fatal(err)
	}
	expired := d
	expired.Now = func() time.Time { return created.Add(refresh.ResponseTTL) }
	if _, err = refreshTestUseCase(t, expired).Execute(context.Background(), in); !errors.Is(err, personalErrors.ErrResultExpired) {
		t.Fatalf("old response survived boundary: %v", err)
	}
	if !refreshTestActive(t, d, next) {
		t.Fatal("expired result revoked current session")
	}
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), uuid.MustParse(old.AccountID)), func(tx shared.Transaction) error {
		return d.Sessions.RevokeSession(tx, uuid.MustParse(old.AccountID), uuid.MustParse(old.SessionID), time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	for _, input := range []personalApp.RefreshSessionInput{in, refreshTestInput(t, next, key, uuid.New())} {
		if _, err = u.Execute(context.Background(), input); !errors.Is(err, personalErrors.ErrSessionRevoked) {
			t.Fatalf("revoked actor recovered result or rotated: %v", err)
		}
	}
	for _, mutate := range []string{"UPDATE mancuerna.sessions SET created_at=clock_timestamp()-interval '2 days',expires_at=clock_timestamp()-interval '1 second' WHERE id=?", "UPDATE mancuerna.devices SET deleted_at=clock_timestamp() WHERE id=?", "UPDATE mancuerna.accounts SET deleted_at=clock_timestamp() WHERE id=?"} {
		activeD, activeOwner, active, private := refreshTestFixture(t)
		id := active.SessionID
		if strings.Contains(mutate, "devices") {
			id = active.DeviceID
		}
		if strings.Contains(mutate, "accounts") {
			id = active.AccountID
		}
		if err = activeOwner.Exec(mutate, id).Error; err != nil {
			t.Fatal(err)
		}
		if _, err = refreshTestUseCase(t, activeD).Execute(context.Background(), refreshTestInput(t, active, private, uuid.New())); !errors.Is(err, personalErrors.ErrSessionRevoked) {
			t.Fatalf("inactive session renewed: %v", err)
		}
	}
}

func TestRefreshSessionPostgresResolverAndRepositoryIsolation(t *testing.T) {
	d, owner, a, keyA := refreshTestFixture(t)
	_, _, b, keyB := refreshTestFixture(t)
	for _, dsn := range []string{"MANCUERNA_TEST_OWNER_DSN", "MANCUERNA_TEST_DATA_DSN"} {
		if _, err := NewRefreshIdentityResolver(budgetTestPool(t, dsn)); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
			t.Fatal("unsafe resolver pool accepted")
		}
	}
	ha := sha256.Sum256([]byte(a.RefreshToken))
	hb := sha256.Sum256([]byte(b.RefreshToken))
	if _, err := d.Identity.Resolve(shared.WithPersonalAccount(context.Background(), uuid.MustParse(a.AccountID)), ha); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("prelogin resolver accepted account scope")
	}
	for _, ctx := range []context.Context{shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{9}), shared.WithPersonalAccount(context.Background(), uuid.Nil), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := d.Identity.Resolve(ctx, ha); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("prelogin resolver accepted ambient capability")
		}
		if _, err := refreshTestUseCase(t, d).Execute(ctx, refreshTestInput(t, a, keyA, uuid.New())); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("refresh inherited ambient capability")
		}
	}
	err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), uuid.MustParse(a.AccountID)), func(tx shared.Transaction) error {
		_, _, err := d.Rotations.GetForUpdate(tx, uuid.MustParse(a.AccountID), hb)
		return err
	})
	if !errors.Is(err, personalErrors.ErrInvalidRefresh) {
		t.Fatal("foreign refresh visible under A scope")
	}
	u := refreshTestUseCase(t, d)
	bad := refreshTestInput(t, a, keyB, uuid.New())
	if _, err = u.Execute(context.Background(), bad); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("foreign device signature authorized refresh")
	}
	op := uuid.New()
	if _, err = u.Execute(context.Background(), refreshTestInput(t, a, keyA, op)); err != nil {
		t.Fatal(err)
	}
	if _, err = u.Execute(context.Background(), refreshTestInput(t, b, keyB, op)); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatalf("same operation ID reused across account: %v", err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sessions WHERE id=? AND generation=1", b.SessionID) != 1 {
		t.Fatal("receipt collision consumed another account's token")
	}
}
