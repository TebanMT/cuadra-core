//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type UserSQLiteRepository struct{}

func NewUserSQLiteRepository() *UserSQLiteRepository { return &UserSQLiteRepository{} }

type sqliteUserRow struct {
	ID                 string         `db:"id"`
	GymID              string         `db:"gym_id"`
	Version            int            `db:"version"`
	CreatedAt          int64          `db:"created_at"`
	UpdatedAt          int64          `db:"updated_at"`
	DeletedAt          sql.NullInt64  `db:"deleted_at"`
	SyncedAt           sql.NullInt64  `db:"synced_at"`
	Email              string         `db:"email"`
	PasswordHash       string         `db:"password_hash"`
	FullName           string         `db:"full_name"`
	Phone              sql.NullString `db:"phone"`
	Role               string         `db:"role"`
	Active             int            `db:"active"`
	MustChangePassword int            `db:"must_change_password"`
	LastLoginAt        sql.NullInt64  `db:"last_login_at"`
	CreatedBy          sql.NullString `db:"created_by"`
	PinHash            sql.NullString `db:"pin_hash"`
	PinAssignedAt      sql.NullInt64  `db:"pin_assigned_at"`
}

func userToRow(u *userDomain.User) sqliteUserRow {
	r := sqliteUserRow{
		ID:                 u.ID.String(),
		GymID:              u.GymID.String(),
		Version:            u.Version,
		CreatedAt:          u.CreatedAt.UnixMilli(),
		UpdatedAt:          u.UpdatedAt.UnixMilli(),
		Email:              u.Email,
		PasswordHash:       u.PasswordHash,
		FullName:           u.FullName,
		Role:               u.Role,
		Active:             boolToInt(u.Active),
		MustChangePassword: boolToInt(u.MustChangePassword),
	}
	if u.Phone != nil {
		r.Phone = sql.NullString{String: *u.Phone, Valid: true}
	}
	if u.CreatedBy != nil {
		r.CreatedBy = sql.NullString{String: u.CreatedBy.String(), Valid: true}
	}
	if u.DeletedAt != nil {
		r.DeletedAt = sql.NullInt64{Int64: u.DeletedAt.UnixMilli(), Valid: true}
	}
	if u.LastLoginAt != nil {
		r.LastLoginAt = sql.NullInt64{Int64: u.LastLoginAt.UnixMilli(), Valid: true}
	}
	if u.PinHash != nil {
		r.PinHash = sql.NullString{String: *u.PinHash, Valid: true}
	}
	if u.PinAssignedAt != nil {
		r.PinAssignedAt = sql.NullInt64{Int64: u.PinAssignedAt.UnixMilli(), Valid: true}
	}
	return r
}

func userFromRow(r *sqliteUserRow) *userDomain.User {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	u := &userDomain.User{
		ID:                 id,
		GymID:              gymID,
		Version:            r.Version,
		Email:              r.Email,
		PasswordHash:       r.PasswordHash,
		FullName:           r.FullName,
		Role:               r.Role,
		Active:             r.Active != 0,
		MustChangePassword: r.MustChangePassword != 0,
		CreatedAt:          time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:          time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.Phone.Valid {
		v := r.Phone.String
		u.Phone = &v
	}
	if r.CreatedBy.Valid {
		c, _ := uuid.Parse(r.CreatedBy.String)
		u.CreatedBy = &c
	}
	if r.LastLoginAt.Valid {
		t := time.UnixMilli(r.LastLoginAt.Int64).UTC()
		u.LastLoginAt = &t
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		u.DeletedAt = &t
	}
	if r.PinHash.Valid {
		v := r.PinHash.String
		u.PinHash = &v
	}
	if r.PinAssignedAt.Valid {
		t := time.UnixMilli(r.PinAssignedAt.Int64).UTC()
		u.PinAssignedAt = &t
	}
	return u
}

func (r *UserSQLiteRepository) Create(tx sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := userToRow(u)
	const stmt = `
		INSERT INTO users (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    email, password_hash, full_name, phone, role, active, must_change_password, last_login_at, created_by,
		    pin_hash, pin_assigned_at
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :email, :password_hash, :full_name, :phone, :role, :active, :must_change_password, :last_login_at, :created_by,
		    :pin_hash, :pin_assigned_at
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueUser(stx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UserSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*userDomain.User, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteUserRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM users WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(userErrors.ErrUserNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return userFromRow(&row), nil
}

func (r *UserSQLiteRepository) GetByEmail(tx sharedDomain.Transaction, email string) (*userDomain.User, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteUserRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM users WHERE email = ? COLLATE NOCASE AND deleted_at IS NULL`, strings.ToLower(email))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(userErrors.ErrUserNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return userFromRow(&row), nil
}

func (r *UserSQLiteRepository) ExistsByEmail(tx sharedDomain.Transaction, email string) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM users WHERE email = ? COLLATE NOCASE AND deleted_at IS NULL`,
		strings.ToLower(email))
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *UserSQLiteRepository) Update(tx sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	u.UpdatedAt = time.Now().UTC()
	row := userToRow(u)
	const stmt = `
		UPDATE users SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    email = :email, password_hash = :password_hash, full_name = :full_name,
		    phone = :phone, role = :role, active = :active,
		    must_change_password = :must_change_password, last_login_at = :last_login_at,
		    pin_hash = :pin_hash, pin_assigned_at = :pin_assigned_at
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueUser(stx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UserSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*userDomain.User, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteUserRow
	err := stx.Select(context.Background(), &rows,
		`SELECT * FROM users WHERE gym_id = ? AND deleted_at IS NULL ORDER BY created_at ASC`,
		gymID.String())
	if err != nil {
		return nil, err
	}
	out := make([]*userDomain.User, len(rows))
	for i := range rows {
		out[i] = userFromRow(&rows[i])
	}
	return out, nil
}

func (r *UserSQLiteRepository) CountOperatorsByGym(tx sharedDomain.Transaction, gymID uuid.UUID) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM users WHERE gym_id = ? AND role = ? AND deleted_at IS NULL`,
		gymID.String(), userDomain.RoleOperator)
	return n, err
}

// pinAssignedAtMs serializes the optional timestamp as a unix-ms number for
// the JSON sync payload, mirroring how the rest of this repo encodes nullable
// timestamps. nil -> nil, matched on the cloud-side projector.
func pinAssignedAtMs(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := t.UnixMilli()
	return &v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func enqueueUser(stx *sharedDomain.SqlxTransaction, u *userDomain.User) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":                   u.ID.String(),
		"gym_id":               u.GymID.String(),
		"version":              u.Version,
		"email":                u.Email,
		"password_hash":        u.PasswordHash, // template-only; cloud trusts payload
		"full_name":            u.FullName,
		"phone":                u.Phone,
		"role":                 u.Role,
		"active":               u.Active,
		"must_change_password": u.MustChangePassword,
		"pin_hash":             u.PinHash,
		"pin_assigned_at":      pinAssignedAtMs(u.PinAssignedAt),
		"updated_at":           u.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "users", u.ID.String(), "upsert", payload, u.Version)
}
