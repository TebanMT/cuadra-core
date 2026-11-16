package app

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

func pullGroup(sequence string, size int) protocol.ChangeGroup {
	raw, _ := json.Marshal(map[string]string{"name": strings.Repeat("a", size)})
	return protocol.ChangeGroup{Sequence: sequence, OperationID: uuid.NewString(), Changes: []protocol.Change{{EntityType: "profile", EntityID: uuid.NewString(), Revision: "1", Data: raw}}}
}
func TestPullPreservesWholeGroupsAndBigIntSequences(t *testing.T) {
	const base int64 = 9007199254740992
	groups := []protocol.ChangeGroup{pullGroup(strconv.FormatInt(base+1, 10), 650000), pullGroup(strconv.FormatInt(base+2, 10), 650000)}
	selected, last, err := selectGroups(groups, base, base+2, 100, crypto.JSONCanonicalizer{})
	if err != nil || len(selected) != 1 || last != base+1 || len(selected[0].Changes) != 1 {
		t.Fatal("page split a group or rounded sequence", err)
	}
	selected, last, err = selectGroups(groups[1:], base+1, base+2, 100, crypto.JSONCanonicalizer{})
	if err != nil || len(selected) != 1 || last != base+2 {
		t.Fatal("next whole group lost", err)
	}
}
func TestPullRejectsJournalGapsOversizeAndUnknownEntities(t *testing.T) {
	unknown := pullGroup("1", 1)
	unknown.Changes[0].EntityType = "unknown"
	cases := [][]protocol.ChangeGroup{nil, {pullGroup("2", 1)}, {pullGroup("1", protocol.PullPageBytes)}, {unknown}, {pullGroup("1", 1), pullGroup("3", 1)}}
	for i, groups := range cases {
		if _, _, err := selectGroups(groups, 0, 3, 100, crypto.JSONCanonicalizer{}); !errors.Is(err, protocol.ErrCommandPersistence) {
			t.Fatalf("case %d accepted: %v", i, err)
		}
	}
	selected, last, err := selectGroups(nil, 9223372036854775807, 9223372036854775807, 100, crypto.JSONCanonicalizer{})
	if err != nil || selected == nil || len(selected) != 0 || last != 9223372036854775807 {
		t.Fatal("empty final page invalid", err)
	}
}
