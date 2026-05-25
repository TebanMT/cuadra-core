package releases

import (
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Release es la fila de la tabla `releases` proyectada al dominio. Coincide
// con el shape del manifest de Tauri (más metadata interna: rollout_percent,
// force_immediate, channel) — el wire DTO se arma en el handler.
type Release struct {
	ID               uuid.UUID
	Version          string
	TargetPlatform   string
	Channel          string
	URL              string
	SignatureEd25519 string
	PubDate          time.Time
	Notes            string
	RolloutPercent   int
	ForceImmediate   bool
}

// Reader lee releases publicados. El use case GetLatest pide la última fila
// (pub_date DESC) para un target+channel y luego aplica la lógica de rollout.
// Se mantiene como interfaz para que los tests puedan inyectar un fake sin
// montar Postgres.
type Reader interface {
	// LatestForTarget devuelve el release más reciente NO soft-deleted para
	// (target, channel). Retorna (nil, nil) si no hay ninguno.
	LatestForTarget(tx sharedDomain.Transaction, target, channel string) (*Release, error)
}

// Writer es la mutación admin que llama el pipeline al publicar. Insertar
// es la única operación — para "retirar" un release se hace UPDATE
// rollout_percent=0 (soft pause) o un soft-delete (caso raro de release
// roto). Ambos se manejan vía SQL manual desde docs/release.md, no via API.
type Writer interface {
	Insert(tx sharedDomain.Transaction, r Release) error
}
