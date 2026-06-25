//go:build sidecar && bio_mock

package app

import (
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// stubGymRepo implementa GymRepository devolviendo un gym con una tz fija.
type stubGymRepo struct{ tz string }

func (s stubGymRepo) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*gymDomain.Gym, error) {
	return &gymDomain.Gym{ID: id, Timezone: s.tz}, nil
}
func (stubGymRepo) Create(_ sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error) {
	return g, nil
}
func (stubGymRepo) Update(_ sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error) {
	return g, nil
}
func (stubGymRepo) HasMembershipType(_ sharedDomain.Transaction, _ uuid.UUID) (bool, error) {
	return false, nil
}
func (stubGymRepo) ExistsByWhatsApp(_ sharedDomain.Transaction, _ string, _ uuid.UUID) (bool, error) {
	return false, nil
}

// El acceso evalúa la vigencia contra el día CALENDARIO del gym en su tz, no
// UTC. A las 22:00 hora de México (UTC-6) ya es el día siguiente en UTC, así
// que el día UTC adelantaba un día y rechazaba a socios en su último día.
func TestGymLocalToday(t *testing.T) {
	gymID := uuid.New()
	// 2026-06-20 04:00 UTC == 2026-06-19 22:00 en America/Mexico_City.
	now := time.Date(2026, 6, 20, 4, 0, 0, 0, time.UTC)

	got := gymLocalToday(nil, stubGymRepo{tz: "America/Mexico_City"}, gymID, now)
	want := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC) // día LOCAL del gym
	if !got.Equal(want) {
		t.Errorf("con tz del gym: gymLocalToday = %v, want %v", got, want)
	}

	// Sin repo cableado → cae al día UTC (comportamiento legacy, no rompe).
	gotUTC := gymLocalToday(nil, nil, gymID, now)
	wantUTC := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	if !gotUTC.Equal(wantUTC) {
		t.Errorf("fallback nil-repo: gymLocalToday = %v, want %v (UTC)", gotUTC, wantUTC)
	}

	// Timezone inválida → fallback UTC.
	gotBad := gymLocalToday(nil, stubGymRepo{tz: "Not/AZone"}, gymID, now)
	if !gotBad.Equal(wantUTC) {
		t.Errorf("fallback tz inválida: gymLocalToday = %v, want %v (UTC)", gotBad, wantUTC)
	}
}
