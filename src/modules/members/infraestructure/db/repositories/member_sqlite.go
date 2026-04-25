//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

const dateLayout = "2006-01-02"

type MemberSQLiteRepository struct{}

func NewMemberSQLiteRepository() *MemberSQLiteRepository { return &MemberSQLiteRepository{} }

type sqliteMemberRow struct {
	ID                   string         `db:"id"`
	GymID                string         `db:"gym_id"`
	Version              int            `db:"version"`
	CreatedAt            int64          `db:"created_at"`
	UpdatedAt            int64          `db:"updated_at"`
	DeletedAt            sql.NullInt64  `db:"deleted_at"`
	SyncedAt             sql.NullInt64  `db:"synced_at"`
	Folio                string         `db:"folio"`
	FullName             string         `db:"full_name"`
	Phone                string         `db:"phone"`
	Email                sql.NullString `db:"email"`
	Birthdate            sql.NullString `db:"birthdate"`
	PhotoURL             sql.NullString `db:"photo_url"`
	Notes                sql.NullString `db:"notes"`
	Status               string         `db:"status"`
	EnrollmentPaid       int            `db:"enrollment_paid"`
	LastMaintenancePaid  sql.NullString `db:"last_maintenance_paid"`
	PinHash              sql.NullString `db:"pin_hash"`
	PinAssignedAt        sql.NullInt64  `db:"pin_assigned_at"`
	LastContactAttemptAt sql.NullInt64  `db:"last_contact_attempt_at"`
	CreatedBy            string         `db:"created_by"`
}

// joinedMemberRow is the read-shape used by List() — Member + LEFT JOIN current
// Membership. The MS_* fields are nullable because there may be no active row.
type joinedMemberRow struct {
	sqliteMemberRow
	MS_ID               sql.NullString `db:"ms_id"`
	MS_GymID            sql.NullString `db:"ms_gym_id"`
	MS_Version          sql.NullInt64  `db:"ms_version"`
	MS_CreatedAt        sql.NullInt64  `db:"ms_created_at"`
	MS_UpdatedAt        sql.NullInt64  `db:"ms_updated_at"`
	MS_DeletedAt        sql.NullInt64  `db:"ms_deleted_at"`
	MS_MemberID         sql.NullString `db:"ms_member_id"`
	MS_MembershipTypeID sql.NullString `db:"ms_membership_type_id"`
	MS_TypeNameSnap     sql.NullString `db:"ms_type_name_snapshot"`
	MS_PriceSnap        sql.NullInt64  `db:"ms_price_snapshot"`
	MS_DurationSnap     sql.NullInt64  `db:"ms_duration_days_snapshot"`
	MS_StartDate        sql.NullString `db:"ms_start_date"`
	MS_ExpiryDate       sql.NullString `db:"ms_expiry_date"`
	MS_Status           sql.NullString `db:"ms_status"`
	MS_ReplacedBy       sql.NullString `db:"ms_replaced_by"`
}

