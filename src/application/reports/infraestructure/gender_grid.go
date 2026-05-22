// Build-tag-less helpers compartidos entre los drivers postgres/sqlite del
// Reader. Sólo viven aquí los pedazos puros (sin tx, sin SQL) — el resto
// queda con su tag para que el linker no arrastre cgo de sqlite al binario
// del server o GORM al sidecar.

package infraestructure

import "github.com/cuadra/cuadra-core/src/application/reports"

// hourBucketRow es el shape intermedio que ambos drivers producen luego del
// agregado. Se define una sola vez para que fillHourlyGenderGrid lo pueda
// consumir sin trucos de interface{}.
type hourBucketRow struct {
	Hour           int
	Hombre         int
	Mujer          int
	NoEspecificado int
}

// fillHourlyGenderGrid genera las 24 filas de salida (0..23) rellenando con
// 0 los huecos del agregado SQL. Compartida entre los dos drivers porque la
// shape es idéntica; el FE depende de las 24 filas para renderear la grilla
// completa sin reconciliar gaps.
func fillHourlyGenderGrid(in []hourBucketRow) []reports.AttendanceByGenderHourRow {
	byHour := make(map[int]hourBucketRow, len(in))
	for _, r := range in {
		byHour[r.Hour] = r
	}
	out := make([]reports.AttendanceByGenderHourRow, 24)
	for h := 0; h < 24; h++ {
		v := byHour[h]
		out[h] = reports.AttendanceByGenderHourRow{
			Hour: h, Hombre: v.Hombre, Mujer: v.Mujer, NoEspecificado: v.NoEspecificado,
		}
	}
	return out
}
