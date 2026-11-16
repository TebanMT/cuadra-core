//go:build server

package repositories

import (
	"encoding/json"
	"errors"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	"github.com/cuadra/cuadra-core/src/modules/training/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RoutinePostgresRepository struct{}

func NewRoutinePostgresRepository() *RoutinePostgresRepository { return &RoutinePostgresRepository{} }

var _ repository.RoutineRepository = (*RoutinePostgresRepository)(nil)

// This is intentionally availability for NEW editorial references. It must not
// be used to discard a historical snapshot whose exercise was later archived.
func (*RoutinePostgresRepository) ReferencesAvailable(handle shared.Transaction, ownerID uuid.UUID, refs []routine.ExerciseRef) (available bool, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			available = false
		}
	}()
	tx, err := trainingDataConnection(handle, ownerID, false)
	if err != nil {
		return false, err
	}
	return newRoutineReferences(tx, ownerID, refs)
}
func newRoutineReferences(tx *gorm.DB, ownerID uuid.UUID, refs []routine.ExerciseRef) (bool, error) {
	if len(refs) > 700 {
		return false, trainingErrors.ErrInvalidRoutine
	}
	for _, ref := range refs {
		id, err := uuid.Parse(ref.ID)
		if err != nil || !protocol.ValidID(id) || id.String() != ref.ID || (ref.Source != "personal" && ref.Source != "catalog") {
			return false, trainingErrors.ErrInvalidRoutine
		}
	}
	if len(refs) == 0 {
		return true, nil
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return false, err
	}
	var available bool
	err = tx.Raw(`SELECT NOT EXISTS (
 SELECT 1 FROM jsonb_to_recordset(?::jsonb) AS r(source text,id uuid)
 WHERE (r.source='personal' AND NOT EXISTS(SELECT 1 FROM mancuerna.personal_exercises e WHERE e.account_id=? AND e.id=r.id AND e.archived_at IS NULL AND e.deleted_at IS NULL))
 OR (r.source='catalog' AND NOT EXISTS(SELECT 1 FROM mancuerna.catalog_exercises e WHERE e.id=r.id AND e.archived_at IS NULL AND e.deleted_at IS NULL)))`, string(raw), ownerID).Row().Scan(&available)
	return available, err
}

func (*RoutinePostgresRepository) Create(handle shared.Transaction, ownerID uuid.UUID, r *routine.Routine, d *routine.DraftRecord) (created bool, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			created = false
		}
	}()
	if err = routine.ValidateCreation(ownerID, r, d); err != nil {
		return false, err
	}
	tx, err := trainingDataConnection(handle, ownerID, true)
	if err != nil {
		return false, err
	}
	available, err := newRoutineReferences(tx, ownerID, d.Content.References())
	if err != nil {
		return false, err
	}
	if !available {
		return false, trainingErrors.ErrRoutineReference
	}
	draft, err := draftToModel(d)
	if err != nil {
		return false, err
	}
	// 059 deliberately has an additional conservative JSONB-text bound. Check
	// before either insert so the caller may persist a terminal size rejection.
	var fits bool
	if err = tx.Raw("SELECT octet_length((?::jsonb)::text)<=1048576", string(draft.Content)).Row().Scan(&fits); err != nil {
		return false, err
	}
	if !fits {
		return false, trainingErrors.ErrTrainingLimit
	}
	row := routineToModel(r)
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil || result.RowsAffected == 0 {
		return false, result.Error
	}
	result = tx.Exec(`INSERT INTO mancuerna.routine_drafts(routine_id,account_id,revision,base_version_id,content,created_at,updated_at,deleted_at)
 VALUES(?,?,?,NULL,?::jsonb,?,?,NULL)`, draft.RoutineID, draft.AccountID, draft.Revision, string(draft.Content), draft.CreatedAt, draft.UpdatedAt)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected != 1 {
		return false, trainingErrors.ErrTrainingPersistence
	}
	return true, nil
}
func (*RoutinePostgresRepository) GetByID(handle shared.Transaction, ownerID, id uuid.UUID) (out *routine.Routine, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			out = nil
		}
	}()
	if !protocol.ValidID(id) {
		return nil, trainingErrors.ErrRoutineNotFound
	}
	tx, err := trainingDataConnection(handle, ownerID, false)
	if err != nil {
		return nil, err
	}
	var row models.RoutineModel
	err = tx.Where("account_id=? AND id=?", ownerID, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, trainingErrors.ErrRoutineNotFound
	}
	if err != nil {
		return nil, err
	}
	return routineToDomain(&row)
}
func (*RoutinePostgresRepository) GetDraftByID(handle shared.Transaction, ownerID, id uuid.UUID) (out *routine.DraftRecord, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			out = nil
		}
	}()
	if !protocol.ValidID(id) {
		return nil, trainingErrors.ErrRoutineNotFound
	}
	tx, err := trainingDataConnection(handle, ownerID, false)
	if err != nil {
		return nil, err
	}
	var row models.RoutineDraftModel
	err = tx.Where("account_id=? AND routine_id=?", ownerID, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, trainingErrors.ErrRoutineNotFound
	}
	if err != nil {
		return nil, err
	}
	return draftToDomain(&row)
}
func routineToModel(r *routine.Routine) models.RoutineModel {
	return models.RoutineModel{ID: r.ID, AccountID: r.AccountID, Title: r.Title, Status: r.Status, Revision: r.Revision, CurrentVersionID: r.CurrentVersionID, DuplicatedFromVersionID: r.DuplicatedFromVersionID, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(), DeletedAt: r.DeletedAt}
}
func routineToDomain(r *models.RoutineModel) (*routine.Routine, error) {
	out := &routine.Routine{ID: r.ID, AccountID: r.AccountID, Title: r.Title, Status: r.Status, Revision: r.Revision, CurrentVersionID: r.CurrentVersionID, DuplicatedFromVersionID: r.DuplicatedFromVersionID, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(), DeletedAt: r.DeletedAt}
	return out, out.Validate()
}
func draftToModel(d *routine.DraftRecord) (models.RoutineDraftModel, error) {
	var content json.RawMessage
	if d.Content != nil {
		var err error
		content, err = json.Marshal(d.Content)
		if err != nil {
			return models.RoutineDraftModel{}, err
		}
	}
	return models.RoutineDraftModel{RoutineID: d.RoutineID, AccountID: d.AccountID, Revision: d.Revision, BaseVersionID: d.BaseVersionID, Content: content, CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(), DeletedAt: d.DeletedAt}, nil
}
func draftToDomain(d *models.RoutineDraftModel) (*routine.DraftRecord, error) {
	out := &routine.DraftRecord{RoutineID: d.RoutineID, AccountID: d.AccountID, Revision: d.Revision, BaseVersionID: d.BaseVersionID, CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(), DeletedAt: d.DeletedAt}
	if len(d.Content) > 0 {
		var content routine.Draft
		if json.Unmarshal(d.Content, &content) != nil {
			return nil, trainingErrors.ErrTrainingPersistence
		}
		out.Content = &content
	}
	return out, out.Validate()
}
