package security

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

var cursorTestNow = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

func cursorFixture(t *testing.T) (*CursorSigner, protocol.CursorBinding, protocol.CursorPosition) {
	t.Helper()
	s, err := NewCursorSigner("cursor_1", map[string][]byte{"cursor_1": bytes.Repeat([]byte{0x42}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return s, protocol.CursorBinding{AccountID: uuid.MustParse("12345678-1234-4234-8234-123456789abc"), Epoch: uuid.MustParse("87654321-4321-4321-8321-123456789abc"), Query: protocol.CursorSyncPull, Order: "sequence.asc.v1"}, protocol.CursorPosition{Sequence: "9223372036854775807"}
}

func requireInvalidCursor(t *testing.T, s *CursorSigner, token string, binding protocol.CursorBinding, now time.Time, want error) {
	t.Helper()
	p, err := s.Verify(token, binding, now)
	if !errors.Is(err, want) || p != (protocol.CursorPosition{}) {
		t.Fatalf("cursor error = %v; want %v and no position", err, want)
	}
}

func TestCursorIndependentVectorAndLosslessSequence(t *testing.T) {
	s, binding, position := cursorFixture(t)
	token, err := s.Issue(binding, position, cursorTestNow)
	if err != nil {
		t.Fatal(err)
	}
	// Generated independently with Python stdlib json/base64/hmac/hashlib.
	const expectedSHA256 = "b39e547de9b388bb63ddf46fd5d3186db8e07e8d50566072b9975dafea77f420"
	digest := sha256.Sum256([]byte(token))
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		t.Fatalf("cursor vector changed: %x", digest)
	}
	got, err := s.Verify(token, binding, cursorTestNow.Add(time.Hour))
	if err != nil || got != position {
		t.Fatal("BIGINT position lost precision")
	}
	if len(token) > protocol.CursorMaximumLength || len(token) < 32 {
		t.Fatal("issued cursor violates contract length")
	}
}

func TestCursorRejectsCrossOwnerQueryFiltersOrderEpochAndTampering(t *testing.T) {
	s, binding, position := cursorFixture(t)
	binding.Query, binding.Order, binding.Filters = protocol.CursorRoutines, "name.asc-id.asc.v1", map[string]string{"query": "sentadilla", "status": "active"}
	position = protocol.CursorPosition{Key: "Sentadilla", ID: uuid.New()}
	token, err := s.Issue(binding, position, cursorTestNow)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*protocol.CursorBinding){
		"owner":   func(b *protocol.CursorBinding) { b.AccountID = uuid.New() },
		"query":   func(b *protocol.CursorBinding) { b.Query = protocol.CursorExercises },
		"filter":  func(b *protocol.CursorBinding) { b.Filters = map[string]string{"query": "press", "status": "active"} },
		"missing": func(b *protocol.CursorBinding) { b.Filters = map[string]string{"query": "sentadilla"} },
		"order":   func(b *protocol.CursorBinding) { b.Order = "name.desc-id.desc.v1" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := binding
			change(&changed)
			requireInvalidCursor(t, s, token, changed, cursorTestNow, protocol.ErrCursorInvalid)
		})
	}
	epoch := binding
	epoch.Epoch = uuid.New()
	requireInvalidCursor(t, s, token, epoch, cursorTestNow, protocol.ErrCursorEpochChanged)
	// A different account cannot learn whether the epoch changed or expired.
	epoch.AccountID = uuid.New()
	requireInvalidCursor(t, s, token, epoch, cursorTestNow.Add(31*24*time.Hour), protocol.ErrCursorInvalid)
	parts := strings.Split(token, ".")
	for _, changed := range []string{
		"", "a.b.c", "pc2." + strings.Join(parts[1:], "."), "pc1.unknown." + strings.Join(parts[2:], "."),
		strings.Join(parts[:3], ".") + "." + strings.Repeat("A", 43),
		strings.Replace(token, parts[2], "A"+parts[2][1:], 1), token + "=", token + ".extra",
		strings.Repeat("a", 2049), token + "\n", "eyJhbGciOiJIUzI1NiJ9.e30.signature",
	} {
		requireInvalidCursor(t, s, changed, binding, cursorTestNow, protocol.ErrCursorInvalid)
	}
}

