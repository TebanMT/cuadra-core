// Package security implements personal sync cursor integrity. It deliberately
// has no dependency on authentication keys, providers, repositories, or SQL.
package security

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

const cursorTimestampLayout = "2006-01-02T15:04:05.000Z"

var cursorBase64 = base64.RawURLEncoding.Strict()

type CursorSigner struct {
	active string
	keys   map[string][sha256.Size]byte
}

var _ protocol.CursorSigner = (*CursorSigner)(nil)

// NewCursorSigner requires a dedicated random HMAC keyring. Keep previous key
// versions for the maximum cursor lifetime during rotation. Configuration is
// copied so subsequent caller mutations cannot change signing/verification.
func NewCursorSigner(active string, keys map[string][]byte) (*CursorSigner, error) {
	if !cursorKeyVersionValid(active) || len(keys) == 0 || len(keys) > 32 {
		return nil, protocol.ErrCursorConfiguration
	}
	s := &CursorSigner{active: active, keys: make(map[string][sha256.Size]byte, len(keys))}
	for version, key := range keys {
		if !cursorKeyVersionValid(version) || len(key) != sha256.Size {
			return nil, protocol.ErrCursorConfiguration
		}
		var copied [sha256.Size]byte
		copy(copied[:], key)
		s.keys[version] = copied
	}
	if _, ok := s.keys[active]; !ok {
		return nil, protocol.ErrCursorConfiguration
	}
	return s, nil
}

func (*CursorSigner) String() string   { return "personal cursor signer [redacted]" }
func (*CursorSigner) GoString() string { return "personal cursor signer [redacted]" }

// Private canonical wire schema. Re-encoding must be byte-identical when
// decoding: this also rejects duplicate/unknown/case-aliased fields and JSON
// spellings the server never issues. Field order is lexical at each level.
type cursorPayload struct {
	AccountID   string                `json:"account_id"`
	Epoch       string                `json:"epoch"`
	ExpiresAt   string                `json:"expires_at"`
	FiltersHash string                `json:"filters_hash"`
	IssuedAt    string                `json:"issued_at"`
	OrderHash   string                `json:"order_hash"`
	Position    cursorPositionPayload `json:"position"`
	Query       protocol.CursorQuery  `json:"query"`
}

type cursorPositionPayload struct {
	ID       string `json:"id"`
	Key      string `json:"key"`
	Sequence string `json:"sequence"`
}

func (s *CursorSigner) Issue(binding protocol.CursorBinding, position protocol.CursorPosition, now time.Time) (string, error) {
	if s == nil || !cursorClockValid(now) || binding.Validate() != nil || position.Validate(binding.Query) != nil {
		return "", protocol.ErrCursorInvalid
	}
	key, ok := s.keys[s.active]
	if !ok || !cursorKeyVersionValid(s.active) {
		return "", protocol.ErrCursorConfiguration
	}
	ttl, _ := binding.Query.TTL()
	issued := now.UTC().Truncate(time.Millisecond)
	expires := issued.Add(ttl)
	if !cursorClockValid(issued) || !cursorClockValid(expires) {
		return "", protocol.ErrCursorInvalid
	}
	filtersHash, orderHash := cursorBindingHashes(binding)
	p := cursorPayload{AccountID: binding.AccountID.String(), Epoch: binding.Epoch.String(), Query: binding.Query,
		IssuedAt: issued.Format(cursorTimestampLayout), ExpiresAt: expires.Format(cursorTimestampLayout),
		FiltersHash: filtersHash, OrderHash: orderHash,
		Position: cursorPositionPayload{Key: position.Key, Sequence: position.Sequence}}
	if position.ID != uuid.Nil {
		p.Position.ID = position.ID.String()
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", protocol.ErrCursorInvalid
	}
	unsigned := "pc1." + s.active + "." + cursorBase64.EncodeToString(raw)
	token := unsigned + "." + cursorBase64.EncodeToString(cursorMAC(key, unsigned))
	// JSON escaping can expand an otherwise valid bounded key. Never emit a
	// cursor that the closed HTTP contract cannot accept.
	if len(token) > protocol.CursorMaximumLength {
		return "", protocol.ErrCursorInvalid
	}
	return token, nil
}

