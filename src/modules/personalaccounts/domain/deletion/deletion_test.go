package deletion_test

import (
	"encoding/base64"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deletion"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

func TestDeletionRequiresExplicitPolicyAndPreservesFailedState(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 123456789, time.UTC)
	base := deletion.State{AccountID: uuid.New(), Status: "active", SecurityVersion: 7}
	for _, p := range []*deletion.Policy{nil, {}, {Version: "test"}, {Version: "test", PurgeDelay: -time.Hour}, {Version: " test", PurgeDelay: time.Hour}, {Version: "test", PurgeDelay: time.Nanosecond}} {
		copy := base
		if err := copy.Request(p, now); !errors.Is(err, personalErrors.ErrDeletionPolicyNotConfigured) || !reflect.DeepEqual(copy, base) {
			t.Fatal("missing policy changed state")
		}
	}
	p := &deletion.Policy{Version: "synthetic-test-only", PurgeDelay: 2 * time.Hour}
	for _, bad := range []deletion.State{{AccountID: base.AccountID, Status: "deletion_pending", SecurityVersion: 7}, {AccountID: base.AccountID, Status: "active", SecurityVersion: math.MaxInt64}, {AccountID: base.AccountID, Status: "active", SecurityVersion: 7, DeletedAt: &now}} {
		before := bad
		if err := bad.Request(p, now); err == nil || !reflect.DeepEqual(before, bad) {
			t.Fatal("invalid transition changed state")
		}
	}
	if err := base.Request(p, now); err != nil {
		t.Fatal(err)
	}
	if base.Status != "deletion_pending" || base.SecurityVersion != 8 || base.PolicyVersion != p.Version || !base.RequestedAt.Equal(now.Truncate(time.Millisecond)) || !base.PurgeAfter.Equal(base.RequestedAt.Add(p.PurgeDelay)) || base.DeletedAt != nil {
		t.Fatal("acceptance state wrong")
	}
	before := base
	if err := base.Request(p, now.Add(time.Minute)); !errors.Is(err, personalErrors.ErrDeletionState) || !reflect.DeepEqual(base, before) {
		t.Fatal("duplicate extended retention")
	}
}
func TestDeletionResultHashBindsOperationAndCanonicalSecret(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	a, b := uuid.New(), uuid.New()
	x, err := deletion.ResultHash(a, secret)
	if err != nil {
		t.Fatal(err)
	}
	y, err := deletion.ResultHash(b, secret)
	if err != nil || x == y {
		t.Fatal("reused secret crossed operation")
	}
	z, err := deletion.ResultHash(a, secret)
	if err != nil || x != z {
		t.Fatal("operation hash unstable")
	}
	for _, bad := range []string{"", secret + "=", secret[:42], secret[:42] + "B"} {
		if _, err := deletion.ResultHash(a, bad); err == nil {
			t.Fatal("noncanonical secret accepted")
		}
	}
	if _, err := deletion.ResultHash(uuid.Nil, secret); err == nil {
		t.Fatal("missing operation accepted")
	}
}
