//go:build server

package repositories

import (
	"time"

	"github.com/google/uuid"

	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
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
	// Exclude fingerprints whose member is inactive or lost — those socios should
	// not be recognized at the door. A member with an *expired* membership but
	// status='active' is still included so the kiosk can surface "renueva tu
	// membresía" instead of silently rejecting them.
	if err := gormTx.
		Joins("JOIN members ON members.id = member_fingerprints.member_id AND members.deleted_at IS NULL AND members.status = ?", memberDomain.StatusActive).
		Where("member_fingerprints.gym_id = ? AND member_fingerprints.deleted_at IS NULL", gymID).
		Order("member_fingerprints.created_at ASC").Find(&rows).Error; err != nil {
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