func (s *CursorSigner) Verify(token string, expected protocol.CursorBinding, now time.Time) (protocol.CursorPosition, error) {
	invalid := func() (protocol.CursorPosition, error) { return protocol.CursorPosition{}, protocol.ErrCursorInvalid }
	if s == nil || !cursorClockValid(now) || expected.Validate() != nil || len(token) < 32 || len(token) > protocol.CursorMaximumLength {
		return invalid()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != "pc1" || !cursorKeyVersionValid(parts[1]) {
		return invalid()
	}
	key, ok := s.keys[parts[1]]
	if !ok {
		return invalid()
	}
	mac, err := cursorBase64.DecodeString(parts[3])
	if err != nil || len(mac) != sha256.Size || cursorBase64.EncodeToString(mac) != parts[3] {
		return invalid()
	}
	unsigned := strings.Join(parts[:3], ".")
	if !hmac.Equal(mac, cursorMAC(key, unsigned)) {
		return invalid()
	}
	raw, err := cursorBase64.DecodeString(parts[2])
	if err != nil || cursorBase64.EncodeToString(raw) != parts[2] {
		return invalid()
	}
	var p cursorPayload
	if json.Unmarshal(raw, &p) != nil {
		return invalid()
	}
	canonical, err := json.Marshal(p)
	if err != nil || !bytes.Equal(canonical, raw) {
		return invalid()
	}
	filtersHash, orderHash := cursorBindingHashes(expected)
	if p.AccountID != expected.AccountID.String() || p.Query != expected.Query || p.FiltersHash != filtersHash || p.OrderHash != orderHash {
		return invalid()
	}
	epoch, err := uuid.Parse(p.Epoch)
	if err != nil || !protocol.ValidID(epoch) || epoch.String() != p.Epoch {
		return invalid()
	}
	issued, err := time.Parse(cursorTimestampLayout, p.IssuedAt)
	if err != nil || !cursorClockValid(issued) || issued.Format(cursorTimestampLayout) != p.IssuedAt || issued.After(now.UTC()) {
		return invalid()
	}
	expires, err := time.Parse(cursorTimestampLayout, p.ExpiresAt)
	ttl, _ := expected.Query.TTL()
	if err != nil || !cursorClockValid(expires) || expires.Format(cursorTimestampLayout) != p.ExpiresAt || !expires.Equal(issued.Add(ttl)) {
		return invalid()
	}
	position := protocol.CursorPosition{Sequence: p.Position.Sequence, Key: p.Position.Key}
	if p.Position.ID != "" {
		position.ID, err = uuid.Parse(p.Position.ID)
		if err != nil || !protocol.ValidID(position.ID) || position.ID.String() != p.Position.ID {
			return invalid()
		}
	}
	if position.Validate(expected.Query) != nil {
		return invalid()
	}
	if epoch != expected.Epoch {
		return protocol.CursorPosition{}, protocol.ErrCursorEpochChanged
	}
	if !now.UTC().Before(expires) {
		return protocol.CursorPosition{}, protocol.ErrCursorExpired
	}
	return position, nil
}

func cursorBindingHashes(binding protocol.CursorBinding) (string, string) {
	filters := binding.Filters
	if filters == nil {
		filters = map[string]string{}
	}
	// encoding/json sorts string map keys. The result preserves exact UTF-8
	// values; normalization/defaults belong to the endpoint's typed input.
	raw, _ := json.Marshal(filters)
	f := sha256.Sum256(append([]byte("mancuerna.personal.cursor.filters.v1\x00"), raw...))
	o := sha256.Sum256([]byte("mancuerna.personal.cursor.order.v1\x00" + binding.Order))
	return cursorBase64.EncodeToString(f[:]), cursorBase64.EncodeToString(o[:])
}

func cursorMAC(key [sha256.Size]byte, unsigned string) []byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("mancuerna.personal.cursor.v1\x00"))
	mac.Write([]byte(unsigned))
	return mac.Sum(nil)
}

func cursorKeyVersionValid(version string) bool {
	if len(version) == 0 || len(version) > 64 {
		return false
	}
	for _, c := range version {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func cursorClockValid(now time.Time) bool {
	return !now.IsZero() && now.UTC().Year() >= 1 && now.UTC().Year() <= 9999
}
