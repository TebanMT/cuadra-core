//go:build server && integration

package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	gymRepoPg "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	"github.com/cuadra/cuadra-core/src/modules/users/domain/legal"
	usersRepoPg "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

func TestSignupOwnerRecordsLegalAcceptance(t *testing.T) {
	db := testutil.OpenPostgres(t)
	uow := shared.NewPostgresUnitOfWork(db)
	uc := usersApp.NewSignupOwner(usersRepoPg.NewUserPostgresRepository(), gymRepoPg.NewGymPostgresRepository(),
		uow, auth.NewJWTService("legal-test-secret"), audit.NewPostgresRecorder(), 30).
		WithLegalAcceptances(usersRepoPg.NewLegalAcceptancePostgresRepository())

	ctx := audit.WithUA(audit.WithIP(context.Background(), "201.141.10.4"), "Mozilla/5.0 "+strings.Repeat("x", 600))
	email := "legal-" + time.Now().UTC().Format("150405.000000") + "@example.test"
	out, err := uc.Execute(ctx, usersApp.SignupOwnerInput{
		FullName:        "Dueña Prueba",
		Email:           email,
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
		AcceptedLegal:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"legal_acceptances", "audit_log", "sync_entities", "users", "cash_drawers", "gyms"} {
			db.Exec("DELETE FROM "+table+" WHERE gym_id=?", out.GymID)
		}
	})

	var rows []struct {
		Document, Version, IPAddress, UserAgent string
		UserID                                  string
	}
	if err := db.Raw(`SELECT document, version, ip_address, user_agent, user_id::text AS user_id
		FROM legal_acceptances WHERE gym_id=? ORDER BY document`, out.GymID).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 legal_acceptances rows, got %d", len(rows))
	}
	want := map[string]string{legal.DocumentPrivacy: legal.PrivacyVersion, legal.DocumentTerms: legal.TermsVersion}
	for _, r := range rows {
		if want[r.Document] != r.Version || r.UserID != out.UserID.String() {
			t.Errorf("unexpected row %+v", r)
		}
		if r.IPAddress != "201.141.10.4" || len(r.UserAgent) != 512 {
			t.Errorf("request metadata not kept/clipped: ip=%q ua_len=%d", r.IPAddress, len(r.UserAgent))
		}
	}

	var rls bool
	if err := db.Raw(`SELECT relrowsecurity FROM pg_class WHERE oid='public.legal_acceptances'::regclass`).Scan(&rls).Error; err != nil || !rls {
		t.Fatalf("legal_acceptances must have RLS enabled (err=%v)", err)
	}
}
