package repositories

import prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"

// normalizePage clamps page (≥1) and pageSize — shared between the Postgres and
// SQLite implementations. Lives in a no-build-tag file so both compile against
// the same definition. Semántica de clamp idéntica al use case (misma fuente,
// prodRepo.MaxPageSize): pedir de más se recorta AL cap, nunca al default —
// así este último candado no re-oculta lo que el use case ya dejó pasar.
func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = prodRepo.DefaultPageSize
	}
	if pageSize > prodRepo.MaxPageSize {
		pageSize = prodRepo.MaxPageSize
	}
	return page, pageSize
}
