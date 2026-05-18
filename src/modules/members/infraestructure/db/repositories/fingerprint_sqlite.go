//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type FingerprintSQLiteRepository struct{}

func NewFingerprintSQLiteRepository() *FingerprintSQLiteRepository {
	return &FingerprintSQLiteRepository{}
}

type sqliteFingerprintRow struct {
	ID                string        `db:"id"`
	GymID             string        `db:"gym_id"`
	Version           int           `db:"version"`
	CreatedAt         int64         `db:"created_at"`
	UpdatedAt         int64         `db:"updated_at"`
	DeletedAt         sql.NullInt64 `db:"deleted_at"`
	SyncedAt          sql.NullInt64 `db:"synced_at"`
	MemberID          string        `db:"member_id"`
	TemplateEncrypted []byte        `db:"template_encrypted"`
	TemplateFormat    string        `db:"template_format"`
	QualityScore      sql.NullInt64 `db:"quality_score"`
	RegisteredBy      string        `db:"registered_by"`
}

func (r *FingerprintSQLiteRepository) Create(tx sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := fingerprintToRow(fp)
	const stmt = `
		INSERT INTO member_fingerprints (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    member_id, template_encrypted, template_format, quality_score, registered_by
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :member_id, :template_encrypted, :template_format, :quality_score, :registered_by
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueFingerprint(stx, fp); err != nil {
		return nil, err
	}
	return fp, nil
}

func (r *FingerprintSQLiteRepository) Update(tx sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	fp.UpdatedAt = time.Now().UTC()
	row := fingerprintToRow(fp)
	const stmt = `
		UPDATE member_fingerprints SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    template_encrypted = :template_encrypted, template_format = :template_format,
		    quality_score = :quality_score
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueFingerprint(stx, fp); err != nil {
		return nil, err
	}
	return fp, nil
}

func (r *FingerprintSQLiteRepository) ListByMember(tx sharedDomain.Transaction, memberID uuid.UUID) ([]*fpDomain.MemberFingerprint, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteFingerprintRow
	err := stx.Select(context.Background(), &rows,
		`SELECT id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
		        member_id, template_encrypted, template_format, quality_score, registered_by
		 FROM member_fingerprints
		 WHERE member_id = ? AND deleted_at IS NULL
		 ORDER BY created_at ASC`,
		memberID.String())
	if err != nil {
		return nil, err
	}
	out := make([]*fpDomain.MemberFingerprint, 0, len(rows))
	for i := range rows {
		out = append(out, fingerprintFromRow(&rows[i]))
	}
	return out, nil
}

func (r *FingerprintSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*fpDomain.MemberFingerprint, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteFingerprintRow
	err := stx.Select(context.Background(), &rows,
		`SELECT id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
		        member_id, template_encrypted, template_format, quality_score, registered_by
		 FROM member_fingerprints
		 WHERE gym_id = ? AND deleted_at IS NULL
		 ORDER BY created_at ASC`,
		gymID.String())
	if err != nil {
		return nil, err
	}
	out := make([]*fpDomain.MemberFingerprint, 0, len(rows))
	for i := range rows {
		out = append(out, fingerprintFromRow(&rows[i]))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func fingerprintToRow(f *fpDomain.MemberFingerprint) sqliteFingerprintRow {
	row := sqliteFingerprintRow{
		ID:                f.ID.String(),
		GymID:             f.GymID.String(),
		Version:           f.Version,
		CreatedAt:         f.CreatedAt.UnixMilli(),
		UpdatedAt:         f.UpdatedAt.UnixMilli(),
		MemberID:          f.MemberID.String(),
		TemplateEncrypted: f.TemplateEncrypted,
		TemplateFormat:    f.TemplateFormat,
		RegisteredBy:      f.RegisteredBy.String(),
	}
	if f.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: f.DeletedAt.UnixMilli(), Valid: true}
	}
	if f.QualityScore != nil {
		row.QualityScore = sql.NullInt64{Int64: int64(*f.QualityScore), Valid: true}
	}
	return row
}

func fingerprintFromRow(r *sqliteFingerprintRow) *fpDomain.MemberFingerprint {
	id, _ := uuid.Parse(r.ID)
	gid, _ := uuid.Parse(r.GymID)
	mid, _ := uuid.Parse(r.MemberID)
	rb, _ := uuid.Parse(r.RegisteredBy)
	fp := &fpDomain.MemberFingerprint{
		ID:                id,
		GymID:             gid,
		Version:           r.Version,
		MemberID:          mid,
		TemplateEncrypted: r.TemplateEncrypted,
		TemplateFormat:    r.TemplateFormat,
		RegisteredBy:      rb,
		CreatedAt:         time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:         time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.QualityScore.Valid {
		v := int(r.QualityScore.Int64)
		fp.QualityScore = &v
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		fp.DeletedAt = &t
	}
	return fp
}

func enqueueFingerprint(stx *sharedDomain.SqlxTransaction, f *fpDomain.MemberFingerprint) error {
	if stx.Queue == nil {
		return nil
	}
	// Encrypted bytes go through base64 — sync_queue.payload is TEXT.
	payload, err := json.Marshal(map[string]any{
		"id":                 f.ID.String(),
		"gym_id":             f.GymID.String(),
		"version":            f.Version,
		"member_id":          f.MemberID.String(),
		"template_encrypted": base64.StdEncoding.EncodeToString(f.TemplateEncrypted),
		"template_format":    f.TemplateFormat,
		"quality_score":      f.QualityScore,
		"registered_by":      f.RegisteredBy.String(),
		"updated_at":         f.UpdatedAt.UnixMilli(),
		"deleted_at_ms":      deletedAtMillis(f.DeletedAt),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "member_fingerprints", f.ID.String(), "upsert", payload, f.Version)
}

func deletedAtMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}
