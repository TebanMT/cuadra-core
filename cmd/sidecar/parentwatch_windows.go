//go:build sidecar && windows

package main

import (
	"time"

	"golang.org/x/sys/windows"
)

// waitForParentExit bloquea hasta que el proceso padre original muere.
//
// Windows NO re-parenta huérfanos: os.Getppid() devuelve el PID viejo
// congelado aunque el padre haya muerto, así que el poll de PPID que
// usa la variante Unix acá es ciego (el bug que dejaba sidecars
// huérfanos eternos tras un auto-update). Lo correcto en Windows:
// abrir un handle SYNCHRONIZE al padre MIENTRAS sigue vivo (acaba de
// spawnearnos, así que no hay carrera de PID reuse) y esperar
// WaitForSingleObject — el kernel señala el handle al terminar el
// proceso, sin polling y sin ventana de reuse.
func waitForParentExit(originalParent int) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(originalParent))
	if err != nil {
		// Sin handle (política de seguridad rara) caemos a un poll de
		// existencia: OpenProcess con el mínimo de permisos falla con
		// ERROR_INVALID_PARAMETER cuando el PID ya no existe. Menos
		// preciso que el wait (PID reuse teórico), pero mejor que la
		// ceguera del Getppid.
		for {
			time.Sleep(2 * time.Second)
			ph, perr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(originalParent))
			if perr != nil {
				return
			}
			windows.CloseHandle(ph)
		}
	}
	defer windows.CloseHandle(h)
	windows.WaitForSingleObject(h, windows.INFINITE)
}
