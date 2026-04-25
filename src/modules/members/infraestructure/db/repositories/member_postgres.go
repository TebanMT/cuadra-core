//go:build server

package repositories

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type MemberPostgresRepository struct{}

func NewMemberPostgresRepository() *MemberPostgresRepository { return &MemberPostgresRepository{} }

func (r *MemberPostgresRepository) Create(tx sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := memberToModel(m)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return memberFromModel(&row), nil
}

func (r *MemberPostgresRepository) Update(tx sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m.UpdatedAt = time.Now().UTC()
	row := memberToModel(m)
	if err := gormTx.Where("id = ?", m.ID).Omit("id", "created_at", "gym_id", "folio", "created_by").Save(&row).Error; err != nil {
		return nil, err
	}
	return memberFromModel(&row), nil
}

func (r *MemberPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*memberDomain.Member, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.MemberModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMemberNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return memberFromModel(&row), nil
}

func (r *MemberPostgresRepository) ExistsByGymAndPhone(tx sharedDomain.Transaction, gymID uuid.UUID, phone string) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.MemberModel{}).
		Where("gym_id = ? AND phone = ? AND deleted_at IS NULL", gymID, phone).
		Count(&n).Error
	return n > 0, err
}

// PinHashCollidesInGym iterates over members in `gymID` with a non-null
// pin_hash and runs bcrypt.Verify against each — bcrypt salts force this.
// In a 1k-member gym the cost (~1ms per Verify) is acceptable for an
// admin-side action; if it ever isn't, we'll cache an HMAC peppered hash.
func (r *MemberPostgresRepository) PinHashCollidesInGym(tx sharedDomain.Transaction, gymID uuid.UUID, plainPin string, excludeMemberID *uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	q := gormTx.Model(&models.MemberModel{}).
		Where("gym_id = ? AND pin_hash IS NOT NULL AND deleted_at IS NULL", gymID)
	if excludeMemberID != nil {
		q = q.Where("id <> ?", *excludeMemberID)
	}
	var rows []struct {
		ID      uuid.UUID
		PinHash string
	}
	if err := q.Select("id, pin_hash").Find(&rows).Error; err != nil {
		return false, err
	}
	for _, h := range rows {
		if bcrypt.CompareHashAndPassword([]byte(h.PinHash), []byte(plainPin)) == nil {
			return true, nil
		}
	}
	return false, nil
}

