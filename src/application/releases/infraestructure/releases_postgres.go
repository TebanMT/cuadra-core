//go:build server

// Package infraestructure aloja la implementación Postgres del read+write
// surface del módulo releases. Vive aparte del paquete `releases` para no
// arrastrar dependencias de GORM al core de application/.
package infraestructure

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/cuadra/cuadra-core/src/application/releases"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// PostgresStore implementa releases.Reader y releases.Writer contra la tabla
// `releases` (migración 026).
type PostgresStore struct{}

func NewPostgresStore() *PostgresStore { return &PostgresStore{} }

// releaseRow es el row interno — no se exporta. El use case habla en
// releases.Release.
type releaseRow struct {
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

func (r *PostgresStore) LatestForTarget(tx sharedDomain.Transaction, target, channel string) (*releases.Release, error) {
	gormTx, ok := tx.(*sharedDomain.GormTransaction)
	if !ok {
		return nil, errors.New("releases.PostgresStore: transaction is not GORM-backed")
	}
	var row releaseRow
	err := gormTx.Tx.Raw(`
		SELECT id, version, target_platform, channel, url, signature_ed25519,
		       pub_date, notes, rollout_percent, force_immediate
		FROM releases
		WHERE target_platform = ?
		  AND channel = ?
		  AND deleted_at IS NULL
		ORDER BY pub_date DESC
		LIMIT 1`, target, channel).Scan(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if row.ID == uuid.Nil {
		return nil, nil
	}
	return &releases.Release{
		ID:               row.ID,
		Version:          row.Version,
		TargetPlatform:   row.TargetPlatform,
		Channel:          row.Channel,
		URL:              row.URL,
		SignatureEd25519: row.SignatureEd25519,
		PubDate:          row.PubDate,
		Notes:            row.Notes,
		RolloutPercent:   row.RolloutPercent,
		ForceImmediate:   row.ForceImmediate,
	}, nil
}

func (r *PostgresStore) Insert(tx sharedDomain.Transaction, rel releases.Release) error {
	gormTx, ok := tx.(*sharedDomain.GormTransaction)
	if !ok {
		return errors.New("releases.PostgresStore: transaction is not GORM-backed")
	}
	err := gormTx.Tx.Exec(`
		INSERT INTO releases (
		    id, version, target_platform, channel, url,
		    signature_ed25519, pub_date, notes, rollout_percent, force_immediate
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rel.ID, rel.Version, rel.TargetPlatform, rel.Channel, rel.URL,
		rel.SignatureEd25519, rel.PubDate, rel.Notes, rel.RolloutPercent, rel.ForceImmediate,
	).Error
	if err != nil {
		// El UNIQUE (version, target_platform, channel) de la migración 026
		// salta cuando el pipeline re-postea el mismo release (re-run de un tag).
		// Lo convertimos en un error tipado para que el handler responda 409 en
		// vez de 500 — el release ya está registrado, es idempotente.
		if isUniqueViolation(err) {
			return releases.ErrAlreadyExists
		}
		return err
	}
	return nil
}

// isUniqueViolation testea tanto el SQLSTATE de Postgres como el wrapping
// genérico de GORM. Evitamos una dependencia dura de github.com/lib/pq
// matcheando el mensaje — alcanza para el único path de unicidad de releases.
// (Mismo patrón que subscriptions/infraestructure/db/event_postgres.go.)
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key value violates unique constraint") ||
		strings.Contains(msg, "UNIQUE constraint failed")
}
