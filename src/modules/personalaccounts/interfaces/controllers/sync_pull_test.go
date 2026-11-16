//go:build server

package controllers

import "testing"

func TestPersonalPageQueryRejectsAmbiguousAndUnboundedPages(t *testing.T) {
	for _, raw := range []string{"cursor=", "cursor=a&cursor=b", "limit=0", "limit=101", "limit=01", "limit=+1", "limit=1&limit=2", "unknown=1", "cursor=%zz", "limit=1;other=2"} {
		if _, _, err := personalPageQuery(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if cursor, limit, err := personalPageQuery(""); err != nil || cursor != "" || limit != 50 {
		t.Fatal("default page", err)
	}
	if cursor, limit, err := personalPageQuery("cursor=pc1.test&limit=100"); err != nil || cursor != "pc1.test" || limit != 100 {
		t.Fatal("explicit page", err)
	}
}
