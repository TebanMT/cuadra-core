package user_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
)

func TestValidateEmail(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"esteban@gym.com", true},
		{"a@b.co", true},
		{"no-at-sign.com", false},
		{"missing@domain", false},
		{"", false},
		{strings.Repeat("a", 256) + "@b.co", false},
	}
	for _, tc := range cases {
		if got := userDomain.ValidateEmail(tc.in); got != tc.want {
			t.Errorf("ValidateEmail(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := userDomain.ValidatePassword("12345678"); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
	if err := userDomain.ValidatePassword("short"); err == nil {
		t.Errorf("expected error for short password")
	}
}

func TestValidateFullName(t *testing.T) {
	if err := userDomain.ValidateFullName("Esteban Mares"); err != nil {
		t.Errorf("expected ok, got %v", err)
	}
	if err := userDomain.ValidateFullName("ab"); err == nil {
		t.Errorf("expected error for too-short name")
	}
	if err := userDomain.ValidateFullName("foo@bar.com"); err != userErrors.ErrNameLooksLikeEmail {
		t.Errorf("email-shaped name: got %v, want ErrNameLooksLikeEmail", err)
	}
}

func TestSetActive(t *testing.T) {
	now := time.Now().UTC()
	owner := userDomain.NewUser(uuid.New(), uuid.New(), "owner@gym.com", "h", "Owner", userDomain.RoleOwner, false, nil, now)
	op := userDomain.NewUser(uuid.New(), owner.GymID, "op@gym.com", "h", "Op", userDomain.RoleOperator, false, nil, now)

	// owner deactivating themselves
	if err := owner.SetActive(false, owner.ID, now); err != userErrors.ErrSelfDeactivate {
		t.Errorf("self-deactivate: got %v, want ErrSelfDeactivate", err)
	}
	// caller deactivating an owner
	if err := owner.SetActive(false, op.ID, now); err != userErrors.ErrCannotDeactivateOwner {
		t.Errorf("deactivate-owner: got %v, want ErrCannotDeactivateOwner", err)
	}
	// owner deactivating an operator (caller != target, target not owner)
	if err := op.SetActive(false, owner.ID, now); err != nil {
		t.Errorf("operator deactivation: %v", err)
	}
	if op.Active {
		t.Errorf("operator should be inactive")
	}
}

func TestNewOperator(t *testing.T) {
	now := time.Now().UTC()
	gymID := uuid.New()
	ownerID := uuid.New()

	// happy path: phone required, email opcional (nil)
	op, err := userDomain.NewOperator(uuid.New(), gymID, "Carla Recepción", "+52 442 123 4567", nil, "", ownerID, now)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if !op.IsOperator() {
		t.Errorf("expected operator role")
	}
	if op.Phone == nil || *op.Phone != "+524421234567" {
		t.Errorf("phone not normalized; got %v", op.Phone)
	}
	if op.Email != "" {
		t.Errorf("email should be empty when nil; got %q", op.Email)
	}
	if op.PasswordHash != "" {
		t.Errorf("password should default empty for PIN-first operators")
	}
	if op.CreatedBy == nil || *op.CreatedBy != ownerID {
		t.Errorf("created_by should reference owner")
	}

	// email present + válido
	email := "Carla@Gym.com"
	op2, err := userDomain.NewOperator(uuid.New(), gymID, "Carla Recepción", "(442) 555-1234", &email, "", ownerID, now)
	if err != nil {
		t.Fatalf("with email: %v", err)
	}
	if op2.Email != "carla@gym.com" {
		t.Errorf("email should be lowercased; got %q", op2.Email)
	}

	// phone vacío -> error
	if _, err := userDomain.NewOperator(uuid.New(), gymID, "Carla", "", nil, "", ownerID, now); err != userErrors.ErrOperatorPhoneRequired {
		t.Errorf("empty phone: got %v, want ErrOperatorPhoneRequired", err)
	}

	// email malformado -> error
	bad := "no-arroba"
	if _, err := userDomain.NewOperator(uuid.New(), gymID, "Carla", "4421234567", &bad, "", ownerID, now); err != userErrors.ErrInvalidEmail {
		t.Errorf("bad email: got %v, want ErrInvalidEmail", err)
	}
}

func TestNewOwner(t *testing.T) {
	now := time.Now().UTC()
	owner, err := userDomain.NewOwner(uuid.New(), uuid.New(), "owner@gym.com", "hash", "Don Esteban", false, now)
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if !owner.IsOwner() {
		t.Errorf("expected owner role")
	}
	if owner.RequiresPhone() {
		t.Errorf("owner should not require phone")
	}
	if _, err := userDomain.NewOwner(uuid.New(), uuid.New(), "owner@gym.com", "", "Owner", false, now); err != userErrors.ErrPasswordTooShort {
		t.Errorf("missing password hash should reject")
	}
	if _, err := userDomain.NewOwner(uuid.New(), uuid.New(), "not-email", "h", "Owner", false, now); err != userErrors.ErrInvalidEmail {
		t.Errorf("bad email should reject")
	}
}

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Canónico (src/shared/phone): quita separadores y antepone +52 a los
		// nacionales de 10 dígitos para que Twilio no los lea con país errado.
		{"+52 442 123 4567", "+524421234567"},
		{"(442) 555-1234", "+524425551234"},
		{"442.555.1234", "+524425551234"},
		{"  4425551234  ", "+524425551234"},
		{"+", ""}, // sólo '+' sin dígitos → vacío
		{"", ""},
		{"abc-def-ghij", ""},
	}
	for _, tc := range cases {
		if got := userDomain.NormalizePhone(tc.in); got != tc.want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUpdateProfilePhoneRequiredForOperator(t *testing.T) {
	now := time.Now().UTC()
	op, _ := userDomain.NewOperator(uuid.New(), uuid.New(), "Recepción", "4421234567", nil, "", uuid.New(), now)
	empty := ""
	if err := op.UpdateProfile(nil, nil, &empty, now); err != userErrors.ErrOperatorPhoneRequired {
		t.Errorf("operator clearing phone: got %v, want ErrOperatorPhoneRequired", err)
	}
	// owner puede borrar phone
	owner, _ := userDomain.NewOwner(uuid.New(), uuid.New(), "o@g.com", "h", "Don", false, now)
	if err := owner.UpdateProfile(nil, nil, &empty, now); err != nil {
		t.Errorf("owner clearing phone: %v", err)
	}
	// owner NO puede borrar email
	if err := owner.UpdateProfile(nil, &empty, nil, now); err != userErrors.ErrInvalidEmail {
		t.Errorf("owner clearing email: got %v, want ErrInvalidEmail", err)
	}
	// operator SÍ puede borrar email
	if err := op.UpdateProfile(nil, &empty, nil, now); err != nil {
		t.Errorf("operator clearing email: %v", err)
	}
}

func TestPromoteAndDemote(t *testing.T) {
	now := time.Now().UTC()
	op := userDomain.NewUser(uuid.New(), uuid.New(), "op@gym.com", "h", "Op", userDomain.RoleOperator, false, nil, now)
	op.PromoteToOwner(now)
	if !op.IsOwner() {
		t.Errorf("expected owner")
	}
	op.DemoteToOperator(now)
	if !op.IsOperator() {
		t.Errorf("expected operator")
	}
}
