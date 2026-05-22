// Package runtime expone el modo de ejecución del binario (dev/test/production),
// leído de la env var TINTA_MODE.
//
// El consumidor canónico es gymDomain.CanAccessPlusFeatures, que en modo dev
// devuelve true para cualquier plan — desbloquea features Plus localmente sin
// tocar la BD. Fuera de ese helper NO se debe consultar IsDev() para alterar
// lógica de billing real: el estado de la suscripción (trial/standard/plus,
// past_due, etc.) debe seguir siendo honesto incluso en dev, sólo los gates
// de feature se relajan.
//
// Fail-closed: cualquier valor inesperado (o env var ausente) cae a production.
package runtime

import (
	"os"
	"strings"
	"sync"
	"testing"
)

const (
	ModeDev        = "dev"
	ModeTest       = "test"
	ModeProduction = "production"

	envVar = "TINTA_MODE"
)

// override permite que SetForTest fije el modo durante un test sin depender
// de la env var del proceso. Cuando override != "" gana sobre TINTA_MODE.
var (
	overrideMu sync.RWMutex
	override   string
)

// Current devuelve el modo activo, normalizado. Lee TINTA_MODE cada vez
// (no se cachea — el coste es despreciable y el cache complica los tests).
// SetForTest sobreescribe el resultado dentro de un test.
func Current() string {
	overrideMu.RLock()
	o := override
	overrideMu.RUnlock()
	if o != "" {
		return o
	}
	return normalize(os.Getenv(envVar))
}

// IsDev es el atajo usado por los gates: en modo dev se relajan; en test y
// production se respetan.
func IsDev() bool {
	return Current() == ModeDev
}

// SetForTest fija el modo para la duración del test y lo revierte con
// t.Cleanup. Tomar *testing.T hace que llamarlo desde código de producción
// no compile.
func SetForTest(t *testing.T, mode string) {
	t.Helper()
	overrideMu.Lock()
	prev := override
	override = normalize(mode)
	overrideMu.Unlock()
	t.Cleanup(func() {
		overrideMu.Lock()
		override = prev
		overrideMu.Unlock()
	})
}

func normalize(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "dev", "development":
		return ModeDev
	case "test":
		return ModeTest
	case "production", "prod":
		return ModeProduction
	default:
		return ModeProduction
	}
}
