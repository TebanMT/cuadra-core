// Package installerbootstrap implements the single-use codes the dashboard
// hands an operator after web signup so the desktop's first launch can
// exchange the code for a full session (ADR-008 §4.5 extension).
//
// Token shape: `cb_install_<32 url-safe random bytes>`. Plaintext is shown
// to the operator once; the cloud stores SHA-256(token).
package installerbootstrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const Prefix = "cb_install_"

// DefaultTTL is how long a freshly-minted token stays redeemable.
const DefaultTTL = 24 * time.Hour

// Generate returns the plaintext token + its SHA-256 hash. Caller stores the
// hash; the plaintext goes once to the operator (dashboard QR / installer
// argument).
func Generate() (string, []byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, fmt.Errorf("installerbootstrap: rand: %w", err)
	}
	suffix := base64.RawURLEncoding.EncodeToString(raw[:])
	plain := Prefix + suffix
	h := sha256.Sum256([]byte(plain))
	return plain, h[:], nil
}

// Hash returns SHA-256(token). Used by the redeem path to look up by hash.
func Hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// HasPrefix reports whether the value starts with the canonical prefix —
// useful for routing logic that needs to discriminate before doing a DB
// round-trip.
func HasPrefix(token string) bool { return strings.HasPrefix(token, Prefix) }

// Bootstrap is one row of the installer_bootstraps table.
type Bootstrap struct {
	ID         uuid.UUID
	GymID      uuid.UUID
	UserID     uuid.UUID
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RedeemedAt *time.Time
}

// Store is the persistence contract — declared here (no build tag) so
// callers in both binaries can type-check against it. The Postgres impl
// lives in store.go behind the `server` tag.
type Store interface {
	Insert(ctx context.Context, gymID, userID uuid.UUID, hash []byte, expiresAt time.Time) (Bootstrap, error)
	Redeem(ctx context.Context, hash []byte, now time.Time) (Bootstrap, error)
	// ExpirePriorActive forces every unredeemed, not-yet-expired token
	// for (gym, user) into "expired" state. Lo invoca el use case de
	// emisión antes de insertar un nuevo código, de modo que generar uno
	// nuevo invalide al anterior — el dueño espera que "Regenerar"
	// signifique "el anterior ya no sirve", no "ahora tengo dos válidos".
	// Devuelve el número de filas afectadas (útil para logging/tests).
	ExpirePriorActive(ctx context.Context, gymID, userID uuid.UUID, now time.Time) (int64, error)
}

var (
	// ErrNotFound — the token hash isn't in the table at all.
	ErrNotFound = errors.New("installerbootstrap: token not found")
	// ErrAlreadyRedeemed — the token was already exchanged once.
	ErrAlreadyRedeemed = errors.New("installerbootstrap: token already redeemed")
	// ErrExpired — past expires_at.
	ErrExpired = errors.New("installerbootstrap: token expired")
)
