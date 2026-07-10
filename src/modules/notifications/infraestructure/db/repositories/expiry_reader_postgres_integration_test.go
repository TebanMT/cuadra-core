//go:build server && integration

package repositories

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// Regresión del incidente de prod (8–10 jul 2026): FindDueForStage moría en
// CADA tick con `operator does not exist: date = integer (42883)` y NINGÚN
// recordatorio se encoló durante 3 días. La causa: en `::date - ?` Postgres
// no conoce el tipo del parámetro bindeado y, entre `date - integer` y
// `date - date`, la resolución de operadores prefiere `date - date` (mismo
// tipo en ambos lados) → resultado integer → `expiry_date = integer` truena
// AL PLANEAR, sin importar los datos.
//
// El bug NO se reproduce con literales (un `- 3` a pelo se tipa integer) —
// por eso el smoke test con psql pasó y prod murió. LECCIÓN: los queries
// con parámetros en expresiones aritméticas se prueban A TRAVÉS del driver
// (GORM bind), nunca sólo con literales. Eso es exactamente lo que hace
// este test. Run:
//
//	go test -tags 'server integration' ./src/modules/notifications/...

func seedExpiryFixture(t *testing.T, db *gorm.DB, expiry string) uuid.UUID {
	t.Helper()
	gymID := uuid.New()
	userID := uuid.New()
	memberID := uuid.New()
	mtID := uuid.New()
	msID := uuid.New()
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
		VALUES (?, ?, 1, NOW(), NOW(), 'Expiry Reader Gym', 'MX', 'America/Mexico_City')`,
		gymID, gymID).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, full_name, role, email)
		VALUES (?, ?, 1, NOW(), NOW(), 'Owner', 'owner', ?)`,
		userID, gymID, uuid.NewString()+"@test.mx").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO members (id, gym_id, version, created_at, updated_at, full_name, phone, status, folio, created_by)
		VALUES (?, ?, 1, NOW(), NOW(), 'Rosa Robles', '+525522334455', 'active', 1, ?)`,
		memberID, gymID, userID).Error; err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO membership_types (id, gym_id, version, created_at, updated_at, name, price, duration_days)
		VALUES (?, ?, 1, NOW(), NOW(), 'Mensual', 500, 30)`,
		mtID, gymID).Error; err != nil {
		t.Fatalf("seed membership_type: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO memberships (id, gym_id, version, created_at, updated_at, member_id, membership_type_id,
		    type_name_snapshot, price_snapshot, duration_days_snapshot, start_date, expiry_date, status)
		VALUES (?, ?, 1, NOW(), NOW(), ?, ?, 'Mensual', 500, 30, ?::date - 30, ?::date, 'active')`,
		msID, gymID, memberID, mtID, expiry, expiry).Error; err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"memberships", "membership_types", "members", "users", "gyms"} {
			_ = db.Exec("DELETE FROM "+table+" WHERE gym_id = ?", gymID).Error
		}
	})
	return gymID
}

// TestFindDueForStage_BindsThroughDriver — las tres etapas, con el bind
// REAL de GORM (no literales), y el caso nocturno que motivó el fix tz:
// a las 10 PM de CDMX (04:00 UTC del día siguiente) la etapa se evalúa
// sobre el día local del gym.
func TestFindDueForStage_BindsThroughDriver(t *testing.T) {
	db := testutil.OpenPostgres(t)
	reader := NewExpiryPostgresReader()

	// "Hoy" local del gym = día D. Simulamos las 10 PM de CDMX del día D:
	// now = D+1 04:00 UTC. La membresía del fixture vence según la etapa.
	localToday := time.Now().UTC().Add(-6 * time.Hour) // día local CDMX aprox
	day := time.Date(localToday.Year(), localToday.Month(), localToday.Day(), 0, 0, 0, 0, time.UTC)
	now := day.AddDate(0, 0, 1).Add(4 * time.Hour) // D+1 04:00 UTC = D 10 PM CDMX

	cases := []struct {
		name   string
		offset int
		expiry time.Time // lo que debe matchear: hoy_local - offset
	}{
		{"etapa -3 (vence en 3 días)", -3, day.AddDate(0, 0, 3)},
		{"etapa 0 (vence hoy)", 0, day},
		{"etapa +5 (persecución post-vencimiento)", 5, day.AddDate(0, 0, -5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gymID := seedExpiryFixture(t, db, tc.expiry.Format("2006-01-02"))
			tx := &sharedDomain.GormTransaction{Tx: db}

			got, err := reader.FindDueForStage(tx, now, tc.offset)
			if err != nil {
				t.Fatalf("FindDueForStage(offset=%d): %v — regresión 42883 'date = integer' (bind del driver)", tc.offset, err)
			}
			found := false
			for _, c := range got {
				if c.GymID == gymID {
					found = true
					if c.GymTimezone != "America/Mexico_City" {
						t.Errorf("tz = %q", c.GymTimezone)
					}
					if !c.ExpiryDate.Equal(tc.expiry) {
						t.Errorf("expiry = %v, want %v", c.ExpiryDate, tc.expiry)
					}
					if c.MemberPhone == "" {
						t.Error("phone vacío")
					}
				}
			}
			if !found {
				t.Errorf("el candidato del gym %s no apareció (offset %d, expiry %s, now %s)",
					gymID, tc.offset, tc.expiry.Format("2006-01-02"), now)
			}
		})
	}
}
