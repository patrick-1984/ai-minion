package ops

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGrantHistoryCatalogAndArgs(t *testing.T) {
	op, ok := Lookup(OpGrantHistory)
	if !ok || op.Write || op.Mutates || op.SessionOnly || !op.Actions.Permits(ActionOperate) {
		t.Fatalf("grant_history = %+v, %v", op, ok)
	}
	id := NewUUIDv7()
	for name, a := range map[string]GrantHistoryArgs{
		"no id": {}, "bad id": {ID: "x"}, "limit": {ID: id, Limit: MaxAuditListLimit + 1},
		"negative": {ID: id, Limit: -1}, "cursor": {ID: id, Cursor: "a b"}, "long cursor": {ID: id, Cursor: strings.Repeat("a", MaxAuditCursorLen+1)},
	} {
		if a.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (GrantHistoryArgs{ID: id, Cursor: "abc", Limit: MaxAuditListLimit}).Validate(); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(GrantHistoryResult{}); string(b) != `{"events":[]}` {
		t.Fatalf("empty result = %s", b)
	}
	// audit_list takes the grant filter and refuses whitespace in it.
	if err := (AuditListArgs{GrantID: id}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (AuditListArgs{GrantID: "a b"}).Validate() == nil || (AuditListArgs{GrantID: strings.Repeat("a", MaxAuditFieldLen+1)}).Validate() == nil {
		t.Fatal("a bad grant_id filter was accepted")
	}
}
