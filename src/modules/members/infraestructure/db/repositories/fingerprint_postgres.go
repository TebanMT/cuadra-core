//go:build server

package repositories

import (
	"time"

	"github.com/google/uuid"

	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	"github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type FingerprintPostgresRepository struct{}

func NewFingerprintPostgresRepository() *FingerprintPostgresRepository {
	return &FingerprintPostgresRepository{}
}

func (r *FingerprintPostgresRepository) Create(tx sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := fingerprintToModel(fp)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return fingerprintFromModel(&row), nil
}

func (r *FingerprintPostgresRepository) Update(tx sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	fp.UpdatedAt = time.Now().UTC()
	row := fingerprintToModel(fp)
	if err := gormTx.Where("id = ?", fp.ID).
		Omit("id", "created_at", "gym_id", "member_id", "registered_by").
		Save(&row).Error; err != nil {
		return nil, err
	}
	return fingerprintFromModel(&row), nil
}

func (r *FingerprintPostgresRepository) ListByMember(tx sharedDomain.Transaction, memberID uuid.UUID) ([]*fpDomain.MemberFingerprint, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.MemberFingerprintModel
	if err := gormTx.Where("member_id = ? AND deleted_at IS NULL", memberID).
		Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*fpDomain.MemberFingerprint, 0, len(rows))
	for i := range rows {
		out = append(out, fingerprintFromModel(&rows[i]))
	}
	return out, nil
}

func (r *FingerprintPostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*fpDomain.MemberFingerprint, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.MemberFingerprintModel
	if err := gormTx.Where("gym_id = ? AND deleted_at IS NULL", gymID).
		Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*fpDomain.MemberFingerprint, 0, len(rows))
	for i := range rows {
		out = append(out, fingerprintFromModel(&rows[i]))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func fingerprintToModel(f *fpDomain.MemberFingerprint) models.MemberFingerprintModel {
	return models.MemberFingerprintModel{
		ID:                f.ID,
		GymID:             f.GymID,
		Version:           f.Version,
		CreatedAt:         f.CreatedAt,
		UpdatedAt:         f.UpdatedAt,
		DeletedAt:         f.DeletedAt,
		MemberID:          f.MemberID,
		TemplateEncrypted: f.TemplateEncrypted,
		TemplateFormat:    f.TemplateFormat,
		QualityScore:      f.QualityScore,
		RegisteredBy:      f.RegisteredBy,
	}
}

func fingerprintFromModel(r *models.MemberFingerprintModel) *fpDomain.MemberFingerprint {
	return &fpDomain.MemberFingerprint{
		ID:                r.ID,
		GymID:             r.GymID,
		Version:           r.Version,
		MemberID:          r.MemberID,
		TemplateEncrypted: r.TemplateEncrypted,
		TemplateFormat:    r.TemplateFormat,
		QualityScore:      r.QualityScore,
		RegisteredBy:      r.RegisteredBy,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
		DeletedAt:         r.DeletedAt,
	}
}
