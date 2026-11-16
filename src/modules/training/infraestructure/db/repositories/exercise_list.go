//go:build server

package repositories

import (
	"strings"
	"unicode/utf8"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/search"
	"github.com/cuadra/cuadra-core/src/modules/training/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type ExerciseListPostgresRepository struct{}

func NewExerciseListPostgresRepository() *ExerciseListPostgresRepository {
	return &ExerciseListPostgresRepository{}
}

var _ repository.ExerciseListRepository = (*ExerciseListPostgresRepository)(nil)

func (*ExerciseListPostgresRepository) List(handle shared.Transaction, ownerID uuid.UUID, filter exercise.ListFilter, after *exercise.ListPosition, limit int) (out []*exercise.ListedExercise, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			out = nil
		}
	}()
	if !utf8.ValidString(filter.Query) || filter.Query != search.Normalize(filter.Query) || len(filter.Query) > 4096 || strings.ContainsRune(filter.Query, 0) || limit < 1 || limit > 101 || after != nil && after.Validate() != nil {
		return nil, trainingErrors.ErrInvalidExercise
	}
	tx, err := trainingDataConnection(handle, ownerID, false)
	if err != nil {
		return nil, err
	}
	sql, args := exerciseListStatement(ownerID, filter, after, limit)
	var rows []models.ExerciseListModel
	if err = tx.Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out = make([]*exercise.ListedExercise, 0, len(rows))
	for _, row := range rows {
		item, err := listedExerciseToDomain(&row)
		if err != nil {
			return nil, trainingErrors.ErrTrainingPersistence
		}
		out = append(out, item)
	}
	return out, nil
}

func exerciseListStatement(ownerID uuid.UUID, filter exercise.ListFilter, after *exercise.ListPosition, limit int) (string, []any) {
	// The stored search_key predates full Unicode folding. Normalize the display
	// name on read so existing catalog/personal records obey the same rules.
	sql := `SELECT * FROM (
 SELECT id,NULL::uuid AS account_id,'catalog'::text AS source,name,load_mode,notes,revision,created_at,updated_at,archived_at,deleted_at
 FROM mancuerna.catalog_exercises WHERE archived_at IS NULL AND deleted_at IS NULL
 UNION ALL
 SELECT id,account_id,'personal'::text AS source,name,load_mode,notes,revision,created_at,updated_at,archived_at,deleted_at
 FROM mancuerna.personal_exercises WHERE account_id=? AND archived_at IS NULL AND deleted_at IS NULL
) AS choices WHERE true`
	args := []any{ownerID}
	if filter.Query != "" {
		expression, searchArgs := trainingSearchExpression(searchName, nil)
		sql += " AND position(? IN " + expression + ")>0"
		args = append(args, filter.Query)
		args = append(args, searchArgs...)
	}
	if after != nil {
		sql += ` AND (name COLLATE "C",source COLLATE "C",id)>(?::text COLLATE "C",?::text COLLATE "C",?::uuid)`
		args = append(args, after.Name, after.Source, after.ID)
	}
	sql += ` ORDER BY name COLLATE "C" ASC,source COLLATE "C" ASC,id ASC LIMIT ?`
	args = append(args, limit)
	return sql, args
}

func listedExerciseToDomain(row *models.ExerciseListModel) (*exercise.ListedExercise, error) {
	e := &exercise.ListedExercise{Exercise: exercise.Exercise{ID: row.ID, Name: row.Name, LoadMode: exercise.LoadMode(row.LoadMode), Notes: row.Notes, Revision: row.Revision, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(), ArchivedAt: row.ArchivedAt, DeletedAt: row.DeletedAt}, Source: row.Source}
	if row.AccountID != nil {
		e.AccountID = *row.AccountID
	}
	return e, e.Validate()
}
