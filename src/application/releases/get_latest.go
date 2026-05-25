package releases

import (
	"context"
	"crypto/sha256"
	"encoding/binary"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// GetLatestInput es lo que el handler pasa al use case después de parsear
// la URL + headers. GymID es opcional (puntero) — vino sólo si el cliente
// mandó X-Tinta-Gym-ID. Sin gym_id, sólo se ofrece el release con
// rollout_percent == 100.
type GetLatestInput struct {
	Target         string
	CurrentVersion string
	Channel        string // 'stable' o 'beta'; default 'stable' lo aplica el handler
	GymID          *uuid.UUID
}

// GetLatestOutput es lo que devuelve el use case. NoUpdate=true significa
// 204 al cliente — sea porque no hay release publicado para ese target, el
// gym cayó fuera del bucket de rollout, o el cliente ya está en la última.
// El handler traduce el shape al manifest de Tauri.
type GetLatestOutput struct {
	NoUpdate bool
	Release  *Release
}

// GetLatest compone (1) lookup del último release publicado para (target,
// channel), (2) skip si current_version == release.version (cliente al día),
// (3) decisión de staged rollout determinista basada en hash(gym_id).
type GetLatest struct {
	reader Reader
	uow    sharedDomain.UnitOfWork
}

func NewGetLatest(reader Reader, uow sharedDomain.UnitOfWork) *GetLatest {
	return &GetLatest{reader: reader, uow: uow}
}

func (u *GetLatest) Execute(ctx context.Context, in GetLatestInput) (*GetLatestOutput, error) {
	tx, err := u.uow.Query(ctx)
	if err != nil {
		return nil, err
	}
	rel, err := u.reader.LatestForTarget(tx, in.Target, in.Channel)
	if err != nil {
		return nil, err
	}
	if rel == nil {
		return &GetLatestOutput{NoUpdate: true}, nil
	}
	if rel.Version == in.CurrentVersion {
		// Cliente ya está al día. 204.
		return &GetLatestOutput{NoUpdate: true}, nil
	}
	if !inRolloutBucket(rel.RolloutPercent, in.GymID) {
		return &GetLatestOutput{NoUpdate: true}, nil
	}
	return &GetLatestOutput{NoUpdate: false, Release: rel}, nil
}

// inRolloutBucket decide si este gym recibe el release dado un
// rollout_percent (ADR-005 §2.4).
//
//   - rollout == 100 → siempre se ofrece (release general).
//   - rollout == 0   → nunca se ofrece (release pausado).
//   - 0 < rollout < 100:
//   - con gym_id: hash(gym_id) % 100 < rollout. Determinista — el
//     mismo gym siempre cae en el mismo bucket, no oscila entre
//     versiones cuando el % cambia.
//   - sin gym_id: NO se ofrece. El cliente debe identificarse vía
//     X-Tinta-Gym-ID para entrar al staged. Esto evita que un
//     attacker anónimo se "auto-eleve" al rollout temprano.
func inRolloutBucket(rolloutPercent int, gymID *uuid.UUID) bool {
	if rolloutPercent >= 100 {
		return true
	}
	if rolloutPercent <= 0 {
		return false
	}
	if gymID == nil {
		return false
	}
	return hashGymBucket(*gymID) < uint32(rolloutPercent)
}

// hashGymBucket proyecta gym_id a un bucket [0,100). SHA-256 es overkill
// para distribución pero es lo que ya tenemos en `crypto/sha256` y nos da
// uniformidad sin esfuerzo. Los primeros 4 bytes alcanzan.
func hashGymBucket(gymID uuid.UUID) uint32 {
	sum := sha256.Sum256(gymID[:])
	return binary.BigEndian.Uint32(sum[:4]) % 100
}
