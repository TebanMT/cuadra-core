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

import (
	"log"
	"sync"
	"time"

	// La base IANA embebida en el binario. NO QUITAR.
	//
	// Va aquí y no en los main.go a propósito: este paquete no sirve de
	// nada sin ella, así que la dependencia vive donde es intrínseca y
	// ningún binario futuro puede olvidarla.
	//
	// En Windows `time.LoadLocation` NO tiene de dónde leer: el stdlib
	// declara `platformZoneSources` vacío ("none: Windows uses system
	// calls instead") y esas syscalls sólo alimentan a time.Local, no a
	// LoadLocation. Sin este import, en la PC de recepción de cada gym
	// TODA zona horaria fallaba al cargar y LocationOrUTC caía a UTC en
	// silencio — es decir, el fix de fechas locales era letra muerta en
	// el sidecar y sólo funcionaba en el cloud (Linux, con
	// /usr/share/zoneinfo). Cuesta ~450 KB de binario.
	_ "time/tzdata"
)

// badZones recuerda qué nombres de zona ya reportamos para no inundar el
// log: LocationOrUTC se llama en caminos calientes (cada cobro, cada
// check-in, cada query de reportes).
var badZones sync.Map

// LocationOrUTC resuelve una zona horaria IANA ("America/Mexico_City").
// Nombre vacío o inválido cae a UTC — comportamiento previo, nunca rompe
// la operación por un dato de configuración.
//
// El fallback es intencionalmente fail-open, pero NO silencioso: un nombre
// que no carga se registra una vez. Un fail-open mudo fue justamente lo
// que dejó pasar meses el problema de la tzdata ausente en Windows.
func LocationOrUTC(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		if _, seen := badZones.LoadOrStore(name, true); !seen {
			log.Printf("[tz] zona horaria %q no se pudo cargar (%v) — usando UTC; "+
				"las fechas de negocio de este gym quedarán corridas", name, err)
		}
		return time.UTC
	}
	return loc
}

// Verify reporta si una zona horaria carga.
func Verify(name string) error {
	_, err := time.LoadLocation(name)
	return err
}

// WarnIfUnavailable es el chequeo de arranque de los binarios: prueba que
// la base de zonas horarias esté disponible y grita si no. Si la tzdata
// embebida se perdiera (alguien quita el import de arriba), esto lo
// delata en el boot en vez de dejar que reaparezca semanas después como
// fechas corridas en los reportes de un gym.
func WarnIfUnavailable() {
	const canary = "America/Mexico_City"
	if err := Verify(canary); err != nil {
		log.Printf("WARN ⚠️ [tz] la base de zonas horarias NO está disponible (%v) — "+
			"toda fecha de negocio caerá a UTC y quedará corrida; "+
			"revisa el import de time/tzdata en src/shared/tz", err)
	}
}

// NameOrUTC devuelve el nombre de zona listo para mandarlo a Postgres en
// un `AT TIME ZONE ?`, o "UTC" si no carga.
//
// Postgres resuelve las zonas con su PROPIO catálogo, así que un nombre
// basura ahí no cae a UTC: revienta el query. Validarlo antes contra la
// tzdata de Go mantiene el mismo fail-open que el resto del paquete —
// números corridos son un bug, una página de reportes en 500 es peor.
func NameOrUTC(name string) string {
	if name == "" {
		return "UTC"
	}
	if _, err := time.LoadLocation(name); err != nil {
		return "UTC"
	}
	return name
}

// LocalToday devuelve el día calendario de `now` visto desde tzName,
// expresado como medianoche UTC (la convención date-only del storage).
func LocalToday(tzName string, now time.Time) time.Time {
	local := now.In(LocationOrUTC(tzName))
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

// DayBounds traduce un rango de DÍAS locales del gym al rango de INSTANTES
// UTC que le corresponde: [inicio, fin) con fin exclusivo.
//
// fromDay/toDay llegan en la convención date-only (medianoche UTC) y ambos
// extremos son inclusivos como días. El resultado sirve para filtrar
// columnas de timestamp (checkin_at, created_at) sin envolver la columna en
// una expresión — o sea, sin perder el índice. Ese es el punto: comparar
// `checkin_at >= X AND checkin_at < Y` usa idx_checkins_gym_date, mientras
// que `checkin_at::date >= ?` obliga a un scan.
//
// Ejemplo en CDMX (UTC-6): el día local 2026-07-27 es el rango
// [2026-07-27 06:00Z, 2026-07-28 06:00Z).
//
// La medianoche se construye con time.Date sobre la Location, no sumando
// un offset fijo, para que el horario de verano quede correcto solo.
func DayBounds(tzName string, fromDay, toDay time.Time) (time.Time, time.Time) {
	loc := LocationOrUTC(tzName)
	start := time.Date(fromDay.Year(), fromDay.Month(), fromDay.Day(), 0, 0, 0, 0, loc)
	endDay := toDay.AddDate(0, 0, 1)
	end := time.Date(endDay.Year(), endDay.Month(), endDay.Day(), 0, 0, 0, 0, loc)
	return start.UTC(), end.UTC()
}

// OffsetSeconds devuelve el desplazamiento de la zona respecto de UTC, en
// segundos, vigente en el instante `at` (CDMX → -21600).
//
// Existe para SQLite, que no trae base de zonas horarias: el agrupado por
// día local se hace desplazando el epoch antes de truncar
// (`date((checkin_at/1000) + ?, 'unixepoch')`). Al depender de un instante
// concreto, un rango que cruce un cambio de horario de verano queda con el
// offset del inicio — irrelevante en México, que ya no lo usa.
func OffsetSeconds(tzName string, at time.Time) int {
	_, off := at.In(LocationOrUTC(tzName)).Zone()
	return off
}
