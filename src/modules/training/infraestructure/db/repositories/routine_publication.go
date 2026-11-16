//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"math"
	"sort"

	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
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

type RoutinePublicationPostgresRepository struct{}

func NewRoutinePublicationPostgresRepository() *RoutinePublicationPostgresRepository {
	return &RoutinePublicationPostgresRepository{}
}

var _ repository.RoutinePublicationRepository = (*RoutinePublicationPostgresRepository)(nil)

func (*RoutinePublicationPostgresRepository) ResolveMaterial(handle shared.Transaction, ownerID uuid.UUID, draft *routine.DraftRecord) (out routine.PublicationMaterial, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			out = routine.PublicationMaterial{}
		}
	}()
	if draft.Validate() != nil || draft.AccountID != ownerID || draft.Content == nil || draft.DeletedAt != nil {
		return out, trainingErrors.ErrInvalidRoutine
	}
	tx, err := trainingDataConnection(handle, ownerID, false)
	if err != nil {
		return out, err
	}
	current, err := persistedDraft(tx, ownerID, draft.RoutineID)
	if err != nil {
		return out, err
	}
	if current.Revision != draft.Revision {
		return out, trainingErrors.ErrRoutineRevisionConflict
	}
	if current.Content == nil {
		return out, trainingErrors.ErrRoutineReference
	}
	var maximum int64
	if err = tx.Raw("SELECT coalesce(max(version_number),0)::bigint FROM mancuerna.routine_versions WHERE account_id=? AND routine_id=?", ownerID, current.RoutineID).Row().Scan(&maximum); err != nil {
		return out, err
	}
	if maximum >= math.MaxInt32 {
		return out, trainingErrors.ErrTrainingLimit
	}
	out = routine.PublicationMaterial{NextVersionNumber: int(maximum + 1), Exercises: map[string]routine.ExerciseSnapshot{}}
	history := map[string]routine.ExerciseSnapshot{}
	if current.BaseVersionID != nil {
		var found bool
		if err = tx.Raw("SELECT EXISTS(SELECT 1 FROM mancuerna.routine_versions WHERE account_id=? AND routine_id=? AND id=? AND sealed)", ownerID, current.RoutineID, *current.BaseVersionID).Row().Scan(&found); err != nil {
			return out, err
		}
		if !found {
			return out, trainingErrors.ErrRoutineReference
		}
		var rows []models.RoutineExerciseModel
		if err = tx.Where("account_id=? AND version_id=?", ownerID, *current.BaseVersionID).Find(&rows).Error; err != nil {
			return out, err
		}
		for _, row := range rows {
			ref, refErr := publicationReference(row)
			if refErr != nil {
				return out, refErr
			}
			history[row.ID.String()] = routine.ExerciseSnapshot{Reference: ref, Name: row.NameSnapshot, LoadMode: row.LoadModeSnapshot}
		}
	}
	type reference struct {
		AppearanceID string `json:"appearance_id"`
		Source       string `json:"source"`
		ID           string `json:"id"`
	}
	refs := []reference{}
	for _, day := range current.Content.Days {
		for _, e := range day.Exercises {
			if e.ExerciseRef == nil {
				return out, trainingErrors.ErrInvalidRoutine
			}
			refs = append(refs, reference{e.ID, e.ExerciseRef.Source, e.ExerciseRef.ID})
		}
	}
	if len(refs) == 0 {
		return out, trainingErrors.ErrInvalidRoutine
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return out, err
	}
	var rows []struct {
		AppearanceID, Source, ID, Name, LoadMode string
		Available, Active                        bool
	}
	err = tx.Raw(`SELECT r.appearance_id,r.source,r.id::text AS id,coalesce(p.name,c.name,'') AS name,coalesce(p.load_mode,c.load_mode,'') AS load_mode,
 (p.id IS NOT NULL OR c.id IS NOT NULL) AS available,
 (p.id IS NOT NULL AND p.archived_at IS NULL AND p.deleted_at IS NULL OR c.id IS NOT NULL AND c.archived_at IS NULL AND c.deleted_at IS NULL) AS active
 FROM jsonb_to_recordset(?::jsonb) AS r(appearance_id text,source text,id uuid)
 LEFT JOIN mancuerna.personal_exercises p ON r.source='personal' AND p.account_id=? AND p.id=r.id
 LEFT JOIN mancuerna.catalog_exercises c ON r.source='catalog' AND c.id=r.id`, string(raw), ownerID).Scan(&rows).Error
	if err != nil {
		return out, err
	}
	if len(rows) != len(refs) {
		return out, trainingErrors.ErrTrainingPersistence
	}
	for _, row := range rows {
		ref := routine.ExerciseRef{Source: row.Source, ID: row.ID}
		if !row.Available {
			return out, trainingErrors.ErrRoutineReference
		}
		if row.Active {
			out.Exercises[row.AppearanceID] = routine.ExerciseSnapshot{Reference: ref, Name: row.Name, LoadMode: row.LoadMode}
			continue
		}
		snapshot, exists := history[row.AppearanceID]
		if !exists || snapshot.Reference != ref {
			return out, trainingErrors.ErrRoutineReference
		}
		out.Exercises[row.AppearanceID] = snapshot
	}
	return out, nil
}

