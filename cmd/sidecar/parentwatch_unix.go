//go:build sidecar && !windows

package main

import (
	"os"
	"time"
)

// waitForParentExit bloquea hasta que el proceso padre original muere.
// En Unix el kernel re-parenta huérfanos a init/launchd, así que basta
// pollear os.Getppid() hasta que cambie. 2s: rápido para que el próximo
// arranque del desktop encuentre el puerto libre, lento para ser
// invisible en CPU.
func waitForParentExit(originalParent int) {
	for {
		time.Sleep(2 * time.Second)
		currentParent := os.Getppid()
		if currentParent != originalParent || currentParent <= 1 {
			return
		}
	}
}
