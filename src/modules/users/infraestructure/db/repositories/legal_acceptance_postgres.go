//go:build server

package repositories

import (
	"github.com/cuadra/cuadra-core/src/modules/users/domain/legal"
	"github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type LegalAcceptancePostgresRepository struct{}

func NewLegalAcceptancePostgresRepository() *LegalAcceptancePostgresRepository {
	return &LegalAcceptancePostgresRepository{}
}

func (r *LegalAcceptancePostgresRepository) Record(tx sharedDomain.Transaction, acceptances []legal.Acceptance) error {
	if len(acceptances) == 0 {
		return nil
	}
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	rows := make([]models.LegalAcceptanceModel, 0, len(acceptances))
	for _, a := range acceptances {
		rows = append(rows, toLegalAcceptanceModel(a))
	}
	return gormTx.Create(&rows).Error
}

func toLegalAcceptanceModel(a legal.Acceptance) models.LegalAcceptanceModel {
	return models.LegalAcceptanceModel{
		ID:         a.ID,
		GymID:      a.GymID,
		UserID:     a.UserID,
		Document:   a.Document,
		Version:    a.Version,
		AcceptedAt: a.AcceptedAt.UTC(),
		IPAddress:  optionalText(a.IPAddress),
		UserAgent:  optionalText(a.UserAgent),
	}
}

func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
