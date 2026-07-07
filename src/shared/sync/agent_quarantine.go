//go:build sidecar

package sync

import (
	"context"
	"errors"
	"time"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// quarantineThreshold — cuántas veces debe fallar la MISMA fila al
// aplicarse antes de saltarla. >1 para no cuarentenar un blip transitorio
// (DB momentáneamente lockeada, etc.); el pull la reintenta en cada tick y
// sólo tras 3 fallos deterministas la da por veneno.
const quarantineThreshold = 3

// errProbeRollback fuerza el rollback de la tx de sondeo: aplicar una fila
// sola es sólo para VER si revienta, nunca para persistir.
var errProbeRollback = errors.New("probe rollback (sentinel)")

type poisonHit struct {
	change PullChange
	err    error
}

func quarantineKey(entityType, entityID string) string {
	return entityType + "\x00" + entityID
}

// probePoison aplica cada change AISLADO en su propia tx que SIEMPRE
// rollbackea, y reporta los que revientan con un error INMEDIATO (no-FK).
//
// Por qué sólo salen los no-FK: con defer_foreign_keys=ON las FKs se
// validan en el COMMIT, y acá nunca commiteamos → una fila que sólo falla
// por su FK (le falta una compañera de la misma página) aplica limpio en
// el sondeo y NO se marca veneno. Los errores inmediatos (no such column,
// CHECK, NOT NULL) sí surgen al ejecutar el statement → esos son el veneno
// determinista que queremos saltar. Justo la distinción correcta.
func (a *Agent) probePoison(ctx context.Context, changes []PullChange) []poisonHit {
	var hits []poisonHit
	for _, ch := range changes {
		var applyErr error
		_ = a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
			stx := tx.(*sharedDomain.SqlxTransaction)
			if _, err := stx.Exec(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
				applyErr = err
				return errProbeRollback
			}
			applyErr = ApplyPullChange(ctx, tx, ch)
			return errProbeRollback // rollback SIEMPRE — sondeo, no persistencia
		})
		if applyErr != nil && !errors.Is(applyErr, errProbeRollback) {
			hits = append(hits, poisonHit{change: ch, err: applyErr})
		}
	}
	return hits
}

// recordQuarantineAttempts sube el contador de intentos de cada fila
// veneno y devuelve el conteo actualizado por llave. Si baja una version
// MAYOR que la registrada, resetea attempts a 1: el cloud cambió la fila,
// merece un intento fresco (así se auto-cura cuando el dato o el binario
// se corrigen y el projector bumpea la version).
func recordQuarantineAttempts(
	ctx context.Context,
	stx *sharedDomain.SqlxTransaction,
	hits []poisonHit,
	now time.Time,
) (map[string]int, error) {
	nowMs := now.UTC().UnixMilli()
	counts := make(map[string]int, len(hits))
	for _, h := range hits {
		errMsg := ""
		if h.err != nil {
			errMsg = h.err.Error()
		}
		if _, err := stx.Exec(ctx, `
			INSERT INTO sync_quarantine
			    (entity_type, entity_id, version, attempts, last_error, first_seen_at, last_seen_at)
			VALUES (?, ?, ?, 1, ?, ?, ?)
			ON CONFLICT(entity_type, entity_id) DO UPDATE SET
			    attempts = CASE
			        WHEN excluded.version > sync_quarantine.version THEN 1
			        ELSE sync_quarantine.attempts + 1
			    END,
			    version      = excluded.version,
			    last_error   = excluded.last_error,
			    last_seen_at = excluded.last_seen_at`,
			h.change.EntityType, h.change.EntityID, h.change.Version, errMsg, nowMs, nowMs,
		); err != nil {
			return nil, err
		}
		var n int
		if err := stx.Get(ctx, &n,
			`SELECT attempts FROM sync_quarantine WHERE entity_type = ? AND entity_id = ?`,
			h.change.EntityType, h.change.EntityID,
		); err != nil {
			return nil, err
		}
		counts[quarantineKey(h.change.EntityType, h.change.EntityID)] = n
	}
	return counts, nil
}

