package routine

import (
	"strings"
	"time"
	"unicode/utf8"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/search"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

type ListFilter struct{ Status, Query string }
type ListPosition struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

func (p ListPosition) Validate() error {
	if !protocol.ValidID(p.ID) || p.CreatedAt.IsZero() || p.CreatedAt.Year() < 1 || p.CreatedAt.Year() > 9999 {
		return protocol.ErrCursorInvalid
	}
	return nil
}

func NormalizeListFilter(status, query string) (ListFilter, error) {
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "archived" && status != "all" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 120 || strings.ContainsRune(query, 0) {
		return ListFilter{}, trainingErrors.ErrInvalidRoutine
	}
	return ListFilter{Status: status, Query: NormalizeSearch(query)}, nil
}

// Wrappers preserve routine search semantics and the existing callers.
func NormalizeSearch(value string) string { return search.Normalize(value) }
func FoldSearchCase(value string) string  { return search.FoldCase(value) }
