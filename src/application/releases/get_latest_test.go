package releases

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeTx struct{}

func (fakeTx) Execute(fn func(tx sharedDomain.Transaction) error) error { return fn(fakeTx{}) }

type fakeUoW struct{}

func (fakeUoW) Begin(_ context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Commit(_ sharedDomain.Transaction) error                   { return nil }
func (fakeUoW) Rollback(_ sharedDomain.Transaction) error                 { return nil }
func (fakeUoW) Query(_ context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Command(_ context.Context, fn func(tx sharedDomain.Transaction) error) error {
	return fn(fakeTx{})
}

type fakeReader struct {
	rel *Release
	err error
}

func (f *fakeReader) LatestForTarget(_ sharedDomain.Transaction, _, _ string) (*Release, error) {
	return f.rel, f.err
}

// ---------------------------------------------------------------------------
// inRolloutBucket
// ---------------------------------------------------------------------------

func TestInRolloutBucket_Boundaries(t *testing.T) {
	someGym := uuid.New()
	cases := []struct {
		name    string
		rollout int
		gymID   *uuid.UUID
		want    bool
	}{
		{"100 sin gym ofrece GA", 100, nil, true},
		{"100 con gym ofrece GA", 100, &someGym, true},
		{"0 nunca ofrece (con gym)", 0, &someGym, false},
		{"0 nunca ofrece (sin gym)", 0, nil, false},
		{"50 sin gym no ofrece — exige identificarse", 50, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inRolloutBucket(tc.rollout, tc.gymID)
			if got != tc.want {
				t.Errorf("inRolloutBucket(%d, %v) = %v, want %v", tc.rollout, tc.gymID, got, tc.want)
			}
		})
	}
}

// El bucket del gym es determinista — el mismo gym_id siempre cae en el
// mismo bucket sin importar cuántas veces le preguntes. Esto es el contrato
// que sostiene "no oscila entre versiones cuando el % cambia" del ADR §2.4.
func TestInRolloutBucket_Deterministic(t *testing.T) {
	gymID := uuid.New()
	first := hashGymBucket(gymID)
	for i := 0; i < 50; i++ {
		if got := hashGymBucket(gymID); got != first {
			t.Errorf("hashGymBucket no determinista en iter %d: got %d, want %d", i, got, first)
		}
	}
}

// Con muchos gyms distintos, la distribución por bucket cubre todo [0,100).
// No buscamos uniformidad estricta (eso es propiedad del hash), sólo que no
// haya un bug obvio que mappee todo al mismo bucket.
func TestInRolloutBucket_DistribuyeAcrossBuckets(t *testing.T) {
	seen := map[uint32]bool{}
	for i := 0; i < 5000; i++ {
		seen[hashGymBucket(uuid.New())] = true
	}
	// Esperamos al menos 50 buckets distintos cubiertos con 5k samples.
	if len(seen) < 50 {
		t.Errorf("buckets distintos vistos = %d, esperaba >= 50 (hash colapsa?)", len(seen))
	}
}

// Si rollout_percent == 50, deberíamos ver ~50% de gyms incluidos en una
// muestra grande. Toleramos ±5pp para evitar flakiness; el hash es
// determinista pero la muestra de UUIDs random tiene varianza.
func TestInRolloutBucket_50PercentRoughlySplit(t *testing.T) {
	in := 0
	const n = 5000
	for i := 0; i < n; i++ {
		gymID := uuid.New()
		if inRolloutBucket(50, &gymID) {
			in++
		}
	}
	ratio := float64(in) / float64(n)
	if ratio < 0.45 || ratio > 0.55 {
		t.Errorf("ratio incluidos = %.3f, esperaba ~0.50 (±0.05)", ratio)
	}
}

// ---------------------------------------------------------------------------
// GetLatest.Execute
// ---------------------------------------------------------------------------

func TestGetLatest_SinReleasePublicado(t *testing.T) {
	uc := NewGetLatest(&fakeReader{rel: nil}, fakeUoW{})
	out, err := uc.Execute(context.Background(), GetLatestInput{
		Target: "x86_64-pc-windows-msvc", CurrentVersion: "1.0.0", Channel: "stable",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !out.NoUpdate {
		t.Errorf("esperaba NoUpdate=true sin releases publicados, got %+v", out)
	}
}

func TestGetLatest_ClienteAlDia(t *testing.T) {
	rel := &Release{Version: "1.2.3", RolloutPercent: 100}
	uc := NewGetLatest(&fakeReader{rel: rel}, fakeUoW{})
	out, err := uc.Execute(context.Background(), GetLatestInput{
		Target: "x86_64-pc-windows-msvc", CurrentVersion: "1.2.3", Channel: "stable",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !out.NoUpdate {
		t.Errorf("cliente ya en la última versión → NoUpdate=true, got %+v", out)
	}
}

func TestGetLatest_OfreceUpdate_RolloutCompleto(t *testing.T) {
	rel := &Release{Version: "1.2.4", RolloutPercent: 100, TargetPlatform: "x86_64-pc-windows-msvc"}
	uc := NewGetLatest(&fakeReader{rel: rel}, fakeUoW{})
	out, err := uc.Execute(context.Background(), GetLatestInput{
		Target: "x86_64-pc-windows-msvc", CurrentVersion: "1.2.3", Channel: "stable",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.NoUpdate || out.Release == nil {
		t.Errorf("esperaba Release ofrecido, got %+v", out)
	}
}

func TestGetLatest_StagedRollout_GymFueraDelBucket(t *testing.T) {
	// rollout=0 garantiza que ningún gym entre, sin depender del hash.
	rel := &Release{Version: "1.2.4", RolloutPercent: 0}
	uc := NewGetLatest(&fakeReader{rel: rel}, fakeUoW{})
	gymID := uuid.New()
	out, err := uc.Execute(context.Background(), GetLatestInput{
		Target: "x86_64-pc-windows-msvc", CurrentVersion: "1.2.3", Channel: "stable", GymID: &gymID,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !out.NoUpdate {
		t.Errorf("rollout=0 → NoUpdate aunque haya gym_id, got %+v", out)
	}
}

func TestGetLatest_ReaderError_Propaga(t *testing.T) {
	boom := errors.New("db reventó")
	uc := NewGetLatest(&fakeReader{err: boom}, fakeUoW{})
	_, err := uc.Execute(context.Background(), GetLatestInput{
		Target: "x86_64-pc-windows-msvc", CurrentVersion: "1.2.3", Channel: "stable",
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, esperaba propagar reader error", err)
	}
}