func publicationReference(row models.RoutineExerciseModel) (routine.ExerciseRef, error) {
	if (row.PersonalExerciseID == nil) == (row.CatalogExerciseID == nil) {
		return routine.ExerciseRef{}, trainingErrors.ErrTrainingPersistence
	}
	if row.PersonalExerciseID != nil {
		return routine.ExerciseRef{Source: "personal", ID: row.PersonalExerciseID.String()}, nil
	}
	return routine.ExerciseRef{Source: "catalog", ID: row.CatalogExerciseID.String()}, nil
}

func (*RoutinePublicationPostgresRepository) Publish(handle shared.Transaction, ownerID uuid.UUID, p *routine.Publication) (created bool, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			created = false
		}
	}()
	if err = p.Validate(ownerID); err != nil {
		return false, err
	}
	if err = verifyVersion(&p.Version); err != nil {
		return false, err
	}
	tx, err := trainingDataConnection(handle, ownerID, true)
	if err != nil {
		return false, err
	}
	var root models.RoutineModel
	if err = tx.Where("account_id=? AND id=?", ownerID, p.Root.ID).Take(&root).Error; err != nil {
		return false, err
	}
	current, err := persistedDraft(tx, ownerID, p.Draft.RoutineID)
	if err != nil {
		return false, err
	}
	if root.Revision != p.ExpectedRootRevision || current.Revision != p.ExpectedDraftRevision || root.DeletedAt != nil || root.Status == "archived" || current.DeletedAt != nil || current.Content == nil {
		return false, trainingErrors.ErrRoutineRevisionConflict
	}
	if !root.CreatedAt.Equal(p.Root.CreatedAt) || !current.CreatedAt.Equal(p.Draft.CreatedAt) || p.Root.UpdatedAt.Before(root.UpdatedAt) || p.Draft.UpdatedAt.Before(current.UpdatedAt) || !sameOptionalID(root.DuplicatedFromVersionID, p.Root.DuplicatedFromVersionID) {
		return false, trainingErrors.ErrInvalidRoutine
	}
	var maximum int64
	if err = tx.Raw("SELECT coalesce(max(version_number),0)::bigint FROM mancuerna.routine_versions WHERE account_id=? AND routine_id=?", ownerID, p.Root.ID).Row().Scan(&maximum); err != nil {
		return false, err
	}
	if maximum+1 != int64(p.Version.VersionNumber) {
		return false, trainingErrors.ErrRoutineRevisionConflict
	}
	// This SQL savepoint cannot commit or escape the caller transaction. The
	// account mutex was acquired before it and remains held after a collision.
	const savepoint = "mancuerna_routine_publication"
	if err = tx.Exec("SAVEPOINT " + savepoint).Error; err != nil {
		return false, err
	}
	collision := func() (bool, error) {
		if err := tx.Exec("ROLLBACK TO SAVEPOINT " + savepoint).Error; err != nil {
			return false, err
		}
		return false, tx.Exec("RELEASE SAVEPOINT " + savepoint).Error
	}
	version, days, exercises := versionToModels(&p.Version)
	r := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&version)
	if r.Error != nil {
		return false, r.Error
	}
	if r.RowsAffected != 1 {
		return collision()
	}
	// Fixed table order plus UUID order avoids reversing unique-index waits
	// between accounts attempting overlapping destination IDs.
	sort.Slice(days, func(i, j int) bool { return days[i].ID.String() < days[j].ID.String() })
	sort.Slice(exercises, func(i, j int) bool { return exercises[i].ID.String() < exercises[j].ID.String() })
	r = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&days)
	if r.Error != nil {
		return false, r.Error
	}
	if r.RowsAffected != int64(len(days)) {
		return collision()
	}
	r = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&exercises)
	if r.Error != nil {
		return false, r.Error
	}
	if r.RowsAffected != int64(len(exercises)) {
		return collision()
	}
	r = tx.Exec("UPDATE mancuerna.routine_versions SET sealed=true WHERE account_id=? AND id=? AND NOT sealed", ownerID, p.Version.ID)
	if r.Error != nil {
		return false, r.Error
	}
	if r.RowsAffected != 1 {
		return false, trainingErrors.ErrTrainingPersistence
	}
	r = tx.Exec(`UPDATE mancuerna.routines SET title=?,status='ready',current_version_id=?,revision=?,updated_at=? WHERE account_id=? AND id=? AND revision=? AND deleted_at IS NULL AND status IN ('draft','ready')`, p.Root.Title, p.Version.ID, p.Root.Revision, p.Root.UpdatedAt, ownerID, p.Root.ID, p.ExpectedRootRevision)
	if r.Error != nil {
		return false, r.Error
	}
	if r.RowsAffected != 1 {
		return false, trainingErrors.ErrRoutineRevisionConflict
	}
	r = tx.Exec(`UPDATE mancuerna.routine_drafts SET content=NULL,base_version_id=?,revision=?,updated_at=?,deleted_at=? WHERE account_id=? AND routine_id=? AND revision=? AND content IS NOT NULL AND deleted_at IS NULL`, p.Version.ID, p.Draft.Revision, p.Draft.UpdatedAt, p.Draft.DeletedAt, ownerID, p.Draft.RoutineID, p.ExpectedDraftRevision)
	if r.Error != nil {
		return false, r.Error
	}
	if r.RowsAffected != 1 {
		return false, trainingErrors.ErrRoutineRevisionConflict
	}
	if err = tx.Exec("RELEASE SAVEPOINT " + savepoint).Error; err != nil {
		return false, err
	}
	return true, nil
}

