//go:build sidecar

package crypto

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"
)

// fakeVault implementa GMKEscrowClient con la misma semántica first-wins del
// server real. Down=true simula sin internet.
type fakeVault struct {
	mu    sync.Mutex
	keys  map[uuid.UUID][]byte
	Down  bool
	calls chan struct{} // señal por llamada, para sincronizar la adopción bg
}

func newFakeVault() *fakeVault {
	return &fakeVault{keys: map[uuid.UUID][]byte{}, calls: make(chan struct{}, 16)}
}

func (f *fakeVault) Ensure(_ context.Context, gymID uuid.UUID, local []byte) ([]byte, error) {
	defer func() { f.calls <- struct{}{} }()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Down {
		return nil, errors.New("network is down")
	}
	if k, ok := f.keys[gymID]; ok {
		out := make([]byte, len(k))
		copy(out, k)
		return out, nil
	}
	if local == nil {
		return nil, ErrNoEscrowedGMK
	}
	stored := make([]byte, len(local))
	copy(stored, local)
	f.keys[gymID] = stored
	return local, nil
}

func newTestProvider(t *testing.T, vault *fakeVault) *EscrowGMKProvider {
	t.Helper()
	keyring.MockInit() // keyring en memoria, global por proceso — un gym por test
	return NewEscrowGMKProvider(NewKeyringGMKProvider(), vault)
}

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, GMKSize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func waitCall(t *testing.T, vault *fakeVault) {
	t.Helper()
	select {
	case <-vault.calls:
	case <-time.After(2 * time.Second):
		t.Fatalf("el vault no recibió la llamada esperada")
	}
}

// Reinstalación / pareo nuevo: sin llave local, el vault tiene la canónica →
// se recupera y persiste (las huellas sincronizadas vuelven a descifrar).
func TestEscrow_RecuperaDelVault(t *testing.T) {
	vault := newFakeVault()
	gym := uuid.New()
	canonical := randKey(t)
	vault.keys[gym] = canonical

	p := newTestProvider(t, vault)
	got, err := p.GetGMK(context.Background(), gym)
	if err != nil {
		t.Fatalf("GetGMK: %v", err)
	}
	if !bytesEq(got, canonical) {
		t.Errorf("debió devolver la canónica del vault")
	}
	if local, ok := p.Local.Lookup(gym); !ok || !bytesEq(local, canonical) {
		t.Errorf("la canónica debió persistirse en el keyring")
	}
}

// Gym nuevo sin llave en ningún lado: genera, ofrece, y adopta lo que el
// vault confirme.
func TestEscrow_GeneraYSube(t *testing.T) {
	vault := newFakeVault()
	gym := uuid.New()

	p := newTestProvider(t, vault)
	got, err := p.GetGMK(context.Background(), gym)
	if err != nil {
		t.Fatalf("GetGMK: %v", err)
	}
	vault.mu.Lock()
	stored := vault.keys[gym]
	vault.mu.Unlock()
	if !bytesEq(got, stored) {
		t.Errorf("la llave devuelta debe ser la que quedó en el vault")
	}
	// Estable en llamadas siguientes.
	again, err := p.GetGMK(context.Background(), gym)
	if err != nil || !bytesEq(again, got) {
		t.Errorf("la llave debe ser estable: %v", err)
	}
}

// Sin internet y sin llave local: error claro, NO se genera llave huérfana
// (generarla era el bug que el escrow arregla).
func TestEscrow_SinInternetSinLlave_Falla(t *testing.T) {
	vault := newFakeVault()
	vault.Down = true
	gym := uuid.New()

	p := newTestProvider(t, vault)
	if _, err := p.GetGMK(context.Background(), gym); err == nil {
		t.Fatalf("sin internet y sin llave local debe fallar")
	}
	if _, ok := p.Local.Lookup(gym); ok {
		t.Errorf("NO debe quedar llave local generada a ciegas")
	}
}

// Con llave local, la operación diaria es 100% offline: el vault caído no
// estorba (la adopción falla en background y se reintenta después).
func TestEscrow_OfflineConLlaveLocal_Opera(t *testing.T) {
	vault := newFakeVault()
	gym := uuid.New()

	p := newTestProvider(t, vault)
	local := randKey(t)
	if err := p.Local.Adopt(gym, local); err != nil {
		t.Fatal(err)
	}
	vault.Down = true

	got, err := p.GetGMK(context.Background(), gym)
	if err != nil {
		t.Fatalf("con llave local el vault caído no debe estorbar: %v", err)
	}
	if !bytesEq(got, local) {
		t.Errorf("debe operar con la llave local")
	}
	waitCall(t, vault) // el intento de adopción ocurrió (y falló) en bg
}

// Adopción: la llave local pre-escrow entra al vault en el primer contacto.
func TestEscrow_AdoptaLlaveLocalAlVault(t *testing.T) {
	vault := newFakeVault()
	gym := uuid.New()

	p := newTestProvider(t, vault)
	local := randKey(t)
	if err := p.Local.Adopt(gym, local); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetGMK(context.Background(), gym); err != nil {
		t.Fatal(err)
	}
	waitCall(t, vault)
	vault.mu.Lock()
	stored := vault.keys[gym]
	vault.mu.Unlock()
	if !bytesEq(stored, local) {
		t.Errorf("la llave local debió quedar escrowed en el vault")
	}
}

// Divergencia (dos PCs generaron por su cuenta antes del escrow): la
// canónica del vault GANA y reemplaza la local — cloud = autoridad.
func TestEscrow_DivergenciaAdoptaCanonica(t *testing.T) {
	vault := newFakeVault()
	gym := uuid.New()
	canonical := randKey(t)
	vault.keys[gym] = canonical

	p := newTestProvider(t, vault)
	local := randKey(t) // ≠ canónica
	if err := p.Local.Adopt(gym, local); err != nil {
		t.Fatal(err)
	}

	// El hot path devuelve la local (no bloquea en red)…
	got, err := p.GetGMK(context.Background(), gym)
	if err != nil || !bytesEq(got, local) {
		t.Fatalf("hot path debe devolver la local sin bloquear: %v", err)
	}
	// …y la adopción en background la reemplaza por la canónica.
	waitCall(t, vault)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if k, ok := p.Local.Lookup(gym); ok && bytesEq(k, canonical) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("la canónica del vault debió reemplazar la local divergente")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Sin cliente (tests/dev sin cloud): passthrough al comportamiento local
// clásico — lazy-generate en el keyring.
func TestEscrow_SinClienteFallbackLocal(t *testing.T) {
	gym := uuid.New()
	keyring.MockInit()
	p := NewEscrowGMKProvider(NewKeyringGMKProvider(), nil)
	got, err := p.GetGMK(context.Background(), gym)
	if err != nil || len(got) != GMKSize {
		t.Fatalf("fallback local: %v", err)
	}
}