func (r *MemberSQLiteRepository) Create(tx sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := memberToRow(m)
	const stmt = `
		INSERT INTO members (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    folio, full_name, phone, email, birthdate, photo_url, notes, status,
		    enrollment_paid, last_maintenance_paid,
		    pin_hash, pin_assigned_at, last_contact_attempt_at, created_by
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :folio, :full_name, :phone, :email, :birthdate, :photo_url, :notes, :status,
		    :enrollment_paid, :last_maintenance_paid,
		    :pin_hash, :pin_assigned_at, :last_contact_attempt_at, :created_by
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueMember(stx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *MemberSQLiteRepository) Update(tx sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	m.UpdatedAt = time.Now().UTC()
	row := memberToRow(m)
	const stmt = `
		UPDATE members SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    full_name = :full_name, phone = :phone, email = :email, birthdate = :birthdate,
		    photo_url = :photo_url, notes = :notes, status = :status,
		    enrollment_paid = :enrollment_paid, last_maintenance_paid = :last_maintenance_paid,
		    pin_hash = :pin_hash, pin_assigned_at = :pin_assigned_at,
		    last_contact_attempt_at = :last_contact_attempt_at
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueMember(stx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *MemberSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*memberDomain.Member, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteMemberRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM members WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMemberNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return memberFromRow(&row), nil
}

func (r *MemberSQLiteRepository) ExistsByGymAndPhone(tx sharedDomain.Transaction, gymID uuid.UUID, phone string) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM members WHERE gym_id = ? AND phone = ? AND deleted_at IS NULL`,
		gymID.String(), phone)
	return n > 0, err
}

func (r *MemberSQLiteRepository) PinHashCollidesInGym(tx sharedDomain.Transaction, gymID uuid.UUID, plainPin string, excludeMemberID *uuid.UUID) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	q := `SELECT id, pin_hash FROM members WHERE gym_id = ? AND pin_hash IS NOT NULL AND deleted_at IS NULL`
	args := []any{gymID.String()}
	if excludeMemberID != nil {
		q += ` AND id <> ?`
		args = append(args, excludeMemberID.String())
	}
	type pinRow struct {
		ID      string `db:"id"`
		PinHash string `db:"pin_hash"`
	}
	var rows []pinRow
	if err := stx.Select(context.Background(), &rows, q, args...); err != nil {
		return false, err
	}
	for _, row := range rows {
		if bcrypt.CompareHashAndPassword([]byte(row.PinHash), []byte(plainPin)) == nil {
			return true, nil
		}
	}
	return false, nil
}

func (r *MemberSQLiteRepository) NextFolio(tx sharedDomain.Transaction, gymID uuid.UUID) (string, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var maxFolio sql.NullString
	err := stx.Get(context.Background(), &maxFolio,
		`SELECT MAX(folio) FROM members WHERE gym_id = ?`, gymID.String())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	next := 1
	if maxFolio.Valid {
		s := strings.TrimPrefix(maxFolio.String, "MEM-")
		var n int
		_, _ = fmt.Sscanf(s, "%d", &n)
		next = n + 1
	}
	return fmt.Sprintf("MEM-%06d", next), nil
}

func (r *MemberSQLiteRepository) List(tx sharedDomain.Transaction, q memRepo.ListQuery) ([]*memRepo.MemberWithMembership, int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	page := q.Page
	if page < 1 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 25
	}
	today := q.Today.UTC().Format(dateLayout)

	where := []string{`m.gym_id = ? AND m.deleted_at IS NULL`}
	args := []any{q.GymID.String()}
	if s := strings.TrimSpace(q.Search); s != "" {
		where = append(where, `(m.full_name LIKE ? COLLATE NOCASE OR m.phone LIKE ?)`)
		args = append(args, s+"%", "%"+s)
	}
	switch q.StatusFilter {
	case "active":
		where = append(where, `m.status = 'active' AND ms.id IS NOT NULL AND CAST(julianday(ms.expiry_date) - julianday(?) AS INTEGER) > 7`)
		args = append(args, today)
	case "expiring_soon":
		where = append(where, `m.status = 'active' AND ms.id IS NOT NULL AND CAST(julianday(ms.expiry_date) - julianday(?) AS INTEGER) BETWEEN 0 AND 7`)
		args = append(args, today)
	case "expired":
		where = append(where, `m.status = 'active' AND (ms.id IS NULL OR ms.expiry_date < ?)`)
		args = append(args, today)
	case "inactive":
		where = append(where, `m.status <> 'active'`)
	}
	if q.PlanID != nil {
		where = append(where, `ms.membership_type_id = ?`)
		args = append(args, q.PlanID.String())
	}

	whereClause := strings.Join(where, " AND ")
	join := `LEFT JOIN memberships AS ms
		ON ms.member_id = m.id AND ms.status = 'active' AND ms.deleted_at IS NULL`

	countQ := fmt.Sprintf(`SELECT COUNT(DISTINCT m.id) FROM members AS m %s WHERE %s`, join, whereClause)
	var total int
	if err := stx.Get(context.Background(), &total, countQ, args...); err != nil {
		return nil, 0, err
	}

	order := "m.full_name COLLATE NOCASE ASC"
	asc := q.SortAscending
	switch q.Sort {
	case "name", "":
		if asc {
			order = "m.full_name COLLATE NOCASE ASC"
		} else {
			order = "m.full_name COLLATE NOCASE DESC"
		}
	case "expiry":
		if asc {
			order = "ms.expiry_date ASC"
		} else {
			order = "ms.expiry_date DESC"
		}
	case "created_at":
		if asc {
			order = "m.created_at ASC"
		} else {
			order = "m.created_at DESC"
		}
	}

	listQ := fmt.Sprintf(`
		SELECT
		    m.id, m.gym_id, m.version, m.created_at, m.updated_at, m.deleted_at, m.synced_at,
		    m.folio, m.full_name, m.phone, m.email, m.birthdate, m.photo_url, m.notes, m.status,
		    m.enrollment_paid, m.last_maintenance_paid,
		    m.pin_hash, m.pin_assigned_at, m.last_contact_attempt_at, m.created_by,
		    ms.id AS ms_id, ms.gym_id AS ms_gym_id, ms.version AS ms_version,
		    ms.created_at AS ms_created_at, ms.updated_at AS ms_updated_at, ms.deleted_at AS ms_deleted_at,
		    ms.member_id AS ms_member_id, ms.membership_type_id AS ms_membership_type_id,
		    ms.type_name_snapshot AS ms_type_name_snapshot, ms.price_snapshot AS ms_price_snapshot,
		    ms.duration_days_snapshot AS ms_duration_days_snapshot,
		    ms.start_date AS ms_start_date, ms.expiry_date AS ms_expiry_date,
		    ms.status AS ms_status, ms.replaced_by AS ms_replaced_by
		FROM members AS m %s
		WHERE %s
		ORDER BY %s
		LIMIT %d OFFSET %d`, join, whereClause, order, pageSize, (page-1)*pageSize)

	var rows []joinedMemberRow
	if err := stx.Select(context.Background(), &rows, listQ, args...); err != nil {
		return nil, 0, err
	}
	out := make([]*memRepo.MemberWithMembership, 0, len(rows))
	for i := range rows {
		mw := &memRepo.MemberWithMembership{Member: memberFromRow(&rows[i].sqliteMemberRow)}
		if rows[i].MS_ID.Valid {
			mw.CurrentMembership = membershipFromJoined(&rows[i])
		}
		out = append(out, mw)
	}
	return out, total, nil
}

func (r *MemberSQLiteRepository) GetWithCurrentMembership(tx sharedDomain.Transaction, gymID, memberID uuid.UUID) (*memRepo.MemberWithMembership, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteMemberRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM members WHERE id = ? AND gym_id = ? AND deleted_at IS NULL`, memberID.String(), gymID.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMemberNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	out := &memRepo.MemberWithMembership{Member: memberFromRow(&row)}
	var msRow sqliteMembershipRow
	err = stx.Get(context.Background(), &msRow,
		`SELECT * FROM memberships WHERE member_id = ? AND status = 'active' AND deleted_at IS NULL`,
		memberID.String())
	if err == nil {
		ms := membershipFromRow(&msRow)
		out.CurrentMembership = ms
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mappers — Member
// ---------------------------------------------------------------------------

func memberToRow(m *memberDomain.Member) sqliteMemberRow {
	row := sqliteMemberRow{
		ID:             m.ID.String(),
		GymID:          m.GymID.String(),
		Version:        m.Version,
		CreatedAt:      m.CreatedAt.UnixMilli(),
		UpdatedAt:      m.UpdatedAt.UnixMilli(),
		Folio:          m.Folio,
		FullName:       m.FullName,
		Phone:          m.Phone,
		Status:         m.Status,
		EnrollmentPaid: boolToInt(m.EnrollmentPaid),
		CreatedBy:      m.CreatedBy.String(),
	}
	if m.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: m.DeletedAt.UnixMilli(), Valid: true}
	}
	if m.Email != nil {
		row.Email = sql.NullString{String: *m.Email, Valid: true}
	}
	if m.Birthdate != nil {
		row.Birthdate = sql.NullString{String: m.Birthdate.UTC().Format(dateLayout), Valid: true}
	}
	if m.PhotoURL != nil {
		row.PhotoURL = sql.NullString{String: *m.PhotoURL, Valid: true}
	}
	if m.Notes != nil {
		row.Notes = sql.NullString{String: *m.Notes, Valid: true}
	}
	if m.LastMaintenancePaid != nil {
		row.LastMaintenancePaid = sql.NullString{String: m.LastMaintenancePaid.UTC().Format(dateLayout), Valid: true}
	}
	if m.PinHash != nil {
		row.PinHash = sql.NullString{String: *m.PinHash, Valid: true}
	}
	if m.PinAssignedAt != nil {
		row.PinAssignedAt = sql.NullInt64{Int64: m.PinAssignedAt.UnixMilli(), Valid: true}
	}
	if m.LastContactAttemptAt != nil {
		row.LastContactAttemptAt = sql.NullInt64{Int64: m.LastContactAttemptAt.UnixMilli(), Valid: true}
	}
	return row
}

