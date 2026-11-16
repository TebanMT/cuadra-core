//go:build server

package repositories

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestIdentityAccessPhoneLockKeyIsStableCopiedAndRedacted(t *testing.T) {
	if _, err := NewIdentityAccessRepository(nil); err == nil {
		t.Fatal("missing stable phone key accepted")
	}
	key := bytes.Repeat([]byte{0x46}, 32)
	a, err := NewIdentityAccessRepository(key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewIdentityAccessRepository(key)
	phone := "+525500000099"
	first := a.phoneLockKey(phone)
	if first != b.phoneLockKey(phone) || first == a.phoneLockKey("+525500000098") {
		t.Fatal("phone lock identity is not stable and specific")
	}
	key[0] = 0
	if a.phoneLockKey(phone) != first {
		t.Fatal("key aliases configuration memory")
	}
	for _, text := range []string{fmt.Sprintf("%+v", a), fmt.Sprintf("%#v", a)} {
		if strings.Contains(text, "FFFF") || strings.Contains(text, phone) {
			t.Fatal("repository exposes secret configuration")
		}
	}
}
