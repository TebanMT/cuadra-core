package security

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalSecurity "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	personalIssuer      = "tinta-personal"
	personalAudience    = "mancuerna"
	maxAccessTokenSize  = 512 // S00 Secret used by Session.access_token.
	maxSigningKeyIDSize = 64
)

type AccessTokens struct {
	activeKeyID      string
	privateKey       ed25519.PrivateKey
	verificationKeys map[string]ed25519.PublicKey
}

var _ personalSecurity.AccessTokens = (*AccessTokens)(nil)

// NewAccessTokens copies the dedicated personal signing material. Old public
// keys can remain in verificationKeys for the access-token rotation window.
func NewAccessTokens(activeKeyID string, privateKey ed25519.PrivateKey, verificationKeys map[string]ed25519.PublicKey) (*AccessTokens, error) {
	if !validSigningKeyID(activeKeyID) || len(privateKey) != ed25519.PrivateKeySize {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	if !bytes.Equal(privateKey, ed25519.NewKeyFromSeed(privateKey[:ed25519.SeedSize])) {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	keys := make(map[string]ed25519.PublicKey, len(verificationKeys))
	for id, key := range verificationKeys {
		if !validSigningKeyID(id) || len(key) != ed25519.PublicKeySize {
			return nil, personalErrors.ErrInvalidSecurityConfiguration
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	if !bytes.Equal(keys[activeKeyID], privateKey.Public().(ed25519.PublicKey)) {
		return nil, personalErrors.ErrInvalidSecurityConfiguration
	}
	return &AccessTokens{activeKeyID: activeKeyID, privateKey: append(ed25519.PrivateKey(nil), privateKey...), verificationKeys: keys}, nil
}

// JWT key IDs have a smaller bound than persisted wrapping/HMAC key versions:
// they travel in every token, which must fit the contract's 512-byte Secret.
// The URL-safe ASCII alphabet also prevents JSON escaping from inflating it.
func validSigningKeyID(id string) bool {
	if id == "" || len(id) > maxSigningKeyIDSize {
		return false
	}
	for i := range id {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

type accessClaims struct {
	SessionID string `json:"sid"`
	DeviceID  string `json:"did"`
	jwt.RegisteredClaims
}

func (s *AccessTokens) Issue(accountID, sessionID, deviceID uuid.UUID, now time.Time) (string, personalSecurity.AccessClaims, error) {
	if !validID(accountID) || !validID(sessionID) || !validID(deviceID) || now.IsZero() {
		return "", personalSecurity.AccessClaims{}, personalErrors.ErrInvalidAccessToken
	}
	now = now.UTC().Truncate(time.Second)
	claims := accessClaims{SessionID: sessionID.String(), DeviceID: deviceID.String(), RegisteredClaims: jwt.RegisteredClaims{
		Issuer: personalIssuer, Subject: accountID.String(), Audience: jwt.ClaimStrings{personalAudience},
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(personalSecurity.AccessTTL)),
	}}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = s.activeKeyID
	encoded, err := token.SignedString(s.privateKey)
	if err != nil || len(encoded) > maxAccessTokenSize {
		return "", personalSecurity.AccessClaims{}, personalErrors.ErrInvalidAccessToken
	}
	return encoded, personalSecurity.AccessClaims{AccountID: accountID, SessionID: sessionID, DeviceID: deviceID, IssuedAt: now, ExpiresAt: claims.ExpiresAt.Time}, nil
}

func (s *AccessTokens) Verify(encoded string, now time.Time) (personalSecurity.AccessClaims, error) {
	invalid := personalErrors.ErrInvalidAccessToken
	if now.IsZero() || len(encoded) > maxAccessTokenSize || !unambiguousJWT(encoded) {
		return personalSecurity.AccessClaims{}, invalid
	}
	claims := &accessClaims{}
	token, err := jwt.ParseWithClaims(encoded, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodEdDSA {
			return nil, invalid
		}
		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, invalid
		}
		key, ok := s.verificationKeys[kid]
		if !ok {
			return nil, invalid
		}
		return key, nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer(personalIssuer), jwt.WithAudience(personalAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(personalSecurity.AccessClockSkew), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || token == nil || !token.Valid || len(claims.Audience) != 1 || claims.Audience[0] != personalAudience || claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return personalSecurity.AccessClaims{}, invalid
	}
	ttl := claims.ExpiresAt.Sub(claims.IssuedAt.Time)
	if ttl <= 0 || ttl > personalSecurity.AccessTTL {
		return personalSecurity.AccessClaims{}, invalid
	}
	accountID, aerr := parseID(claims.Subject)
	sessionID, serr := parseID(claims.SessionID)
	deviceID, derr := parseID(claims.DeviceID)
	if aerr != nil || serr != nil || derr != nil {
		return personalSecurity.AccessClaims{}, invalid
	}
	return personalSecurity.AccessClaims{AccountID: accountID, SessionID: sessionID, DeviceID: deviceID, IssuedAt: claims.IssuedAt.Time.UTC(), ExpiresAt: claims.ExpiresAt.Time.UTC()}, nil
}

// Duplicate JSON members make a signature ambiguous across parsers. Reject them
// before the JWT library decodes either the header or the claims.
func unambiguousJWT(encoded string) bool {
	parts := strings.Split(encoded, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts[:2] {
		decoded, err := rawURL.DecodeString(part)
		if err != nil {
			return false
		}
		value, err := parseJSON(decoded)
		if err != nil {
			return false
		}
		if _, ok := value.(map[string]any); !ok {
			return false
		}
	}
	return true
}
