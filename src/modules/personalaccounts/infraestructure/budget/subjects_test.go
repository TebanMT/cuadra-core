package budget

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	domain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
)

func TestBudgetSubjectHashesStableAcrossInstancesAndScoped(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, 32)
	a, err := NewSubjects(key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSubjects(key)
	if err != nil {
		t.Fatal(err)
	}
	subject := "+525500000099"
	first, err := a.Hash(domain.Destination, subject)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Hash(domain.Destination, subject)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == [32]byte{} {
		t.Fatal("restart changed stable subject hash")
	}
	origin, _ := a.Hash(domain.Origin, subject)
	if origin == first {
		t.Fatal("scope domains collide")
	}
	key[0] = 0
	again, _ := a.Hash(domain.Destination, subject)
	if again != first {
		t.Fatal("key aliased caller memory")
	}
	for _, rendered := range []string{fmt.Sprintf("%+v", a), fmt.Sprintf("%#v", a)} {
		if strings.Contains(rendered, string(key)) || strings.Contains(rendered, subject) {
			t.Fatal("hasher leaked key or subject")
		}
	}
}

func TestBudgetSubjectHasherRejectsMissingKeyOrSubject(t *testing.T) {
	if _, err := NewSubjects(nil); err == nil {
		t.Fatal("missing key accepted")
	}
	h, _ := NewSubjects(bytes.Repeat([]byte{1}, 32))
	for _, subject := range []string{"", " ", strings.Repeat("x", 4097)} {
		if _, err := h.Hash(domain.Destination, subject); err == nil {
			t.Fatal("invalid subject accepted")
		}
	}
	if _, err := h.Hash(domain.Scope("other"), "private input"); err == nil || strings.Contains(err.Error(), "private input") {
		t.Fatal("invalid scope accepted or leaked")
	}
}
