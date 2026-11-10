package ops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAuditListIsAHubReadOpForAdminsOnly(t *testing.T) {
	op, ok := Lookup(OpAuditList)
	if !ok {
		t.Fatal("audit_list is not registered")
	}
	if op.Scope != ScopeHub || op.Write || op.Stream {
		t.Errorf("op = %+v", op)
	}
	if op.Actions.String() != "manage" {
		t.Errorf("accepted actions = %s, want manage only", op.Actions)
	}
	for _, a := range []Action{ActionRead, ActionOperate, ActionMessage} {
		if op.Actions.Permits(a) {
			t.Errorf("audit_list must not accept %s", a)
		}
	}
	if op.RESTPath() != "/api/v1/ops/audit_list" {
		t.Errorf("path = %s", op.RESTPath())
	}
}

func TestAuditListArgsValidate(t *testing.T) {
	from := Timestamp(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	to := Timestamp(time.Date(2026, 11, 9, 13, 0, 0, 0, time.UTC))
	tests := []struct {
		name    string
		a       AuditListArgs
		wantErr bool
	}{
		{"empty", AuditListArgs{}, false},
		{"full", AuditListArgs{Actor: "a", Action: "b", From: &from, To: &to, Cursor: "abc", Limit: MaxAuditListLimit}, false},
		{"from after to", AuditListArgs{From: &to, To: &from}, true},
		{"from equals to", AuditListArgs{From: &from, To: &from}, true},
		{"negative limit", AuditListArgs{Limit: -1}, true},
		{"limit too large", AuditListArgs{Limit: MaxAuditListLimit + 1}, true},
		{"oversize actor", AuditListArgs{Actor: strings.Repeat("x", MaxAuditFieldLen+1)}, true},
		{"oversize action", AuditListArgs{Action: strings.Repeat("x", MaxAuditFieldLen+1)}, true},
		{"oversize cursor", AuditListArgs{Cursor: strings.Repeat("x", MaxAuditCursorLen+1)}, true},
		{"cursor with space", AuditListArgs{Cursor: "a b"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.a.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("err %v does not match ErrInvalidArgument", err)
			}
		})
	}
	if (AuditListArgs{}).EffectiveLimit() != DefaultAuditListLimit || (AuditListArgs{Limit: 7}).EffectiveLimit() != 7 {
		t.Error("EffectiveLimit")
	}
}

func TestAuditListArgsDecodeRFC3339Milliseconds(t *testing.T) {
	var a AuditListArgs
	if err := json.Unmarshal([]byte(`{"from":"2026-11-09T12:00:00.250Z","limit":5}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.From == nil || a.From.Time().UnixMilli() != time.Date(2026, 11, 9, 12, 0, 0, 250e6, time.UTC).UnixMilli() || a.To != nil {
		t.Errorf("args = %+v", a)
	}
	if err := json.Unmarshal([]byte(`{"from":"2026-11-09T12:00:00Z"}`), &AuditListArgs{}); err == nil {
		t.Error("a time without milliseconds was accepted")
	}
}