// clearQuarantineForApplied borra las filas de cuarentena de entidades que
// acaban de aplicarse bien (se curaron: el cloud subió la version con el
// dato corregido, o un binario nuevo ya sabe aplicarlas). Se llama tras un
// apply de página exitoso, y sólo cuando hay algo en cuarentena — el path
// sano (99.99% de installs, tabla vacía) no paga el costo.
func clearQuarantineForApplied(
	ctx context.Context,
	stx *sharedDomain.SqlxTransaction,
	changes []PullChange,
) error {
	for _, ch := range changes {
		if _, err := stx.Exec(ctx,
			`DELETE FROM sync_quarantine WHERE entity_type = ? AND entity_id = ?`,
			ch.EntityType, ch.EntityID,
		); err != nil {
			return err
		}
	}
	return nil
}

// countQuarantined cuenta las filas efectivamente saltadas (attempts al
// umbral o más). El estado de sync lo expone para que saltar NUNCA sea
// silencioso: el operador ve "hay un problema al guardar cambios" mientras
// haya algo en cuarentena, aunque el resto del sync fluya.
func countQuarantined(ctx context.Context, stx *sharedDomain.SqlxTransaction) (int, error) {
	var n int
	err := stx.Get(ctx, &n,
		`SELECT COUNT(*) FROM sync_quarantine WHERE attempts >= ?`, quarantineThreshold)
	return n, err
}

// quarantineAndRetry maneja un error de aplicación NO-FK de una página:
// sondea qué filas son veneno, sube su contador, y —si TODAS cruzaron el
// umbral— re-aplica la página SIN ellas y avanza el cursor vía tail.
//
// Devuelve (handled): true si la página se re-aplicó saltando el veneno
// (el cursor avanzó, sync sigue). false si aún no toca saltar (veneno por
// debajo del umbral, o el error no es aislable por fila) — el caller
// devuelve el error original y el tick reintenta.
func (a *Agent) quarantineAndRetry(
	ctx context.Context,
	batch []PullChange,
	tail func(tx sharedDomain.Transaction) error,
) (bool, error) {
	hits := a.probePoison(ctx, batch)
	if len(hits) == 0 {
		// El error no se reproduce fila-por-fila (combinacional o
		// transitorio). No hay nada que aislar — que reintente.
		return false, nil
	}

	var counts map[string]int
	if err := a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		var e error
		counts, e = recordQuarantineAttempts(ctx, stx, hits, time.Now().UTC())
		return e
	}); err != nil {
		return false, err
	}

	poison := make(map[string]bool, len(hits))
	allReady := true
	for _, h := range hits {
		k := quarantineKey(h.change.EntityType, h.change.EntityID)
		if counts[k] < quarantineThreshold {
			allReady = false
		}
		poison[k] = true
	}
	if !allReady {
		// Aún no: alguna fila no llegó al umbral. Reintentar en próximos
		// ticks — se cuentan solas hasta cruzarlo.
		return false, nil
	}

	// Todas listas para saltar: re-aplicar la página SIN el veneno y
	// avanzar el cursor hasta el final de la página (deja el veneno atrás).
	clean := make([]PullChange, 0, len(batch))
	for _, ch := range batch {
		if poison[quarantineKey(ch.EntityType, ch.EntityID)] {
			a.cfg.Logger.Printf("[sync] quarantine SKIP %s/%s (version %d): %s",
				ch.EntityType, ch.EntityID, ch.Version, quarantineErrFor(hits, ch))
			continue
		}
		clean = append(clean, ch)
	}
	if err := ApplyPullPage(ctx, a.uow, clean, tail); err != nil {
		// Aún falla sin el veneno (p.ej. saltamos un padre FK). Degrada al
		// comportamiento previo: devolver el error, no peor que hoy.
		return false, err
	}
	return true, nil
}

func quarantineErrFor(hits []poisonHit, ch PullChange) string {
	for _, h := range hits {
		if h.change.EntityType == ch.EntityType && h.change.EntityID == ch.EntityID {
			if h.err != nil {
				return h.err.Error()
			}
		}
	}
	return "?"
}

// refreshQuarantinedCount actualiza el conteo en memoria (lo lee el estado
// de sync). Best-effort.
func (a *Agent) refreshQuarantinedCount(ctx context.Context) {
	tx, err := a.uow.Query(ctx)
	if err != nil {
		return
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	n, err := countQuarantined(ctx, stx)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.state.QuarantinedCount = n
	a.mu.Unlock()
}