func memberFromRow(r *sqliteMemberRow) *memberDomain.Member {
	id, _ := uuid.Parse(r.ID)
	gid, _ := uuid.Parse(r.GymID)
	cb, _ := uuid.Parse(r.CreatedBy)
	m := &memberDomain.Member{
		ID:             id,
		GymID:          gid,
		Version:        r.Version,
		Folio:          r.Folio,
		FullName:       r.FullName,
		Phone:          r.Phone,
		Status:         r.Status,
		EnrollmentPaid: r.EnrollmentPaid != 0,
		CreatedBy:      cb,
		CreatedAt:      time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:      time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.Email.Valid {
		v := r.Email.String
		m.Email = &v
	}
	if r.Birthdate.Valid {
		if t, err := time.Parse(dateLayout, r.Birthdate.String); err == nil {
			m.Birthdate = &t
		}
	}
	if r.PhotoURL.Valid {
		v := r.PhotoURL.String
		m.PhotoURL = &v
	}
	if r.Notes.Valid {
		v := r.Notes.String
		m.Notes = &v
	}
	if r.LastMaintenancePaid.Valid {
		if t, err := time.Parse(dateLayout, r.LastMaintenancePaid.String); err == nil {
			m.LastMaintenancePaid = &t
		}
	}
	if r.PinHash.Valid {
		v := r.PinHash.String
		m.PinHash = &v
	}
	if r.PinAssignedAt.Valid {
		t := time.UnixMilli(r.PinAssignedAt.Int64).UTC()
		m.PinAssignedAt = &t
	}
	if r.LastContactAttemptAt.Valid {
		t := time.UnixMilli(r.LastContactAttemptAt.Int64).UTC()
		m.LastContactAttemptAt = &t
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		m.DeletedAt = &t
	}
	return m
}

func enqueueMember(stx *sharedDomain.SqlxTransaction, m *memberDomain.Member) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":              m.ID.String(),
		"gym_id":          m.GymID.String(),
		"version":         m.Version,
		"folio":           m.Folio,
		"full_name":       m.FullName,
		"phone":           m.Phone,
		"status":          m.Status,
		"enrollment_paid": m.EnrollmentPaid,
		"updated_at":      m.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "members", m.ID.String(), "upsert", payload, m.Version)
}

