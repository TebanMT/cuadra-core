// Package tz centraliza la regla "las fechas de negocio parten del día
// calendario del GYM en su zona horaria, no del día UTC".
//
// Por qué existe: el servidor y el wire trabajan en UTC (correcto para
// timestamps), pero las FECHAS de negocio — día del cobro, vigencias de
// membresía, etapas de recordatorio — son calendario local del gym. En
// CDMX (UTC-6) la medianoche UTC cae a las 6 PM: un cobro a las 10 PM del
// día 2 truncado en UTC caía en el día 3 → vencimiento corrido un día y
// venta en la caja del día equivocado. Mismo bug que ya mordió a los
// check-ins (gymLocalToday en checkins/app/services.go, que es el patrón
// que este paquete generaliza).
//
// Convención de salida: el día local expresado como MEDIANOCHE UTC —
// idéntico a como se almacenan todas las columnas date-only (DATE en
// Postgres, "YYYY-MM-DD" en SQLite).
package tz

import "time"

// LocationOrUTC resuelve una zona horaria IANA ("America/Mexico_City").
// Nombre vacío o inválido cae a UTC — comportamiento previo, nunca rompe
// la operación por un dato de configuración.
func LocationOrUTC(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// LocalToday devuelve el día calendario de `now` visto desde tzName,
// expresado como medianoche UTC (la convención date-only del storage).
func LocalToday(tzName string, now time.Time) time.Time {
	local := now.In(LocationOrUTC(tzName))
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}
