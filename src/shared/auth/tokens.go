// Package auth holds shared crypto primitives: JWT issuance + bcrypt password
// hashing. It is consumed by users/app and the auth middleware. We keep it in
// shared/ rather than inside users/ because the middleware lives in shared/
// too and we'd otherwise have a domain→infra import cycle.
package auth

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Default token durations. Cloud server uses these as-is so the web
// dashboard sees a 15-min access window. Sidecar overrides via
// NewJWTServiceWithDurations to issue effectively-eternal tokens — the
// reception screen at the gym must never log the operator out
// involuntarily (CUADRA-SPEC offline-first principle).
const (
	AccessTokenDuration  = 15 * time.Minute
	RefreshTokenDuration = 30 * 24 * time.Hour
)

// Claims is the JWT body we mint and validate. UserID and GymID are stored as
// strings on the wire to keep the payload deterministic and round-trip cleanly
// through any JSON middleware.
type Claims struct {
	UserID    uuid.UUID
	GymID     uuid.UUID
	Role      string
	TokenType string // "access" | "refresh"
	IssuedAt  time.Time
	ExpiresAt time.Time
}

type rawClaims struct {
	UserID    string `json:"user_id"`
	GymID     string `json:"gym_id"`
	Role      string `json:"role"`
	TokenType string `json:"token_type"`
	jwt.RegisteredClaims
}

// TokenService is what use cases and middleware depend on. JWTService is the
// production impl; tests can fake it.
type TokenService interface {
	GenerateAccessToken(userID, gymID uuid.UUID, role string) (string, error)
	GenerateRefreshToken(userID, gymID uuid.UUID, role string) (string, error)
	ValidateAccessToken(token string) (Claims, error)
	ValidateRefreshToken(token string) (Claims, error)
	HashRefreshToken(token string) ([]byte, error)
}

type JWTService struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func NewJWTService(secret string) *JWTService {
	return NewJWTServiceWithDurations(secret, AccessTokenDuration, RefreshTokenDuration)
}

// NewJWTServiceWithDurations returns a JWTService that mints tokens with the
// specified TTLs. Used by the sidecar to issue long-lived credentials so the
// reception screen never sees an involuntary logout.
func NewJWTServiceWithDurations(secret string, access, refresh time.Duration) *JWTService {
	if access <= 0 {
		access = AccessTokenDuration
	}
	if refresh <= 0 {
		refresh = RefreshTokenDuration
	}
	return &JWTService{secret: []byte(secret), accessTTL: access, refreshTTL: refresh}
}

func (s *JWTService) GenerateAccessToken(userID, gymID uuid.UUID, role string) (string, error) {
	return s.generate(userID, gymID, role, "access", s.accessTTL)
}

func (s *JWTService) GenerateRefreshToken(userID, gymID uuid.UUID, role string) (string, error) {
	return s.generate(userID, gymID, role, "refresh", s.refreshTTL)
}

func (s *JWTService) ValidateAccessToken(token string) (Claims, error) {
	return s.validate(token, "access")
}

func (s *JWTService) ValidateRefreshToken(token string) (Claims, error) {
	return s.validate(token, "refresh")
}

// HashRefreshToken returns sha256(token). We store the hash, not the token, in
// refresh_token_blacklist so logout (UC-003) can revoke without the cleartext.
func (s *JWTService) HashRefreshToken(token string) ([]byte, error) {
	h := sha256.Sum256([]byte(token))
	return h[:], nil
}

func (s *JWTService) generate(userID, gymID uuid.UUID, role, tokenType string, dur time.Duration) (string, error) {
	now := time.Now().UTC()
	c := rawClaims{
		UserID:    userID.String(),
		GymID:     gymID.String(),
		Role:      role,
		TokenType: tokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(dur)),
			Issuer:    "cuadra",
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	return t.SignedString(s.secret)
}

func (s *JWTService) validate(token, expected string) (Claims, error) {
	parsed, err := jwt.ParseWithClaims(token, &rawClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		return Claims{}, fmt.Errorf("invalid token: %w", err)
	}
	c, ok := parsed.Claims.(*rawClaims)
	if !ok || !parsed.Valid {
		return Claims{}, errors.New("invalid token claims")
	}
	if c.TokenType != expected {
		return Claims{}, fmt.Errorf("unexpected token type: got %s, want %s", c.TokenType, expected)
	}
	uid, err := uuid.Parse(c.UserID)
	if err != nil {
		return Claims{}, fmt.Errorf("invalid user_id: %w", err)
	}
	gid, err := uuid.Parse(c.GymID)
	if err != nil {
		return Claims{}, fmt.Errorf("invalid gym_id: %w", err)
	}
	out := Claims{
		UserID:    uid,
		GymID:     gid,
		Role:      c.Role,
		TokenType: c.TokenType,
	}
	if c.IssuedAt != nil {
		out.IssuedAt = c.IssuedAt.Time
	}
	if c.ExpiresAt != nil {
		out.ExpiresAt = c.ExpiresAt.Time
	}
	return out, nil
}

// HashPassword wraps bcrypt with our standard cost (12, ADR-002 §3.2).
func HashPassword(plaintext string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plaintext), 12)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func VerifyPassword(hash, plaintext string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext))
}
