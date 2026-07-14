package repositories

import (
	"testing"

	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
)

// Pin del clamp de paginación. El bug histórico: page_size > 200 se
// reseteaba a 50 en vez de recortarse al cap — un caller que pedía "todos
// los activos" (venta rápida / buscador global) con page_size=500 recibía
// SÓLO 50 productos, y el resto quedaba invisible/no-vendible en gyms con
// más de 50 activos. La semántica correcta: de más → cap, no default.
func TestNormalizePage_ClampsToCapNotDefault(t *testing.T) {
	cases := []struct {
		name         string
		page         int
		pageSize     int
		wantPage     int
		wantPageSize int
	}{
		{"pedir de más recorta AL cap (no resetea a 50)", 1, 500, 1, prodRepo.MaxPageSize},
		{"exactamente el cap se respeta", 1, prodRepo.MaxPageSize, 1, prodRepo.MaxPageSize},
		{"justo por encima del cap → cap", 1, prodRepo.MaxPageSize + 1, 1, prodRepo.MaxPageSize},
		{"cero → default", 2, 0, 2, prodRepo.DefaultPageSize},
		{"negativo → default", 1, -10, 1, prodRepo.DefaultPageSize},
		{"un valor válido intermedio pasa tal cual", 3, 75, 3, 75},
		{"page < 1 → 1", 0, 50, 1, 50},
		{"page negativo → 1", -5, 50, 1, 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPage, gotPageSize := normalizePage(c.page, c.pageSize)
			if gotPage != c.wantPage {
				t.Errorf("page = %d, want %d", gotPage, c.wantPage)
			}
			if gotPageSize != c.wantPageSize {
				t.Errorf("pageSize = %d, want %d", gotPageSize, c.wantPageSize)
			}
		})
	}
}
