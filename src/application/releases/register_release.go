package releases

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RegisterReleaseInput es lo que recibe el endpoint admin desde el pipeline
// de GitHub Actions al subir un manifest nuevo. version + target_platform +
// channel forman la unique key — re-postear el mismo set falla con UNIQUE
// constraint (intencional: hace al pipeline idempotente sólo por re-tag).
//
// rollout_percent default = 5 (ADR-005 §2.4 día 0). El pipeline puede
// override-ar al subir si es hotfix urgente (100 directo).
type RegisterReleaseInput struct {
	Version          string
	TargetPlatform   string
	Channel          string
	URL              string
	SignatureEd25519 string
	Notes            string
	RolloutPercent   int
	ForceImmediate   bool
	PubDate          time.Time
}

var (
	ErrInvalidVersion   = errors.New("version requerido")
	ErrInvalidTarget    = errors.New("target_platform requerido")
	ErrInvalidURL       = errors.New("url requerido")
	ErrInvalidSignature = errors.New("signature_ed25519 requerido")
	ErrInvalidRollout   = errors.New("rollout_percent fuera de [0,100]")
	ErrInvalidChannel   = errors.New("channel debe ser 'stable' o 'beta'")
)

type RegisterRelease struct {
	writer Writer
	uow    sharedDomain.UnitOfWork
}

func NewRegisterRelease(writer Writer, uow sharedDomain.UnitOfWork) *RegisterRelease {
	return &RegisterRelease{writer: writer, uow: uow}
}

func (u *RegisterRelease) Execute(ctx context.Context, in RegisterReleaseInput) (*Release, error) {
	if in.Version == "" {
		return nil, ErrInvalidVersion
	}
	if in.TargetPlatform == "" {
		return nil, ErrInvalidTarget
	}
	if in.URL == "" {
		return nil, ErrInvalidURL
	}
	if in.SignatureEd25519 == "" {
		return nil, ErrInvalidSignature
	}
	if in.RolloutPercent < 0 || in.RolloutPercent > 100 {
		return nil, ErrInvalidRollout
	}
	channel := in.Channel
	if channel == "" {
		channel = "stable"
	}
	if channel != "stable" && channel != "beta" {
		return nil, ErrInvalidChannel
	}
	if in.PubDate.IsZero() {
		in.PubDate = time.Now().UTC()
	}

	rel := Release{
		ID:               uuid.New(),
		Version:          in.Version,
		TargetPlatform:   in.TargetPlatform,
		Channel:          channel,
		URL:              in.URL,
		SignatureEd25519: in.SignatureEd25519,
		PubDate:          in.PubDate,
		Notes:            in.Notes,
		RolloutPercent:   in.RolloutPercent,
		ForceImmediate:   in.ForceImmediate,
	}
	if err := u.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		return u.writer.Insert(tx, rel)
	}); err != nil {
		return nil, err
	}
	return &rel, nil
}
