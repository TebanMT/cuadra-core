package domain

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/google/uuid"
)

func TestPersonalScopeCannotSwitchOrMixIdentities(t *testing.T) {
	a, b, attempt := uuid.New(), uuid.New(), uuid.New()
	hash := sha256.Sum256([]byte("synthetic continuation"))
	ctx := context.Background()
	account := WithPersonalAccount(ctx, a)
	for name, candidate := range map[string]context.Context{
		"missing":                       ctx,
		"nil account":                   WithPersonalAccount(ctx, uuid.Nil),
		"changed account":               WithPersonalAccount(account, b),
		"changed then restored account": WithPersonalAccount(WithPersonalAccount(account, b), a),
		"gym first":                     WithPersonalAccount(WithTenant(ctx, b), a),
		"gym later":                     WithTenant(account, b),
		"invalid gym":                   WithPersonalAccount(WithTenant(ctx, uuid.Nil), a),
		"attempt in data":               WithPersonalAttempt(account, attempt, hash),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePersonalScope(candidate, PersonalDataRole); err != ErrPersonalScope {
				t.Fatalf("got %v", err)
			}
		})
	}
	if PersonalAccountID(account) != a {
		t.Fatal("account identity lost")
	}
	if err := validatePersonalScope(WithPersonalAccount(account, a), PersonalDataRole); err != nil {
		t.Fatal(err)
	}
	if err := validatePersonalScope(account, PersonalRole("unknown")); err == nil {
		t.Fatal("unknown pool accepted")
	}
}

func TestPersonalAttemptRequiresStableIDAndHash(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	hash := sha256.Sum256([]byte("synthetic continuation"))
	otherHash := sha256.Sum256([]byte("another continuation"))
	attempt := WithPersonalAttempt(ctx, id, hash)
	for _, valid := range []context.Context{attempt, WithPersonalAttempt(attempt, id, hash), WithPersonalAccount(attempt, uuid.New())} {
		if err := validatePersonalScope(valid, PersonalAuthRole); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []context.Context{
		ctx, WithPersonalAttempt(ctx, uuid.Nil, hash), WithPersonalAttempt(ctx, id, [32]byte{}),
		WithPersonalAttempt(attempt, id, otherHash), WithPersonalAttempt(attempt, uuid.New(), hash),
		WithPersonalAttempt(WithTenant(ctx, uuid.New()), id, hash), WithTenant(attempt, uuid.New()),
	} {
		if err := validatePersonalScope(invalid, PersonalAuthRole); err != ErrPersonalScope {
			t.Fatalf("got %v", err)
		}
	}
}

func TestTenantScopeRejectsPersonalIdentity(t *testing.T) {
	ctx := context.Background()
	a, gym := uuid.New(), uuid.New()
	hash := sha256.Sum256([]byte("synthetic continuation"))
	for _, scope := range []context.Context{
		WithPersonalAccount(ctx, a),
		WithPersonalAccount(ctx, uuid.Nil),
		WithPersonalAttempt(ctx, uuid.New(), hash),
		WithPersonalAttempt(ctx, uuid.Nil, hash),
		WithTenant(WithPersonalAccount(ctx, a), gym),
		WithPersonalAccount(WithTenant(ctx, gym), a),
	} {
		if !tenantInvalid(scope) {
			t.Fatal("personal identity accepted in tenant scope")
		}
	}
	if tenantInvalid(WithTenant(ctx, gym)) {
		t.Fatal("plain gym scope rejected")
	}
}

func TestPersonalSessionAndAnonymousEntryRejectAmbientCapabilities(t *testing.T) {
	accountID, other := uuid.New(), uuid.New()
	clean := context.Background()
	for _, ctx := range []context.Context{nil, WithPersonalAttempt(clean, uuid.New(), [32]byte{1}), WithTenant(clean, uuid.New()), WithPersonalAccount(clean, other)} {
		if _, err := WithAuthenticatedPersonalAccount(ctx, accountID); err == nil {
			t.Fatal("session entry accepted incompatible authority")
		}
	}
	valid, err := WithAuthenticatedPersonalAccount(clean, accountID)
	if err != nil || PersonalAccountID(valid) != accountID {
		t.Fatal("verified actor was not scoped")
	}
	if _, err = WithAuthenticatedPersonalAccount(valid, accountID); err != nil {
		t.Fatal("same verified actor cannot rebind")
	}
	if err = ValidatePersonalAnonymousScope(clean); err != nil {
		t.Fatal("clean prelogin context rejected")
	}
	for _, ctx := range []context.Context{nil, valid, WithPersonalAccount(clean, uuid.Nil), WithPersonalAttempt(clean, uuid.New(), [32]byte{1}), WithTenant(clean, uuid.New())} {
		if err = ValidatePersonalAnonymousScope(ctx); err == nil {
			t.Fatal("credential resolver accepted ambient authority")
		}
	}
}
