//go:build sidecar

package crypto

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// GMKEscrowClient habla con el vault del cloud (POST /api/v1/sync/gmk).
// localGMK nil = sólo consulta. Devuelve la GMK CANÓNICA del gym (que puede
// no ser la enviada, si otro sidecar ganó la carrera) o ErrNoEscrowedGMK si
// el cloud no tiene y no se ofreció ninguna.
type GMKEscrowClient interface {
	Ensure(ctx context.Context, gymID uuid.UUID, localGMK []byte) ([]byte, error)
}

// EscrowGMKProvider implementa ADR-006 §2.2/§2.6 sobre el KeyringGMKProvider:
//
//   - PC con llave local (keyring): opera 100% OFFLINE con ella, igual que
//     siempre — y en background, UNA vez por arranque, la sube al vault
//     ("adopción": así las llaves generadas antes del escrow entran solas).
//   - PC sin llave (pareo nuevo, reinstalación con keyring perdido): pide la
//     canónica al cloud; si el cloud tampoco tiene, genera una y la ofrece
//     (el server es first-wins y devuelve la que quedó). Este camino SÍ
//     requiere internet — el mismo momento que ya lo requería (el primer
//     handshake); sin conexión la biometría queda "pendiente" en lugar de
//     generar una llave huérfana que vuelve indescifrable todo lo sincronizado.
//   - Divergencia (el vault tiene una llave ≠ a la local — p.ej. dos PCs que
//     generaron por su cuenta antes del escrow): la del cloud GANA (misma
//     postura que password_hash: cloud = autoridad). Se adopta, se loggea
//     fuerte, y las huellas cifradas con la llave perdedora requieren
//     re-enroll — ya eran indescifrables para el resto del gym de todos modos.
type EscrowGMKProvider struct {
	Local  *KeyringGMKProvider
	Client GMKEscrowClient // nil = sin cloud (tests/dev) → passthrough al Local
	Logger *log.Logger

	mu        sync.Mutex
	attempted map[uuid.UUID]bool // adopción: un intento por gym por proceso
}

func NewEscrowGMKProvider(local *KeyringGMKProvider, client GMKEscrowClient) *EscrowGMKProvider {
	return &EscrowGMKProvider{
		Local:     local,
		Client:    client,
		Logger:    log.Default(),
		attempted: make(map[uuid.UUID]bool),
	}
}

func (p *EscrowGMKProvider) GetGMK(ctx context.Context, gymID uuid.UUID) ([]byte, error) {
	if gymID == uuid.Nil {
		return nil, ErrGMKNotFound
	}
	if p.Client == nil {
		return p.Local.GetGMK(ctx, gymID)
	}

	if local, ok := p.Local.Lookup(gymID); ok {
		// Camino caliente (cada check-in descifra la galería): jamás
		// bloquea en red. La adopción corre aparte, una vez por arranque.
		go p.adoptOnce(gymID, local)
		return local, nil
	}

	// Sin llave local: serializar a los concurrentes para no disparar N
	// round-trips (y N generaciones) en el arranque de un pareo nuevo.
	p.mu.Lock()
	defer p.mu.Unlock()
	if local, ok := p.Local.Lookup(gymID); ok {
		return local, nil
	}

	canonical, err := p.Client.Ensure(ctx, gymID, nil)
	if errors.Is(err, ErrNoEscrowedGMK) {
		// Gym sin llave en ningún lado: generar y OFRECER — lo que el
		// server confirme (first-wins) es lo que se adopta.
		fresh := make([]byte, GMKSize)
		if _, rerr := rand.Read(fresh); rerr != nil {
			return nil, fmt.Errorf("generate gmk for gym %s: %w", gymID, rerr)
		}
		canonical, err = p.Client.Ensure(ctx, gymID, fresh)
	}
	if err != nil {
		return nil, fmt.Errorf("GMK no disponible: esta PC no tiene llave local y el vault no respondió (se requiere internet para el primer pareo / recuperación): %w", err)
	}
	if aerr := p.Local.Adopt(gymID, canonical); aerr != nil {
		return nil, aerr
	}
	// Ya está en el vault por construcción. Escritura directa: p.mu ya se
	// sostiene en este camino (tomarlo de nuevo era un self-deadlock).
	p.attempted[gymID] = true
	p.Logger.Printf("[biometric] GMK del gym %s recuperada del vault y persistida en el keyring", gymID)
	return canonical, nil
}

// Forget espeja el contrato del provider local (logout/unpair) y rearma la
// adopción para el próximo pareo.
func (p *EscrowGMKProvider) Forget(gymID uuid.UUID) {
	p.Local.Forget(gymID)
	p.mu.Lock()
	delete(p.attempted, gymID)
	p.mu.Unlock()
}

// adoptOnce sube la llave local al vault una sola vez por proceso. Fallos se
// loggean y se reintenta hasta el siguiente arranque — la operación local no
// depende de esto (la llave del keyring sigue mandando para descifrar).
func (p *EscrowGMKProvider) adoptOnce(gymID uuid.UUID, local []byte) {
	p.mu.Lock()
	if p.attempted[gymID] {
		p.mu.Unlock()
		return
	}
	p.attempted[gymID] = true
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	canonical, err := p.Client.Ensure(ctx, gymID, local)
	if err != nil {
		p.Logger.Printf("[biometric] adopción de GMK al vault falló (gym %s, reintento al próximo arranque): %v", gymID, err)
		p.mu.Lock()
		delete(p.attempted, gymID) // que un GetGMK posterior de este proceso pueda reintentar
		p.mu.Unlock()
		return
	}
	if bytesEqualConst(canonical, local) {
		return // adoptada o ya coincidía — nada que hacer
	}
	// Divergencia: otra PC del gym llegó primero al vault. Cloud = autoridad.
	if aerr := p.Local.Adopt(gymID, canonical); aerr != nil {
		p.Logger.Printf("[biometric] GMK divergente y no pude adoptar la canónica (gym %s): %v", gymID, aerr)
		return
	}
	p.Logger.Printf("[biometric] GMK DIVERGENTE en gym %s: se adoptó la canónica del vault. Las huellas enroladas en esta PC con la llave anterior dejan de descifrar y requieren re-enroll (ya eran ilegibles para las demás PCs del gym).", gymID)
}

func bytesEqualConst(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