func sameOptionalID(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
func versionToModels(v *routine.Version) (models.RoutineVersionModel, []models.RoutineDayModel, []models.RoutineExerciseModel) {
	version := models.RoutineVersionModel{ID: v.ID, AccountID: v.AccountID, RoutineID: v.RoutineID, VersionNumber: v.VersionNumber, TitleSnapshot: v.TitleSnapshot, ContentHash: append([]byte(nil), v.ContentHash[:]...), FormatVersion: 1, SourceDraftRevision: v.SourceDraftRevision, ActorDeviceID: v.ActorDeviceID, PublishedAt: v.PublishedAt}
	days := make([]models.RoutineDayModel, 0, len(v.Days))
	exercises := []models.RoutineExerciseModel{}
	for _, day := range v.Days {
		days = append(days, models.RoutineDayModel{ID: day.ID, AccountID: v.AccountID, VersionID: v.ID, Label: day.Label, Position: day.Position})
		for _, e := range day.Exercises {
			row := models.RoutineExerciseModel{ID: e.ID, AccountID: v.AccountID, VersionID: v.ID, DayID: day.ID, NameSnapshot: e.NameSnapshot, LoadModeSnapshot: e.LoadModeSnapshot, Position: e.Position, PrescribedSets: e.PrescribedSets, PrescribedReps: e.PrescribedReps, RestSeconds: e.RestSeconds, Notes: e.Notes}
			id := uuid.MustParse(e.Reference.ID)
			if e.Reference.Source == "personal" {
				row.PersonalExerciseID = &id
			} else {
				row.CatalogExerciseID = &id
			}
			exercises = append(exercises, row)
		}
	}
	return version, days, exercises
}

func (*RoutinePublicationPostgresRepository) GetVersion(handle shared.Transaction, ownerID, id uuid.UUID) (out *routine.Version, err error) {
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
	var row models.RoutineVersionModel
	err = tx.Where("account_id=? AND id=? AND sealed", ownerID, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, trainingErrors.ErrRoutineNotFound
	}
	if err != nil {
		return nil, err
	}
	if row.FormatVersion != 1 || len(row.ContentHash) != 32 {
		return nil, trainingErrors.ErrTrainingPersistence
	}
	out = &routine.Version{ID: row.ID, AccountID: row.AccountID, RoutineID: row.RoutineID, ActorDeviceID: row.ActorDeviceID, VersionNumber: row.VersionNumber, TitleSnapshot: row.TitleSnapshot, SourceDraftRevision: row.SourceDraftRevision, PublishedAt: row.PublishedAt.UTC(), Days: []routine.PublishedDay{}}
	copy(out.ContentHash[:], row.ContentHash)
	var days []models.RoutineDayModel
	if err = tx.Where("account_id=? AND version_id=?", ownerID, id).Order("position,id").Find(&days).Error; err != nil {
		return nil, err
	}
	indices := map[uuid.UUID]int{}
	for i, day := range days {
		indices[day.ID] = i
		out.Days = append(out.Days, routine.PublishedDay{ID: day.ID, Label: day.Label, Position: day.Position, Exercises: []routine.PublishedExercise{}})
	}
	var exercises []models.RoutineExerciseModel
	if err = tx.Where("account_id=? AND version_id=?", ownerID, id).Order("day_id,position,id").Find(&exercises).Error; err != nil {
		return nil, err
	}
	for _, e := range exercises {
		i, exists := indices[e.DayID]
		if !exists {
			return nil, trainingErrors.ErrTrainingPersistence
		}
		ref, err := publicationReference(e)
		if err != nil {
			return nil, err
		}
		out.Days[i].Exercises = append(out.Days[i].Exercises, routine.PublishedExercise{ID: e.ID, Reference: ref, NameSnapshot: e.NameSnapshot, LoadModeSnapshot: e.LoadModeSnapshot, Position: e.Position, PrescribedSets: e.PrescribedSets, PrescribedReps: e.PrescribedReps, RestSeconds: e.RestSeconds, Notes: e.Notes})
	}
	if err = out.Validate(); err != nil {
		return nil, trainingErrors.ErrTrainingPersistence
	}
	if err = verifyVersion(out); err != nil {
		return nil, trainingErrors.ErrTrainingPersistence
	}
	return out, nil
}
func verifyVersion(v *routine.Version) error {
	if v.Validate() != nil {
		return trainingErrors.ErrInvalidRoutine
	}
	raw, err := json.Marshal(v.DTO())
	if err != nil {
		return trainingErrors.ErrInvalidRoutine
	}
	codec := crypto.JSONCanonicalizer{}
	raw, err = codec.Canonicalize(raw)
	if err != nil {
		return trainingErrors.ErrInvalidRoutine
	}
	if _, err = protocol.DecodeRoutineVersion(raw, codec); err != nil {
		return trainingErrors.ErrInvalidRoutine
	}
	return nil
}
