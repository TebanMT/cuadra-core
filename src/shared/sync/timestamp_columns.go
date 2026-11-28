package sync

import "strings"

// Catálogo compartido (cloud + sidecar, sin build tag) de qué columnas son
// instantes. El projector (cloud) lo usa para convertir epoch-ms → time.Time;
// el apply del sidecar, para convertir strings RFC3339 → epoch-ms.

// timestamptzColumns lists the extra columns whose Postgres type is
// TIMESTAMPTZ but whose name does NOT end in `_at`, so the suffix
// heuristic in isTimestampColumn would miss them. Without an entry here,
// epoch-ms ints from the sidecar payload reach pgx as raw float64 and the
// upsert dies with "cannot find encode plan for OID 1184".
//
// Discover new entries by grepping `db_migrations/postgres/` for
// `TIMESTAMPTZ` and filtering out names ending in `_at`.
//
// El sidecar usa el MISMO catálogo en el apply del pull (extractColumnValue):
// SQLite guarda epoch-ms y un string RFC3339 escrito tal cual en la columna
// INTEGER rompe todo Scan posterior (scheduled_for de recordatorios del
// cloud → 500 en el historial de WhatsApp, nov-2026; reparado por la
// migración SQLite 052).
var timestamptzColumns = map[string]map[string]bool{
	"challenges":         {"measurement_t0_deadline": true, "measurement_t1_start": true},
	"notification_queue": {"scheduled_for": true},
	// subscription_events tiene tres TIMESTAMPTZ que no terminan en `_at`-
	// del-record-estándar: occurred_at sí matchea, pero period_ends_at va
	// con sufijo distinto y recorded_at es semántica BE — los tres llegan
	// como epoch-ms del sidecar (en realidad sólo cloud → sidecar pull,
	// pero declararlos hace simétrico el wire shape si algún día cambia).
	"subscription_events": {"period_ends_at": true, "occurred_at": true, "recorded_at": true},
}

// isTimestampColumn returns true for columns whose Postgres type is
// timestamptz and whose payload value may arrive as an epoch-ms number.
//
// La mayoría de columnas timestamp del schema terminan en `_at`, así que
// esa es la heurística rápida. Para las que no (medición t0/t1 de retos,
// scheduled_for de notification_queue), el override por tabla en
// timestamptzColumns las atrapa. Las columnas tipo `_date` / `birthdate`
// llegan como strings ISO y Postgres las parsea sin coerción.
func isTimestampColumn(table, name string) bool {
	if strings.HasSuffix(name, "_at") {
		return true
	}
	if cols := timestamptzColumns[table]; cols != nil {
		return cols[name]
	}
	return false
}
