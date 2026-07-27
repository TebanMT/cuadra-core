//go:build server

package sync

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
)

// POST /api/v1/sync/gmk — "ensure" del escrow de GMK (ADR-006 §2.2/§2.6).
//
// Semántica FIRST-WINS, un solo round-trip, idempotente:
//
//   - El cloud YA tiene GMK para el gym → devuelve la canónica (ignora la
//     del request). Una llave por gym, para siempre — reescribirla dejaría
//     indescifrable toda huella cifrada con la anterior.
//   - No tiene y el request trae una → la guarda (adopción: así las llaves
//     generadas localmente ANTES del escrow entran al vault en el primer
//     contacto) y devuelve la que quedó — que puede ser la de un sidecar
//     rival que ganó la carrera (ON CONFLICT DO NOTHING + re-SELECT).
//   - No tiene y el request no trae → 404 no_gmk: el sidecar genera una y
//     re-llama con ella.
//
// Auth: el MISMO gate que el resto de /sync (sk_live_* del pareo u operator
// JWT); el gym sale del token, jamás del body. La GMK viaja en claro SOLO
// aquí (TLS) — gym_keys NO está en SyncedTables a propósito.
//
// Vive en el Handler del sync y no en un BC porque es protocolo técnico
// sidecar↔cloud, igual que push/pull — misma frontera de confianza, misma
// credencial, mismo registro de rutas.

type ensureGMKReq struct {
	// GMK local del sidecar en base64 (32 bytes). Omitida/vacía = sólo
	// consulta (el caller no tiene llave que ofrecer).
	GMK string `json:"gmk,omitempty"`
}

// EnsureGMK es el handler HTTP. Requiere WithSMK — sin SMK responde 503 y
// el sidecar conserva su llave local (feature apagada, nada se rompe).
func (h *Handler) EnsureGMK(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok || gymID == uuid.Nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if len(h.SMK) == 0 {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "smk_not_configured"})
		return
	}

	var req ensureGMKReq
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	var clientGMK []byte
	if req.GMK != "" {
		raw, err := base64.StdEncoding.DecodeString(req.GMK)
		if err != nil || len(raw) != bcrypto.GMKSize {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "gmk must be 32 bytes base64"})
			return
		}
		clientGMK = raw
	}

	var canonical []byte
	var adopted bool
	err := h.UoW.Command(c.Request.Context(), func(tx sharedDomain.Transaction) error {
		var err error
		canonical, adopted, err = ensureGMKTx(tx, h.SMK, gymID, clientGMK)
		return err
	})
	switch {
	case errors.Is(err, bcrypto.ErrNoEscrowedGMK):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "no_gmk"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"gmk":     base64.StdEncoding.EncodeToString(canonical),
		"adopted": adopted,
	})
}

// ensureGMKTx es la lógica first-wins dentro de una tx. Devuelve la GMK
// canónica y si este request fue el que la sembró.
func ensureGMKTx(tx sharedDomain.Transaction, smk []byte, gymID uuid.UUID, clientGMK []byte) ([]byte, bool, error) {
	g := gormTx(tx)

	readCanonical := func() ([]byte, error) {
		var blob []byte
		row := g.Raw(`SELECT encrypted_gmk FROM gym_keys WHERE gym_id = ?`, gymID).Row()
		if err := row.Scan(&blob); err != nil {
			return nil, err
		}
		return bcrypto.UnwrapGMK(smk, blob)
	}

	// Camino común: ya hay llave. (Scan de cero filas → sql.ErrNoRows,
	// que Row() reporta como error genérico; distinguimos con un COUNT
	// para no depender del texto del driver.)
	var n int64
	if err := g.Raw(`SELECT COUNT(1) FROM gym_keys WHERE gym_id = ?`, gymID).Scan(&n).Error; err != nil {
		return nil, false, fmt.Errorf("gym_keys lookup: %w", err)
	}
	if n > 0 {
		gmk, err := readCanonical()
		if err != nil {
			return nil, false, fmt.Errorf("gym_keys unwrap: %w", err)
		}
		return gmk, false, nil
	}

	if clientGMK == nil {
		return nil, false, bcrypto.ErrNoEscrowedGMK
	}

	blob, err := bcrypto.WrapGMK(smk, clientGMK)
	if err != nil {
		return nil, false, fmt.Errorf("gym_keys wrap: %w", err)
	}
	// FIRST-WINS: si otro sidecar del mismo gym insertó entre el COUNT y
	// aquí, el DO NOTHING pierde la carrera en silencio y el re-SELECT
	// devuelve la del ganador.
	if err := g.Exec(`
		INSERT INTO gym_keys (id, gym_id, encrypted_gmk, smk_version)
		VALUES (?, ?, ?, 1)
		ON CONFLICT (gym_id) DO NOTHING`,
		uuid.New(), gymID, blob,
	).Error; err != nil {
		return nil, false, fmt.Errorf("gym_keys insert: %w", err)
	}
	gmk, err := readCanonical()
	if err != nil {
		return nil, false, fmt.Errorf("gym_keys post-insert read: %w", err)
	}
	return gmk, bytesEqual(gmk, clientGMK), nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
