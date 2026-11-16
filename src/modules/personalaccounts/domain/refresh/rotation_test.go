package refresh

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	"github.com/google/uuid"
)

func refreshTestSession(t *testing.T) *session.Session {
	t.Helper()
	s, err := session.NewSession(uuid.New(), uuid.New(), uuid.New(), [32]byte{1}, uuid.New(), [32]byte{2}, time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestRotationPreservesIdentityAndEnforcesExactResponseWindow(t *testing.T) {
	s := refreshTestSession(t)
	previous := *s
	now := s.CreatedAt.Add(time.Minute)
	r, err := NewRotation(s, uuid.New(), [32]byte{3}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Rotate([32]byte{4}, [32]byte{5}, now); err != nil {
		t.Fatal(err)
	}
	if s.ID != previous.ID || s.AccountID != previous.AccountID || s.DeviceID != previous.DeviceID || s.RefreshFamilyID != previous.RefreshFamilyID || s.Generation != 2 || !s.ExpiresAt.Equal(now.Add(session.InactivityTTL)) || !s.LastUsedAt.Equal(now) || *r.OldRevocationTicketHash != previous.RevocationTicketHash || r.OldRefreshHash != previous.RefreshHash || r.Generation != 1 {
		t.Fatal("rotation identity or captured old credentials changed")
	}
	ciphertext := []byte(strings.Repeat("synthetic-ciphertext", 3))
	if err = r.SetResponse(security.Envelope{KeyVersion: "test", Ciphertext: ciphertext}); err != nil {
		t.Fatal(err)
	}
	ciphertext[0] = 'X'
	if r.Response.Ciphertext[0] == 'X' {
		t.Fatal("response aliased")
	}
	e, err := r.ReadResponse(now.Add(ResponseTTL - time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	e.Ciphertext[0] = 'Y'
	if r.Response.Ciphertext[0] == 'Y' {
		t.Fatal("returned response aliased")
	}
	for _, stamp := range []time.Time{time.Time{}, now.Add(-time.Nanosecond), now.Add(ResponseTTL), now.Add(ResponseTTL + time.Second)} {
		if _, err = r.ReadResponse(stamp); !errors.Is(err, personalErrors.ErrResultExpired) {
			t.Fatal("response outside window returned")
		}
	}
	if r.Match(r.OperationID, r.RequestHash) != nil || !errors.Is(r.Match(uuid.New(), r.RequestHash), personalErrors.ErrInvalidProof) || !errors.Is(r.Match(r.OperationID, [32]byte{9}), personalErrors.ErrInvalidProof) {
		t.Fatal("idempotency did not bind both ID and hash")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", *r, *r), "synthetic-ciphertext") {
		t.Fatal("rotation formatting leaked envelope")
	}
}
func TestRotationRejectsInvalidStateWithoutMutatingSession(t *testing.T) {
	for _, change := range []func(*session.Session){func(s *session.Session) { s.Generation = math.MaxInt64 }, func(s *session.Session) { s.ExpiresAt = s.CreatedAt.Add(time.Second) }, func(s *session.Session) { at := s.CreatedAt.Add(time.Second); s.RevokedAt = &at }} {
		s := refreshTestSession(t)
		change(s)
		copy := *s
		if err := s.Rotate([32]byte{4}, [32]byte{5}, s.CreatedAt.Add(time.Minute)); err == nil || !reflect.DeepEqual(*s, copy) {
			t.Fatal("invalid session mutated")
		}
	}
	s := refreshTestSession(t)
	copy := *s
	for _, hashes := range [][2][32]byte{{s.RefreshHash, {5}}, {{4}, s.RevocationTicketHash}, {{4}, {4}}, {{}, {5}}, {{4}, {}}} {
		if err := s.Rotate(hashes[0], hashes[1], s.CreatedAt.Add(time.Minute)); err == nil || !reflect.DeepEqual(*s, copy) {
			t.Fatal("reused or empty credential mutated session")
		}
	}
}