// membershipFromJoined materialises a Membership from the LEFT JOIN row used
// by List(). Callers MUST guard with `MS_ID.Valid` first.
func membershipFromJoined(j *joinedMemberRow) *membershipDomain.Membership {
	id, _ := uuid.Parse(j.MS_ID.String)
	gymID, _ := uuid.Parse(j.MS_GymID.String)
	memberID, _ := uuid.Parse(j.MS_MemberID.String)
	typeID, _ := uuid.Parse(j.MS_MembershipTypeID.String)
	startDate, _ := time.Parse(dateLayout, j.MS_StartDate.String)
	expiryDate, _ := time.Parse(dateLayout, j.MS_ExpiryDate.String)
	ms := &membershipDomain.Membership{
		ID:                   id,
		GymID:                gymID,
		Version:              int(j.MS_Version.Int64),
		MemberID:             memberID,
		MembershipTypeID:     typeID,
		TypeNameSnapshot:     j.MS_TypeNameSnap.String,
		PriceSnapshot:        fromCents(j.MS_PriceSnap.Int64),
		DurationDaysSnapshot: int(j.MS_DurationSnap.Int64),
		StartDate:            startDate,
		ExpiryDate:           expiryDate,
		Status:               j.MS_Status.String,
		CreatedAt:            time.UnixMilli(j.MS_CreatedAt.Int64).UTC(),
		UpdatedAt:            time.UnixMilli(j.MS_UpdatedAt.Int64).UTC(),
	}
	if j.MS_DeletedAt.Valid {
		t := time.UnixMilli(j.MS_DeletedAt.Int64).UTC()
		ms.DeletedAt = &t
	}
	if j.MS_ReplacedBy.Valid {
		rb, _ := uuid.Parse(j.MS_ReplacedBy.String)
		ms.ReplacedBy = &rb
	}
	return ms
}