func TestCursorTTLStrictClockAndUTCNormalization(t *testing.T) {
	s, binding, position := cursorFixture(t)
	for _, query := range []protocol.CursorQuery{protocol.CursorSyncPull, protocol.CursorDevices} {
		binding.Query = query
		position = protocol.CursorPosition{Sequence: "0"}
		if query != protocol.CursorSyncPull {
			position = protocol.CursorPosition{ID: binding.AccountID, Key: binding.AccountID.String()}
		}
		ttl, _ := query.TTL()
		token, err := s.Issue(binding, position, cursorTestNow.Add(123456*time.Nanosecond).In(time.FixedZone("local", -6*3600)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Verify(token, binding, cursorTestNow.Add(ttl-time.Nanosecond)); err != nil {
			t.Fatal("cursor expired before exact deadline")
		}
		requireInvalidCursor(t, s, token, binding, cursorTestNow.Add(ttl), protocol.ErrCursorExpired)
		requireInvalidCursor(t, s, token, binding, cursorTestNow.Add(-time.Nanosecond), protocol.ErrCursorInvalid)
		for _, badNow := range []time.Time{{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)} {
			requireInvalidCursor(t, s, token, binding, badNow, protocol.ErrCursorInvalid)
			if issued, err := s.Issue(binding, position, badNow); err == nil || issued != "" {
				t.Fatal("invalid clock accepted")
			}
		}
		if token, err := s.Issue(binding, position, time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)); err == nil || token != "" {
			t.Fatal("unrepresentable expiry accepted")
		}
	}
}

func TestCursorRotationCopiesKeysAndBindsKeyVersion(t *testing.T) {
	s, binding, position := cursorFixture(t)
	old, _ := s.Issue(binding, position, cursorTestNow)
	keys := map[string][]byte{"cursor_1": bytes.Repeat([]byte{0x42}, 32), "cursor_2": bytes.Repeat([]byte{0x43}, 32)}
	rotated, err := NewCursorSigner("cursor_2", keys)
	if err != nil {
		t.Fatal(err)
	}
	keys["cursor_1"][0] = 0
	delete(keys, "cursor_2")
	if _, err = rotated.Verify(old, binding, cursorTestNow); err != nil {
		t.Fatal("rotation lost previous key or caller changed copied key")
	}
	newToken, err := rotated.Issue(binding, position, cursorTestNow)
	if err != nil || !strings.HasPrefix(newToken, "pc1.cursor_2.") {
		t.Fatal("rotation did not issue active version")
	}
	requireInvalidCursor(t, s, newToken, binding, cursorTestNow, protocol.ErrCursorInvalid)
	retired, _ := NewCursorSigner("cursor_2", map[string][]byte{"cursor_2": bytes.Repeat([]byte{0x43}, 32)})
	requireInvalidCursor(t, retired, old, binding, cursorTestNow, protocol.ErrCursorInvalid)
	sameKey, _ := NewCursorSigner("cursor_2", map[string][]byte{"cursor_1": bytes.Repeat([]byte{0x42}, 32), "cursor_2": bytes.Repeat([]byte{0x42}, 32)})
	requireInvalidCursor(t, sameKey, strings.Replace(old, ".cursor_1.", ".cursor_2.", 1), binding, cursorTestNow, protocol.ErrCursorInvalid)
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if fmt.Sprintf(format, rotated) != "personal cursor signer [redacted]" {
			t.Fatal("signing material exposed by diagnostics")
		}
	}
}

func TestCursorRejectsInvalidKeyringConfiguration(t *testing.T) {
	for _, config := range []struct {
		active string
		keys   map[string][]byte
	}{
		{"", nil}, {"v1", nil}, {"v1", map[string][]byte{"v2": make([]byte, 32)}},
		{"v1", map[string][]byte{"v1": make([]byte, 31)}}, {"v1", map[string][]byte{"v1": make([]byte, 33)}},
		{"v.1", map[string][]byte{"v.1": make([]byte, 32)}}, {strings.Repeat("a", 65), map[string][]byte{strings.Repeat("a", 65): make([]byte, 32)}},
		{"v1", map[string][]byte{"v1": make([]byte, 32), "é": make([]byte, 32)}},
	} {
		if s, err := NewCursorSigner(config.active, config.keys); !errors.Is(err, protocol.ErrCursorConfiguration) || s != nil {
			t.Fatal("invalid keyring accepted")
		}
	}
	var nilSigner *CursorSigner
	_, binding, position := cursorFixture(t)
	if token, err := nilSigner.Issue(binding, position, cursorTestNow); err == nil || token != "" {
		t.Fatal("nil signer issued token")
	}
	requireInvalidCursor(t, nilSigner, "", binding, cursorTestNow, protocol.ErrCursorInvalid)
}

