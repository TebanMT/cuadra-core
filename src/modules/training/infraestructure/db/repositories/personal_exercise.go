//go:build server

package repositories

import (
	"context"
	"errors"
	"strings"
	"unicode"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type PersonalExercisePostgresRepository struct{}

func NewPersonalExercisePostgresRepository() *PersonalExercisePostgresRepository {
	return &PersonalExercisePostgresRepository{}
}

var _ repository.PersonalExerciseRepository = (*PersonalExercisePostgresRepository)(nil)

func (r *PersonalExercisePostgresRepository) Create(handle shared.Transaction, ownerID uuid.UUID, e *exercise.Exercise) (created bool, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			created = false
		}
	}()
	if err = e.ValidateCreation(ownerID); err != nil {
		return false, err
	}
	tx, err := trainingDataConnection(handle, ownerID, true)
	if err != nil {
		return false, err
	}
	row := exerciseToModel(e)
	// DO NOTHING is deliberate: a hidden cross-account primary-key collision
	// has exactly the same outcome as an existing ID in this account.
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	return result.RowsAffected == 1, result.Error
}

func (r *PersonalExercisePostgresRepository) GetByID(handle shared.Transaction, ownerID, id uuid.UUID) (out *exercise.Exercise, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			out = nil
		}
	}()
	if !protocol.ValidID(id) {
		return nil, trainingErrors.ErrExerciseNotFound
	}
	tx, err := trainingDataConnection(handle, ownerID, false)
	if err != nil {
		return nil, err
	}
	var row models.PersonalExerciseModel
	err = tx.Where("account_id=? AND id=?", ownerID, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, trainingErrors.ErrExerciseNotFound
	}
	if err != nil {
		return nil, err
	}
	return exerciseToDomain(&row)
}

func trainingDataConnection(handle shared.Transaction, ownerID uuid.UUID, write bool) (*gorm.DB, error) {
	t, ok := handle.(*shared.GormTransaction)
	if !ok || t == nil || t.Tx == nil || t.Tx.Statement == nil || !protocol.ValidID(ownerID) {
		return nil, protocol.ErrCommandScope
	}
	if _, ok = t.Tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return nil, protocol.ErrCommandScope
	}
	tx := t.Tx.Session(&gorm.Session{Logger: logger.Discard, SkipDefaultTransaction: true})
	var scope struct {
		Account, Gym, Attempt, AttemptHash, Isolation, ReadOnly string
		DataOnly                                                bool
	}
	err := tx.Raw(`SELECT coalesce(current_setting('mancuerna.account_id',true),'') AS account,
 coalesce(current_setting('tinta.gym_id',true),'') AS gym,coalesce(current_setting('mancuerna.attempt_id',true),'') AS attempt,
 coalesce(current_setting('mancuerna.attempt_hash',true),'') AS attempt_hash,current_setting('transaction_isolation') AS isolation,
 current_setting('transaction_read_only') AS read_only,(pg_has_role(current_user,'mancuerna_data','USAGE')
 AND NOT pg_has_role(current_user,'mancuerna_auth','MEMBER') AND NOT pg_has_role(current_user,'mancuerna_identity','MEMBER')) AS data_only`).Scan(&scope).Error
	if err != nil {
		return nil, err
	}
	if scope.Account != ownerID.String() || scope.Gym != "" || scope.Attempt != "" || scope.AttemptHash != "" || !scope.DataOnly || scope.Isolation != "read committed" || (write && scope.ReadOnly != "off") {
		return nil, protocol.ErrCommandScope
	}
	if write {
		var locked uuid.UUID
		if err = tx.Raw("SELECT account_id FROM mancuerna.sync_streams WHERE account_id=? FOR UPDATE", ownerID).Row().Scan(&locked); err != nil {
			return nil, err
		}
		if locked != ownerID {
			return nil, protocol.ErrCommandScope
		}
	}
	return tx, nil
}

func trainingPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{trainingErrors.ErrInvalidExercise, trainingErrors.ErrExerciseNotFound, trainingErrors.ErrInvalidRoutine, trainingErrors.ErrRoutineNotFound, trainingErrors.ErrRoutineReference, trainingErrors.ErrRoutineRevisionConflict, trainingErrors.ErrTrainingLimit, protocol.ErrCommandScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	return trainingErrors.ErrTrainingPersistence
}

func exerciseToModel(e *exercise.Exercise) models.PersonalExerciseModel {
	return models.PersonalExerciseModel{ID: e.ID, AccountID: e.AccountID, Name: e.Name, SearchKey: exerciseSearchKey(e.Name), LoadMode: string(e.LoadMode), Notes: e.Notes, Revision: e.Revision, CreatedAt: e.CreatedAt.UTC(), UpdatedAt: e.UpdatedAt.UTC(), ArchivedAt: e.ArchivedAt, DeletedAt: e.DeletedAt}
}
func exerciseToDomain(row *models.PersonalExerciseModel) (*exercise.Exercise, error) {
	e := &exercise.Exercise{ID: row.ID, AccountID: row.AccountID, Name: row.Name, LoadMode: exercise.LoadMode(row.LoadMode), Notes: row.Notes, Revision: row.Revision, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(), ArchivedAt: row.ArchivedAt, DeletedAt: row.DeletedAt}
	return e, e.Validate()
}

// Derived search material only; never alters the display name or request hash.
// Equivalent names are not identities and are intentionally not unique.
func exerciseSearchKey(name string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(name)) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
