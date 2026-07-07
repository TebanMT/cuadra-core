package app

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Pin del filtro de vigencia (dogfood 6-jul-2026): CurrentlyValid debe
// llegar al repo TRUNCADO al día calendario del gym en su tz. Antes
// llegaba el timestamp UTC completo, lo que (a) corría el día desde las
// 6 PM en CDMX y (b) excluía a las promos durante TODO su último día de
// vigencia (valid_until es DATE = medianoche; medianoche >= now-con-horas
// es falso desde las 00:01).

type capturePromoRepo struct {
	promoRepo.PromotionRepository
	got promoRepo.ListFilter
}

func (f *capturePromoRepo) List(_ sharedDomain.Transaction, filter promoRepo.ListFilter) ([]*promoDomain.Promotion, error) {
	f.got = filter
	return nil, nil
}

type listFakeGyms struct {
	gymRepo.GymRepository
	gym *gymDomain.Gym
}

func (f *listFakeGyms) GetByID(_ sharedDomain.Transaction, _ uuid.UUID) (*gymDomain.Gym, error) {
	return f.gym, nil
}

type listFakeTx struct{}

func (listFakeTx) Execute(fn func(tx sharedDomain.Transaction) error) error {
	return fn(listFakeTx{})
}

type listFakeUoW struct{}

func (listFakeUoW) Begin(context.Context) (sharedDomain.Transaction, error) { return listFakeTx{}, nil }
func (listFakeUoW) Commit(sharedDomain.Transaction) error                   { return nil }
func (listFakeUoW) Rollback(sharedDomain.Transaction) error                 { return nil }
func (listFakeUoW) Query(context.Context) (sharedDomain.Transaction, error) { return listFakeTx{}, nil }
func (listFakeUoW) Command(_ context.Context, fn func(tx sharedDomain.Transaction) error) error {
	return fn(listFakeTx{})
}

func TestListPromotions_VigenciaSeEvaluaEnDiaLocalDelGym(t *testing.T) {
	repo := &capturePromoRepo{}
	uc := NewListPromotions(repo, listFakeUoW{}).WithGyms(&listFakeGyms{
		gym: &gymDomain.Gym{ID: uuid.New(), Timezone: "America/Mexico_City"},
	})

	// 6-jul 10:00 PM CDMX == 7-jul 04:00 UTC: el día de vigencia sigue
	// siendo el 6.
	now := time.Date(2026, 7, 7, 4, 0, 0, 0, time.UTC)
	_, err := uc.Execute(context.Background(), ListPromotionsInput{
		GymID: uuid.New(), CurrentlyValid: &now,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if repo.got.CurrentlyValid == nil || !repo.got.CurrentlyValid.Equal(want) {
		t.Errorf("CurrentlyValid al repo = %v, want día local truncado %v", repo.got.CurrentlyValid, want)
	}
}

func TestListPromotions_SinGymsTruncaAlDiaUTC(t *testing.T) {
	repo := &capturePromoRepo{}
	uc := NewListPromotions(repo, listFakeUoW{})

	now := time.Date(2026, 7, 7, 4, 30, 0, 0, time.UTC)
	if _, err := uc.Execute(context.Background(), ListPromotionsInput{
		GymID: uuid.New(), CurrentlyValid: &now,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Fallback: día UTC, pero TRUNCADO — el timestamp con horas es lo que
	// excluía a las promos en su último día.
	want := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	if repo.got.CurrentlyValid == nil || !repo.got.CurrentlyValid.Equal(want) {
		t.Errorf("CurrentlyValid al repo = %v, want día UTC truncado %v", repo.got.CurrentlyValid, want)
	}
}