func TestCursorCanonicalFiltersAndContractLengthBoundary(t *testing.T) {
	s, binding, _ := cursorFixture(t)
	binding.Query, binding.Order = protocol.CursorRoutines, "name.asc-id.asc.v1"
	position := protocol.CursorPosition{ID: uuid.New(), Key: strings.Repeat("a", 512)}
	binding.Filters = map[string]string{"status": "active", "query": "sentadilla"}
	same := binding
	same.Filters = map[string]string{"query": "sentadilla", "status": "active"}
	token, err := s.Issue(binding, position, cursorTestNow)
	if err != nil {
		t.Fatal(err)
	}
	tokenSame, err := s.Issue(same, position, cursorTestNow)
	if err != nil || token != tokenSame {
		t.Fatal("map iteration order changed cursor binding")
	}
	if _, err = s.Verify(token, same, cursorTestNow); err != nil {
		t.Fatal(err)
	}
	for _, escaped := range []string{strings.Repeat("\x00", 512), strings.Repeat("<", 512)} {
		position.Key = escaped
		if oversized, err := s.Issue(binding, position, cursorTestNow); !errors.Is(err, protocol.ErrCursorInvalid) || oversized != "" {
			t.Fatal("emitted cursor exceeds closed contract after escaping")
		}
	}
	position.Key = "x"
	binding.Filters = nil
	a, _ := s.Issue(binding, position, cursorTestNow)
	binding.Filters = map[string]string{}
	b, _ := s.Issue(binding, position, cursorTestNow)
	if a != b {
		t.Fatal("absent and empty semantic filters differ")
	}
	// Largest key-version and ordinary bounded key must remain usable.
	kid := strings.Repeat("a", 64)
	max, _ := NewCursorSigner(kid, map[string][]byte{kid: bytes.Repeat([]byte{0x42}, 32)})
	position.Key = strings.Repeat("z", 512)
	token, err = max.Issue(binding, position, cursorTestNow)
	if err != nil || len(token) > protocol.CursorMaximumLength {
		t.Fatal("bounded cursor cannot round-trip")
	}
	if got, err := max.Verify(token, binding, cursorTestNow); err != nil || got != position {
		t.Fatal("maximum bounded cursor did not round-trip")
	}
}

func cursorResign(raw []byte, domainPrefix string) string {
	unsigned := "pc1.cursor_1." + base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, bytes.Repeat([]byte{0x42}, 32))
	mac.Write([]byte(domainPrefix))
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestCursorStrictWireSchemaRejectsMalformedAuthenticatedClaims(t *testing.T) {
	s, binding, position := cursorFixture(t)
	token, _ := s.Issue(binding, position, cursorTestNow)
	raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[2])
	for _, bad := range [][]byte{
		append(raw, '\n'), append([]byte(" "), raw...),
		bytes.Replace(raw, []byte(`"account_id":`), []byte(`"Account_ID":`), 1),
		bytes.Replace(raw, []byte(`{"account_id":`), []byte(`{"extra":true,"account_id":`), 1),
		bytes.Replace(raw, []byte(`{"account_id":`), []byte(`{"query":"devices","account_id":`), 1),
		bytes.Replace(raw, []byte(`"id":""`), []byte(`"id":null`), 1),
		bytes.Replace(raw, []byte(`"key":""`), []byte(`"key":"\ud800"`), 1),
	} {
		requireInvalidCursor(t, s, cursorResign(bad, "mancuerna.personal.cursor.v1\x00"), binding, cursorTestNow, protocol.ErrCursorInvalid)
	}
	for name, change := range map[string]func(*cursorPayload){
		"future": func(p *cursorPayload) { p.IssuedAt = cursorTestNow.Add(time.Second).Format(cursorTimestampLayout) },
		"overlong_ttl": func(p *cursorPayload) {
			p.ExpiresAt = cursorTestNow.Add(protocol.CursorSyncTTL + time.Millisecond).Format(cursorTimestampLayout)
		},
		"seconds":      func(p *cursorPayload) { p.IssuedAt = "2026-10-19T10:00:00Z" },
		"offset":       func(p *cursorPayload) { p.IssuedAt = "2026-10-19T04:00:00.000-06:00" },
		"epoch":        func(p *cursorPayload) { p.Epoch = strings.ToUpper(p.Epoch) },
		"bad_sequence": func(p *cursorPayload) { p.Position.Sequence = "9223372036854775808" },
		"leading_zero": func(p *cursorPayload) { p.Position.Sequence = "00" },
		"mixed_key":    func(p *cursorPayload) { p.Position.Key = "a" },
		"nil_id":       func(p *cursorPayload) { p.Position.ID = uuid.Nil.String() },
		"mixed_id":     func(p *cursorPayload) { p.Position.ID = binding.AccountID.String() },
	} {
		t.Run(name, func(t *testing.T) {
			var p cursorPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			change(&p)
			bad, _ := json.Marshal(p)
			requireInvalidCursor(t, s, cursorResign(bad, "mancuerna.personal.cursor.v1\x00"), binding, cursorTestNow, protocol.ErrCursorInvalid)
		})
	}
	// Even a valid HMAC under the same key for another purpose cannot be used.
	requireInvalidCursor(t, s, cursorResign(raw, ""), binding, cursorTestNow, protocol.ErrCursorInvalid)
	requireInvalidCursor(t, s, cursorResign(raw, "mancuerna.auth.receipt.v1\x00"), binding, cursorTestNow, protocol.ErrCursorInvalid)
}
