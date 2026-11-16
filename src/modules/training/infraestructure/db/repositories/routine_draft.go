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
)

var _ repository.RoutineDraftRepository = (*RoutinePostgresRepository)(nil)

func (*RoutinePostgresRepository) ReferencesAvailableForSave(handle shared.Transaction, ownerID, routineID uuid.UUID, content routine.Draft, baseVersionID *uuid.UUID) (available bool, err error) {
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
	current, err := persistedDraft(tx, ownerID, routineID)
	if err != nil {
		return false, err
	}
	return saveReferences(tx, ownerID, routineID, content, baseVersionID, current)
}
func persistedDraft(tx *gorm.DB, ownerID, routineID uuid.UUID) (*routine.DraftRecord, error) {
	if !protocol.ValidID(routineID) {
		return nil, trainingErrors.ErrRoutineNotFound
	}
	var row models.RoutineDraftModel
	err := tx.Where("account_id=? AND routine_id=?", ownerID, routineID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, trainingErrors.ErrRoutineNotFound
	}
	if err != nil {
		return nil, err
	}
	return draftToDomain(&row)
}

func saveReferences(tx *gorm.DB, ownerID, routineID uuid.UUID, content routine.Draft, baseVersionID *uuid.UUID, current *routine.DraftRecord) (bool, error) {
	if err := content.Validate(); err != nil {
		return false, err
	}
	previous := map[string]routine.ExerciseRef{}
	if current.Content != nil {
		for _, day := range current.Content.Days {
			for _, e := range day.Exercises {
				if e.ExerciseRef != nil {
					previous[e.ID] = *e.ExerciseRef
				}
			}
		}
	}
	historical := map[string]routine.ExerciseRef{}
	if baseVersionID != nil {
		if !protocol.ValidID(*baseVersionID) {
			return false, trainingErrors.ErrInvalidRoutine
		}
		var found bool
		if err := tx.Raw("SELECT EXISTS(SELECT 1 FROM mancuerna.routine_versions WHERE account_id=? AND routine_id=? AND id=? AND sealed)", ownerID, routineID, *baseVersionID).Row().Scan(&found); err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		var rows []struct {
			ID                                    uuid.UUID
			CatalogExerciseID, PersonalExerciseID *uuid.UUID
		}
		if err := tx.Table("mancuerna.routine_exercises").Select("id,catalog_exercise_id,personal_exercise_id").Where("account_id=? AND version_id=?", ownerID, *baseVersionID).Find(&rows).Error; err != nil {
			return false, err
		}
		for _, row := range rows {
			if (row.CatalogExerciseID == nil) == (row.PersonalExerciseID == nil) {
				return false, trainingErrors.ErrTrainingPersistence
			}
			ref := routine.ExerciseRef{Source: "personal"}
			if row.PersonalExerciseID != nil {
				ref.ID = row.PersonalExerciseID.String()
			} else {
				ref.Source = "catalog"
				ref.ID = row.CatalogExerciseID.String()
			}
			historical[row.ID.String()] = ref
		}
	}
	type referenced struct {
		AppearanceID string `json:"appearance_id"`
		Source       string `json:"source"`
		ID           string `json:"id"`
	}
	refs := []referenced{}
	for _, day := range content.Days {
		for _, e := range day.Exercises {
			if e.ExerciseRef != nil {
				refs = append(refs, referenced{e.ID, e.ExerciseRef.Source, e.ExerciseRef.ID})
			}
		}
	}
	if len(refs) == 0 {
		return true, nil
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return false, err
	}
	var states []struct {
		AppearanceID      string
		Source            string
		ID                string
		Available, Active bool
	}
	err = tx.Raw(`SELECT r.appearance_id,r.source,r.id::text AS id,
 CASE r.source WHEN 'personal' THEN EXISTS(SELECT 1 FROM mancuerna.personal_exercises e WHERE e.account_id=? AND e.id=r.id)
 ELSE EXISTS(SELECT 1 FROM mancuerna.catalog_exercises e WHERE e.id=r.id) END AS available,
 CASE r.source WHEN 'personal' THEN EXISTS(SELECT 1 FROM mancuerna.personal_exercises e WHERE e.account_id=? AND e.id=r.id AND e.archived_at IS NULL AND e.deleted_at IS NULL)
 ELSE EXISTS(SELECT 1 FROM mancuerna.catalog_exercises e WHERE e.id=r.id AND e.archived_at IS NULL AND e.deleted_at IS NULL) END AS active
 FROM jsonb_to_recordset(?::jsonb) AS r(appearance_id text,source text,id uuid)`, ownerID, ownerID, string(raw)).Scan(&states).Error
	if err != nil {
		return false, err
	}
	if len(states) != len(refs) {
		return false, trainingErrors.ErrTrainingPersistence
	}
	for _, state := range states {
		if !state.Available {
			return false, nil
		}
		if state.Active {
			continue
		}
		ref := routine.ExerciseRef{Source: state.Source, ID: state.ID}
		if previous[state.AppearanceID] != ref && historical[state.AppearanceID] != ref {
			return false, nil
		}
	}
	return true, nil
}

func (*RoutinePostgresRepository) Save(handle shared.Transaction, ownerID uuid.UUID, next *routine.DraftRecord, expectedRevision int64) (saved bool, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			saved = false
		}
	}()
	if err = next.ValidateSave(ownerID, expectedRevision); err != nil {
		return false, err
	}
	tx, err := trainingDataConnection(handle, ownerID, true)
	if err != nil {
		return false, err
	}
	current, err := persistedDraft(tx, ownerID, next.RoutineID)
	if err != nil {
		return false, err
	}
	if current.Revision != expectedRevision {
		return false, nil
	}
	if !current.CreatedAt.Equal(next.CreatedAt) || next.UpdatedAt.Before(current.UpdatedAt) {
		return false, trainingErrors.ErrInvalidRoutine
	}
	available, err := saveReferences(tx, ownerID, next.RoutineID, *next.Content, next.BaseVersionID, current)
	if err != nil {
		return false, err
	}
	if !available {
		return false, trainingErrors.ErrRoutineReference
	}
	row, err := draftToModel(next)
	if err != nil {
		return false, err
	}
	var fits bool
	if err = tx.Raw("SELECT octet_length((?::jsonb)::text)<=1048576", string(row.Content)).Row().Scan(&fits); err != nil {
		return false, err
	}
	if !fits {
		return false, trainingErrors.ErrTrainingLimit
	}
	result := tx.Exec(`UPDATE mancuerna.routine_drafts SET content=?::jsonb,base_version_id=?,revision=?,updated_at=?,deleted_at=NULL
 WHERE account_id=? AND routine_id=? AND revision=? AND EXISTS(SELECT 1 FROM mancuerna.routines r WHERE r.account_id=? AND r.id=? AND r.deleted_at IS NULL AND r.status IN ('draft','ready'))`, string(row.Content), row.BaseVersionID, row.Revision, row.UpdatedAt, ownerID, row.RoutineID, expectedRevision, ownerID, row.RoutineID)
	return result.RowsAffected == 1, result.Error
}
