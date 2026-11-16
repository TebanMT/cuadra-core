package security

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalSecurity "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	legacyAuth "github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	accountID   = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	sessionID   = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	deviceID    = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	proofID     = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	operationID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	testNow     = time.Date(2026, 10, 19, 0, 56, 0, 0, time.UTC)
)

func testSigningKey() ed25519.PrivateKey {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func testTokens(t *testing.T) *AccessTokens {
	t.Helper()
	key := testSigningKey()
	service, err := NewAccessTokens("personal-v1", key, map[string]ed25519.PublicKey{"personal-v1": key.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestAccessIssueVerifyAndExactTTL(t *testing.T) {
	s := testTokens(t)
	encoded, claims, err := s.Issue(accountID, sessionID, deviceID, testNow.Add(123*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := s.Verify(encoded, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if verified != claims || claims.AccountID != accountID || claims.SessionID != sessionID || claims.DeviceID != deviceID || !claims.IssuedAt.Equal(testNow) || claims.ExpiresAt.Sub(claims.IssuedAt) != personalSecurity.AccessTTL {
		t.Fatalf("unexpected personal claims: %#v", claims)
	}
	if _, err := s.Verify(encoded, testNow.Add(personalSecurity.AccessTTL+29*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(encoded, testNow.Add(personalSecurity.AccessTTL+30*time.Second)); !errors.Is(err, personalErrors.ErrInvalidAccessToken) {
		t.Fatal("expiration tolerance exceeded", err)
	}
	if _, err := s.Verify(encoded, testNow.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(encoded, testNow.Add(-31*time.Second)); !errors.Is(err, personalErrors.ErrInvalidAccessToken) {
		t.Fatal("future issuance accepted", err)
	}
}

func TestAccessRejectsMissingOrCrossScopeClaims(t *testing.T) {
	s := testTokens(t)
	base := func() jwt.MapClaims {
		return jwt.MapClaims{"iss": personalIssuer, "aud": []string{personalAudience}, "sub": accountID.String(), "sid": sessionID.String(), "did": deviceID.String(), "iat": testNow.Unix(), "exp": testNow.Add(personalSecurity.AccessTTL).Unix()}
	}
	cases := map[string]func(jwt.MapClaims){
		"wrong issuer":       func(c jwt.MapClaims) { c["iss"] = "cuadra" },
		"wrong audience":     func(c jwt.MapClaims) { c["aud"] = "dashboard" },
		"extra audience":     func(c jwt.MapClaims) { c["aud"] = []string{personalAudience, "dashboard"} },
		"duplicate audience": func(c jwt.MapClaims) { c["aud"] = []string{personalAudience, personalAudience} },
		"long lifetime":      func(c jwt.MapClaims) { c["exp"] = testNow.Add(16 * time.Minute).Unix() },
		"inverted lifetime":  func(c jwt.MapClaims) { c["exp"] = testNow.Unix() },
		"future issuance":    func(c jwt.MapClaims) { c["iat"] = testNow.Add(31 * time.Second).Unix() },
		"expired": func(c jwt.MapClaims) {
			c["iat"] = testNow.Add(-time.Hour).Unix()
			c["exp"] = testNow.Add(-31 * time.Second).Unix()
		},
		"zero account":      func(c jwt.MapClaims) { c["sub"] = uuid.Nil.String() },
		"invalid session":   func(c jwt.MapClaims) { c["sid"] = "session" },
		"wrong device type": func(c jwt.MapClaims) { c["did"] = 42 },
	}
	for _, field := range []string{"iss", "aud", "sub", "sid", "did", "iat", "exp"} {
		field := field
		cases["missing "+field] = func(c jwt.MapClaims) { delete(c, field) }
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := base()
			mutate(claims)
			token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
			token.Header["kid"] = "personal-v1"
			encoded, err := token.SignedString(testSigningKey())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Verify(encoded, testNow); !errors.Is(err, personalErrors.ErrInvalidAccessToken) {
				t.Fatalf("accepted invalid claims: %v", err)
			}
		})
	}
}

func TestAccessRejectsUntrustedAlgorithmsKeysAndAdministrativeTokens(t *testing.T) {
	s := testTokens(t)
	claims := jwt.MapClaims{"iss": personalIssuer, "aud": personalAudience, "sub": accountID.String(), "sid": sessionID.String(), "did": deviceID.String(), "iat": testNow.Unix(), "exp": testNow.Add(personalSecurity.AccessTTL).Unix()}
	for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS256, jwt.SigningMethodHS384, jwt.SigningMethodNone} {
		token := jwt.NewWithClaims(method, claims)
		token.Header["kid"] = "personal-v1"
		var key any = []byte(s.verificationKeys["personal-v1"])
		if method == jwt.SigningMethodNone {
			key = jwt.UnsafeAllowNoneSignatureType
		}
		encoded, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Verify(encoded, testNow); !errors.Is(err, personalErrors.ErrInvalidAccessToken) {
			t.Fatal("accepted algorithm", method.Alg())
		}
	}
	for _, kid := range []any{"unknown", nil, 42} {
		token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		if kid != nil {
			token.Header["kid"] = kid
		}
		encoded, err := token.SignedString(testSigningKey())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Verify(encoded, testNow); !errors.Is(err, personalErrors.ErrInvalidAccessToken) {
			t.Fatal("accepted unknown key", kid)
		}
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = "personal-v1"
	encoded, err := token.SignedString(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(encoded, testNow); !errors.Is(err, personalErrors.ErrInvalidAccessToken) {
		t.Fatal("accepted another signer")
	}
	for name, service := range map[string]*legacyAuth.JWTService{"administrative": legacyAuth.NewJWTService("synthetic-test-key"), "sidecar": legacyAuth.NewJWTServiceWithDurations("synthetic-test-key", 100*365*24*time.Hour, time.Hour)} {
		encoded, err := service.GenerateAccessToken(uuid.New(), uuid.New(), "owner")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Verify(encoded, time.Now()); !errors.Is(err, personalErrors.ErrInvalidAccessToken) {
			t.Fatal("accepted " + name)
		}
	}
}

func TestAccessRejectsDuplicateJWTFields(t *testing.T) {
	s := testTokens(t)
	for _, pair := range [][2]string{
		{`{"alg":"EdDSA","kid":"personal-v1","kid":"personal-v1"}`, `{"iss":"tinta-personal","aud":"mancuerna","sub":"` + accountID.String() + `","sid":"` + sessionID.String() + `","did":"` + deviceID.String() + `","iat":1792371360,"exp":1792372260}`},
		{`{"alg":"EdDSA","kid":"personal-v1"}`, `{"iss":"other","iss":"tinta-personal","aud":"mancuerna","sub":"` + accountID.String() + `","sid":"` + sessionID.String() + `","did":"` + deviceID.String() + `","iat":1792371360,"exp":1792372260}`},
	} {
		input := rawURL.EncodeToString([]byte(pair[0])) + "." + rawURL.EncodeToString([]byte(pair[1]))
		encoded := input + "." + rawURL.EncodeToString(ed25519.Sign(testSigningKey(), []byte(input)))
		if _, err := s.Verify(encoded, testNow); !errors.Is(err, personalErrors.ErrInvalidAccessToken) {
			t.Fatal("duplicate JWT fields accepted")
		}
	}
}

func TestAccessRotationAndKeyCopies(t *testing.T) {
	old := testTokens(t)
	token, _, err := old.Issue(accountID, sessionID, deviceID, testNow)
	if err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	public := private.Public().(ed25519.PublicKey)
	keys := map[string]ed25519.PublicKey{"personal-v1": testSigningKey().Public().(ed25519.PublicKey), "personal-v2": public}
	rotated, err := NewAccessTokens("personal-v2", private, keys)
	if err != nil {
		t.Fatal(err)
	}
	private[0] ^= 1
	public[0] ^= 1
	delete(keys, "personal-v1")
	if _, err := rotated.Verify(token, testNow); err != nil {
		t.Fatal("old token lost during rotation", err)
	}
	newToken, _, err := rotated.Issue(accountID, sessionID, deviceID, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.Verify(newToken, testNow); err != nil {
		t.Fatal("configuration aliased mutable keys", err)
	}
	parts := strings.Split(newToken, ".")
	parts[1] = parts[0]
	if _, err := rotated.Verify(strings.Join(parts, "."), testNow); !errors.Is(err, personalErrors.ErrInvalidAccessToken) {
		t.Fatal("tampered token accepted")
	}
}

func TestAccessMaximumKeyIDFitsWireContract(t *testing.T) {
	key := testSigningKey()
	kid := strings.Repeat("aZ9_-", 12) + "aZ9_" // Exactly 64 URL-safe bytes.
	s, err := NewAccessTokens(kid, key, map[string]ed25519.PublicKey{kid: key.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, claims, err := s.Issue(accountID, sessionID, deviceID, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 512 {
		t.Fatalf("access_token exceeds S00 Secret: %d bytes", len(encoded))
	}
	verified, err := s.Verify(encoded, testNow)
	if err != nil || verified != claims {
		t.Fatalf("maximum kid did not round-trip: %v", err)
	}

	// Even unexpected timestamps or future claim growth cannot return a token
	// outside the public contract; the output guard must fail without a token.
	encoded, claims, err = s.Issue(accountID, sessionID, deviceID, time.Unix(1<<60, 0))
	if !errors.Is(err, personalErrors.ErrInvalidAccessToken) || encoded != "" || claims != (personalSecurity.AccessClaims{}) {
		t.Fatal("out-of-contract token was emitted", err)
	}
}

func TestAccessRejectsUnsafeOrOversizedKeyIDs(t *testing.T) {
	key := testSigningKey()
	public := key.Public().(ed25519.PublicKey)
	for _, kid := range []string{"", strings.Repeat("k", 65), strings.Repeat("k", 128), "key with spaces", "key\n", "<key>", "ñ", "key/1", `key"1`, "key=1"} {
		if _, err := NewAccessTokens(kid, key, map[string]ed25519.PublicKey{kid: public}); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
			t.Fatalf("invalid active kid accepted: %q (%v)", kid, err)
		}
		if _, err := NewAccessTokens("valid", key, map[string]ed25519.PublicKey{"valid": public, kid: public}); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
			t.Fatalf("invalid previous kid accepted: %q (%v)", kid, err)
		}
	}
}
