//go:build server

package repositories

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
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

func (r *MemberPostgresRepository) GetNamesByIDs(tx sharedDomain.Transaction, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []struct {
		ID       uuid.UUID `gorm:"column:id"`
		FullName string    `gorm:"column:full_name"`
	}
	err := gormTx.Model(&models.MemberModel{}).
		Select("id", "full_name").
		Where("id IN ? AND deleted_at IS NULL", ids).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r.FullName
	}
	return out, nil
}

func (r *MemberPostgresRepository) GetContactsByIDs(tx sharedDomain.Transaction, gymID uuid.UUID, ids []uuid.UUID) ([]memRepo.MemberContact, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []struct {
		ID       uuid.UUID `gorm:"column:id"`
		FullName string    `gorm:"column:full_name"`
		Phone    string    `gorm:"column:phone"`
	}
	// gym_id en el WHERE = cerrojo anti cross-tenant. deleted_at IS NULL omite
	// socios borrados entre que el FE arma la selección y se confirma.
	err := gormTx.Model(&models.MemberModel{}).
		Select("id", "full_name", "phone").
		Where("gym_id = ? AND id IN ? AND deleted_at IS NULL", gymID, ids).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]memRepo.MemberContact, 0, len(rows))
	for _, row := range rows {
		out = append(out, memRepo.MemberContact{ID: row.ID, FullName: row.FullName, Phone: row.Phone})
	}
	return out, nil
}

func (r *MemberPostgresRepository) ExistsByGymAndPhone(tx sharedDomain.Transaction, gymID uuid.UUID, phone string) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.MemberModel{}).
		Where("gym_id = ? AND phone = ? AND deleted_at IS NULL", gymID, phone).
		Count(&n).Error
	return n > 0, err
}

// MemberNumberExistsInGym is an O(1) lookup on `uq_members_gym_number`
// (ADR-010). Returns true when another non-deleted member in `gymID` already
// holds `number`; excludeMemberID lets a member re-claim its own number.
func (r *MemberPostgresRepository) MemberNumberExistsInGym(tx sharedDomain.Transaction, gymID uuid.UUID, number int, excludeMemberID *uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	q := gormTx.Model(&models.MemberModel{}).
		Where("gym_id = ? AND member_number = ? AND deleted_at IS NULL", gymID, number)
	if excludeMemberID != nil {
		q = q.Where("id <> ?", *excludeMemberID)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListUsedMemberNumbers returns every member_number in use in `gymID`
// (non-deleted, non-null). Backs the asignador's bump + gap-fill.
func (r *MemberPostgresRepository) ListUsedMemberNumbers(tx sharedDomain.Transaction, gymID uuid.UUID) ([]int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var nums []int
	err := gormTx.Model(&models.MemberModel{}).
		Where("gym_id = ? AND member_number IS NOT NULL AND deleted_at IS NULL", gymID).
		Pluck("member_number", &nums).Error
	if err != nil {
		return nil, err
	}
	return nums, nil
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
			AND ms.status IN ('active','pending_payment')
			AND ms.deleted_at IS NULL`).
		Where("m.gym_id = ? AND m.deleted_at IS NULL", q.GymID)

	if s := strings.TrimSpace(q.Search); s != "" {
		// Nombre: prefix match. Teléfono: substring match sobre la
		// versión solo-dígitos del término (los teléfonos en BD están
		// normalizados sin espacios ni guiones). Antes era sufijo y los
		// primeros dígitos no matcheaban nada.
		like := strings.ToLower(s) + "%"
		phoneDigits := stripPhoneSeparators(s)
		if phoneDigits != "" {
			base = base.Where("LOWER(m.full_name) LIKE ? OR m.phone LIKE ?", like, "%"+phoneDigits+"%")
		} else {
			base = base.Where("LOWER(m.full_name) LIKE ?", like)
		}
	}
	switch q.StatusFilter {
	case "active":
		// Vigente = expiry >= hoy; INCLUYE "por vencer" (subconjunto), igual
		// que CountActiveMembers. expiring_soon ya no es excluyente.
		base = base.Where("m.status = ? AND ms.status = 'active' AND ms.expiry_date IS NOT NULL AND ms.expiry_date >= ?::date", memberDomain.StatusActive, today)
	case "expiring_soon":
		base = base.Where("m.status = ? AND ms.status = 'active' AND ms.expiry_date IS NOT NULL AND ms.expiry_date - ?::date BETWEEN 0 AND 7", memberDomain.StatusActive, today)
	case "expired":
		// "Por cobrar": vencidos clásicos + pending_payment + sin membership.
		base = base.Where("m.status = ? AND (ms.id IS NULL OR ms.status = 'pending_payment' OR (ms.status = 'active' AND ms.expiry_date < ?::date))", memberDomain.StatusActive, today)
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
				ExpiryDate:           rows[i].MS_ExpiryDate,
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
	err = gormTx.Where("member_id = ? AND status IN (?, ?) AND deleted_at IS NULL",
		memberID, membershipDomain.StatusActive, membershipDomain.StatusPendingPayment).First(&ms).Error
	if err == nil {
		out.CurrentMembership = membershipFromModel(&ms)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return out, nil
}

// FindByMemberNumber does the O(1) check-in lookup on `uq_members_gym_number`
// (ADR-010). Returns (nil, nil) when no non-deleted member holds the number.
// NO filtramos por status: si un socio inactivo ingresa su número, queremos
// que el access evaluator devuelva denied_inactive con el nombre del socio
// en vez de "no encontré a este socio" (que sugiere número incorrecto, no
// socio bloqueado).
func (r *MemberPostgresRepository) FindByMemberNumber(tx sharedDomain.Transaction, gymID uuid.UUID, number int) (*memberDomain.Member, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.MemberModel
	err := gormTx.Where("gym_id = ? AND member_number = ? AND deleted_at IS NULL", gymID, number).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return memberFromModel(&row), nil
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
		MemberNumber:         m.MemberNumber,
		LastContactAttemptAt: m.LastContactAttemptAt,
		Gender:               m.Gender,
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
		MemberNumber:         r.MemberNumber,
		LastContactAttemptAt: r.LastContactAttemptAt,
		Gender:               r.Gender,
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