// NextFolio uses SELECT ... FOR UPDATE on the matching gym scope to serialise
// folio assignment. Format: "MEM-NNNNNN".
func (r *MemberPostgresRepository) NextFolio(tx sharedDomain.Transaction, gymID uuid.UUID) (string, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var maxFolio struct {
		Max *string
	}
	err := gormTx.Model(&models.MemberModel{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("MAX(folio) AS max").
		Where("gym_id = ?", gymID).
		Scan(&maxFolio).Error
	if err != nil {
		return "", err
	}
	next := 1
	if maxFolio.Max != nil {
		s := strings.TrimPrefix(*maxFolio.Max, "MEM-")
		var n int
		_, _ = fmt.Sscanf(s, "%d", &n)
		next = n + 1
	}
	return fmt.Sprintf("MEM-%06d", next), nil
}

// List backs UC-014.
func (r *MemberPostgresRepository) List(tx sharedDomain.Transaction, q memRepo.ListQuery) ([]*memRepo.MemberWithMembership, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	page := q.Page
	if page < 1 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 25
	}
	today := truncateDate(q.Today)

	base := gormTx.Table("members AS m").
		Joins(`LEFT JOIN memberships AS ms
			ON ms.member_id = m.id
			AND ms.status = 'active'
			AND ms.deleted_at IS NULL`).
		Where("m.gym_id = ? AND m.deleted_at IS NULL", q.GymID)

	if s := strings.TrimSpace(q.Search); s != "" {
		like := strings.ToLower(s) + "%"
		phoneLike := "%" + s
		base = base.Where("LOWER(m.full_name) LIKE ? OR m.phone LIKE ?", like, phoneLike)
	}
	switch q.StatusFilter {
	case "active":
		base = base.Where("m.status = ? AND ms.id IS NOT NULL AND ms.expiry_date - ?::date > 7", memberDomain.StatusActive, today)
	case "expiring_soon":
		base = base.Where("m.status = ? AND ms.id IS NOT NULL AND ms.expiry_date - ?::date BETWEEN 0 AND 7", memberDomain.StatusActive, today)
	case "expired":
		base = base.Where("m.status = ? AND (ms.id IS NULL OR ms.expiry_date < ?::date)", memberDomain.StatusActive, today)
	case "inactive":
		base = base.Where("m.status <> ?", memberDomain.StatusActive)
	}
	if q.PlanID != nil {
		base = base.Where("ms.membership_type_id = ?", *q.PlanID)
	}

	var total int64
	if err := base.Session(&gorm.Session{}).Distinct("m.id").Count(&total).Error; err != nil {
		return nil, 0, err
	}

	order := "m.full_name ASC"
	asc := q.SortAscending
	switch q.Sort {
	case "name", "":
		if asc {
			order = "LOWER(m.full_name) ASC"
		} else {
			order = "LOWER(m.full_name) DESC"
		}
	case "expiry":
		if asc {
			order = "ms.expiry_date ASC NULLS LAST"
		} else {
			order = "ms.expiry_date DESC NULLS LAST"
		}
	case "created_at":
		if asc {
			order = "m.created_at ASC"
		} else {
			order = "m.created_at DESC"
		}
	}

	type row struct {
		models.MemberModel
		MS_ID               *uuid.UUID `gorm:"column:ms_id"`
		MS_GymID            *uuid.UUID `gorm:"column:ms_gym_id"`
		MS_Version          *int       `gorm:"column:ms_version"`
		MS_CreatedAt        *time.Time `gorm:"column:ms_created_at"`
		MS_UpdatedAt        *time.Time `gorm:"column:ms_updated_at"`
		MS_DeletedAt        *time.Time `gorm:"column:ms_deleted_at"`
		MS_MemberID         *uuid.UUID `gorm:"column:ms_member_id"`
		MS_MembershipTypeID *uuid.UUID `gorm:"column:ms_membership_type_id"`
		MS_TypeNameSnap     *string    `gorm:"column:ms_type_name_snapshot"`
		MS_PriceSnap        *float64   `gorm:"column:ms_price_snapshot"`
		MS_DurationSnap     *int       `gorm:"column:ms_duration_days_snapshot"`
		MS_StartDate        *time.Time `gorm:"column:ms_start_date"`
		MS_ExpiryDate       *time.Time `gorm:"column:ms_expiry_date"`
		MS_Status           *string    `gorm:"column:ms_status"`
		MS_ReplacedBy       *uuid.UUID `gorm:"column:ms_replaced_by"`
	}
	var rows []row
	err := base.
		Select(`m.*,
			ms.id AS ms_id, ms.gym_id AS ms_gym_id, ms.version AS ms_version,
			ms.created_at AS ms_created_at, ms.updated_at AS ms_updated_at, ms.deleted_at AS ms_deleted_at,
			ms.member_id AS ms_member_id, ms.membership_type_id AS ms_membership_type_id,
			ms.type_name_snapshot AS ms_type_name_snapshot, ms.price_snapshot AS ms_price_snapshot,
			ms.duration_days_snapshot AS ms_duration_days_snapshot,
			ms.start_date AS ms_start_date, ms.expiry_date AS ms_expiry_date,
			ms.status AS ms_status, ms.replaced_by AS ms_replaced_by`).
		Order(order).
		Limit(pageSize).Offset((page - 1) * pageSize).
		Scan(&rows).Error
	if err != nil {
		return nil, 0, err
	}

	out := make([]*memRepo.MemberWithMembership, 0, len(rows))
	for i := range rows {
		mw := &memRepo.MemberWithMembership{Member: memberFromModel(&rows[i].MemberModel)}
		if rows[i].MS_ID != nil {
			ms := &models.MembershipModel{
				ID:                   *rows[i].MS_ID,
				GymID:                derefUUID(rows[i].MS_GymID),
				Version:              derefInt(rows[i].MS_Version),
				CreatedAt:            derefTime(rows[i].MS_CreatedAt),
				UpdatedAt:            derefTime(rows[i].MS_UpdatedAt),
				DeletedAt:            rows[i].MS_DeletedAt,
				MemberID:             derefUUID(rows[i].MS_MemberID),
				MembershipTypeID:     derefUUID(rows[i].MS_MembershipTypeID),
				TypeNameSnapshot:     derefString(rows[i].MS_TypeNameSnap),
				PriceSnapshot:        derefFloat(rows[i].MS_PriceSnap),
				DurationDaysSnapshot: derefInt(rows[i].MS_DurationSnap),
				StartDate:            derefTime(rows[i].MS_StartDate),
				ExpiryDate:           derefTime(rows[i].MS_ExpiryDate),
				Status:               derefString(rows[i].MS_Status),
				ReplacedBy:           rows[i].MS_ReplacedBy,
			}
			mw.CurrentMembership = membershipFromModel(ms)
		}
		out = append(out, mw)
	}
	return out, int(total), nil
}

func (r *MemberPostgresRepository) GetWithCurrentMembership(tx sharedDomain.Transaction, gymID, memberID uuid.UUID) (*memRepo.MemberWithMembership, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var mm models.MemberModel
	err := gormTx.Where("id = ? AND gym_id = ? AND deleted_at IS NULL", memberID, gymID).First(&mm).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMemberNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	out := &memRepo.MemberWithMembership{Member: memberFromModel(&mm)}
	var ms models.MembershipModel
	err = gormTx.Where("member_id = ? AND status = ? AND deleted_at IS NULL", memberID, membershipDomain.StatusActive).First(&ms).Error
	if err == nil {
		out.CurrentMembership = membershipFromModel(&ms)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func memberToModel(m *memberDomain.Member) models.MemberModel {
	return models.MemberModel{
		ID:                   m.ID,
		GymID:                m.GymID,
		Version:              m.Version,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
		DeletedAt:            m.DeletedAt,
		Folio:                m.Folio,
		FullName:             m.FullName,
		Phone:                m.Phone,
		Email:                m.Email,
		Birthdate:            m.Birthdate,
		PhotoURL:             m.PhotoURL,
		Notes:                m.Notes,
		Status:               m.Status,
		EnrollmentPaid:       m.EnrollmentPaid,
		LastMaintenancePaid:  m.LastMaintenancePaid,
		PinHash:              m.PinHash,
		PinAssignedAt:        m.PinAssignedAt,
		LastContactAttemptAt: m.LastContactAttemptAt,
		CreatedBy:            m.CreatedBy,
	}
}

func memberFromModel(r *models.MemberModel) *memberDomain.Member {
	return &memberDomain.Member{
		ID:                   r.ID,
		GymID:                r.GymID,
		Version:              r.Version,
		Folio:                r.Folio,
		FullName:             r.FullName,
		Phone:                r.Phone,
		Email:                r.Email,
		Birthdate:            r.Birthdate,
		PhotoURL:             r.PhotoURL,
		Notes:                r.Notes,
		Status:               r.Status,
		EnrollmentPaid:       r.EnrollmentPaid,
		LastMaintenancePaid:  r.LastMaintenancePaid,
		PinHash:              r.PinHash,
		PinAssignedAt:        r.PinAssignedAt,
		LastContactAttemptAt: r.LastContactAttemptAt,
		CreatedBy:            r.CreatedBy,
		CreatedAt:            r.CreatedAt,
		UpdatedAt:            r.UpdatedAt,
		DeletedAt:            r.DeletedAt,
	}
}

func truncateDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func derefUUID(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func derefTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}
