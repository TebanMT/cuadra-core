//go:build server

package repositories

import (
	"strings"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	"github.com/cuadra/cuadra-core/src/modules/training/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

var _ repository.RoutineListRepository = (*RoutinePostgresRepository)(nil)

// Compatibility wrappers retain the routine differential tests and semantics.
var routineSearch = trainingSearch

func routineSearchTables() trainingSearchMapping { return trainingSearchTables() }
func routineSearchExpression(value *string) (string, []any) {
	return trainingSearchExpression(searchTitle, value)
}

func (*RoutinePostgresRepository) List(handle shared.Transaction, ownerID uuid.UUID, filter routine.ListFilter, after *routine.ListPosition, limit int) (out []*routine.Routine, err error) {
	defer func() {
		err = trainingPersistenceError(err)
		if err != nil {
			out = nil
		}
	}()
	if filter.Status != "active" && filter.Status != "archived" && filter.Status != "all" || filter.Query != routine.NormalizeSearch(filter.Query) || len(filter.Query) > 4096 || strings.ContainsRune(filter.Query, 0) || limit < 1 || limit > 101 || after != nil && after.Validate() != nil {
		return nil, trainingErrors.ErrInvalidRoutine
	}
	tx, err := trainingDataConnection(handle, ownerID, false)
	if err != nil {
		return nil, err
	}
	query := tx.Where("account_id=? AND deleted_at IS NULL", ownerID)
	switch filter.Status {
	case "active":
		query = query.Where("status IN ('draft','ready')")
	case "archived":
		query = query.Where("status='archived'")
	}
	if filter.Query != "" {
		expression, args := routineSearchExpression(nil)
		query = query.Where("position(? IN "+expression+")>0", append([]any{filter.Query}, args...)...)
	}
	if after != nil {
		query = query.Where("(created_at,id)<(?,?)", after.CreatedAt, after.ID)
	}
	var rows []models.RoutineModel
	if err = query.Order("created_at DESC,id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out = make([]*routine.Routine, 0, len(rows))
	for _, row := range rows {
		value, err := routineToDomain(&row)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}
