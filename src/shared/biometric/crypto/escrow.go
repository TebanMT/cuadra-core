package crypto

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Escrow de la GMK (ADR-006 §2.2 / §2.6, "managed key per gym").
//
// El cloud guarda cada GMK cifrada con la SMK (Server Master Key) en la
// tabla `gym_keys` (existe en el schema postgres desde el día 1; este
// archivo es el cableado que faltaba). El sidecar la sube al primer
// contacto ("adopción" de llaves generadas localmente antes del escrow) y
// la descarga al parear un equipo nuevo o al reinstalar con el keyring
// perdido — sin esto, las filas de member_fingerprints sincronizadas eran
// blobs indescifrables en cualquier PC distinta a la que enroló (visto en
// campo: "el socio ya tiene huella" pero nada reconoce tras reinstalar).
//
// La GMK viaja en claro SOLO sobre TLS y autenticada con la credencial del
// pareo (sk_live_*); nunca via el sync genérico (gym_keys NO está en
// SyncedTables a propósito).

// ErrNoEscrowedGMK — el cloud no tiene GMK para este gym todavía (gym
// anterior al escrow cuyo sidecar aún no adopta, o gym recién creado sin
// enrolamientos). El caller decide: subir la local o generar una nueva.
var ErrNoEscrowedGMK = errors.New("el cloud no tiene GMK para este gym")

// ParseSMK decodifica TINTA_SMK_BASE64 (32 bytes en base64 — `openssl rand
// -base64 32`). "" devuelve (nil, nil): escrow deshabilitado, fail-closed en
// los endpoints — el cloud NUNCA genera una SMK sola (perderla = perder
// todas las GMKs escrowed; su ciclo de vida es del operador de infra).
func ParseSMK(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("TINTA_SMK_BASE64 no es base64 válido: %w", err)
	}
	if len(raw) != GMKSize {
		return nil, fmt.Errorf("TINTA_SMK_BASE64 debe decodificar a %d bytes, tiene %d", GMKSize, len(raw))
	}
	return raw, nil
}

// WrapGMK cifra una GMK bajo la SMK para almacenarla en gym_keys. Mismo
// primitivo AES-256-GCM (y mismo wire format versionado) que los templates —
// una GMK es un "plaintext" de 32 bytes como cualquier otro.
func WrapGMK(smk, gmk []byte) ([]byte, error) {
	if len(gmk) != GMKSize {
		return nil, ErrInvalidGMK
	}
	return EncryptTemplate(smk, gmk)
}

// UnwrapGMK descifra el blob de gym_keys.encrypted_gmk.
func UnwrapGMK(smk, blob []byte) ([]byte, error) {
	gmk, err := DecryptTemplate(smk, blob)
	if err != nil {
		return nil, err
	}
	if len(gmk) != GMKSize {
		return nil, fmt.Errorf("GMK escrowed con longitud inválida (%d != %d)", len(gmk), GMKSize)
	}
	return gmk, nil
}
