package ops

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestGrantOpsDeclaration(t *testing.T) {
	tests := []struct {
		name        string
		actions     string
		write       bool
		mutates     bool
		sessionOnly bool
	}{
		{OpGrantList, "operate", false, false, false},
		{OpGrantSet, "operate", false, true, true},
		{OpGrantRevoke, "operate", true, false, false},
		{OpGrantApprove, "manage", false, true, true},
	}
	for _, tc := range tests {
		op, ok := Lookup(tc.name)
		if !ok {
			t.Fatalf("%s is not in the catalog", tc.name)
		}
		if op.Scope != ScopeHub || op.Actions.String() != tc.actions || op.Write != tc.write ||
			op.Mutates != tc.mutates || op.SessionOnly != tc.sessionOnly {
			t.Errorf("%s = scope %s actions %s write %v mutates %v sessionOnly %v",
				tc.name, op.Scope, op.Actions, op.Write, op.Mutates, op.SessionOnly)
		}
		if op.ChangesState() != (tc.name != OpGrantList) {
			t.Errorf("%s ChangesState = %v", tc.name, op.ChangesState())
		}
	}
	if op, _ := Lookup(OpGrantRevoke); op.Result != reflect.TypeFor[WriteResult]() {
		t.Errorf("grant_revoke result = %s, want WriteResult", op.Result)
	}
}

func TestGrantLevelMapping(t *testing.T) {
	for rank, level := range map[int]string{1: GrantLevelView, 2: GrantLevelMessage, 3: GrantLevelControl} {
		if got, ok := GrantLevelRank(level); !ok || got != rank {
			t.Errorf("GrantLevelRank(%q) = %d, %v, want %d", level, got, ok, rank)
		}
		if got, ok := GrantLevelName(rank); !ok || got != level {
			t.Errorf("GrantLevelName(%d) = %q, %v, want %q", rank, got, ok, level)
		}
	}
	for _, level := range []string{"", "View", "manage", "operate", "none"} {
		if _, ok := GrantLevelRank(level); ok {
			t.Errorf("GrantLevelRank(%q) is ok", level)
		}
	}
	for _, rank := range []int{-1, 0, 4} {
		if _, ok := GrantLevelName(rank); ok {
			t.Errorf("GrantLevelName(%d) is ok", rank)
		}
	}
}

func TestGrantSetArgsValidate(t *testing.T) {
	src, dst, id := NewUUIDv7(), NewUUIDv7(), NewUUIDv7()
	valid := func() GrantSetArgs {
		return GrantSetArgs{
			WriteArgs:      WriteArgs{RequestID: NewUUIDv7()},
			SourceWorkerID: src, SourceSession: "boss",
			TargetWorkerID: dst, TargetSession: "build",
			Level: GrantLevelMessage,
		}
	}
	tests := []struct {
		name  string
		edit  func(*GrantSetArgs)
		field string // "" = valid
	}{
		{"create", func(*GrantSetArgs) {}, ""},
		{"all shared sessions", func(a *GrantSetArgs) { a.TargetSession = "" }, ""},
		{"view", func(a *GrantSetArgs) { a.Level = GrantLevelView }, ""},
		{"control", func(a *GrantSetArgs) { a.Level = GrantLevelControl }, ""},
		{"change", func(a *GrantSetArgs) { a.ID, a.IfRevision = id, 3 }, ""},
		{"expiry", func(a *GrantSetArgs) { a.ExpiresAt = "2027-01-02T03:04:05.000Z" }, ""},
		{"longest note", func(a *GrantSetArgs) { a.Note = strings.Repeat("\u00e4", MaxGrantNote) }, ""},
		{"no request id", func(a *GrantSetArgs) { a.RequestID = "" }, "request_id"},
		{"force", func(a *GrantSetArgs) { a.Force = true }, "force"},
		{"bad id", func(a *GrantSetArgs) { a.ID, a.IfRevision = "x", 1 }, "id"},
		{"change without revision", func(a *GrantSetArgs) { a.ID = id }, "if_revision"},
		{"create with revision", func(a *GrantSetArgs) { a.IfRevision = 1 }, "if_revision"},
		{"bad source worker", func(a *GrantSetArgs) { a.SourceWorkerID = "w1" }, "source_worker_id"},
		{"no target worker", func(a *GrantSetArgs) { a.TargetWorkerID = "" }, "target_worker_id"},
		{"same worker", func(a *GrantSetArgs) { a.TargetWorkerID = src }, "target_worker_id"},
		{"no source session", func(a *GrantSetArgs) { a.SourceSession = "" }, "source_session"},
		{"source session with colon", func(a *GrantSetArgs) { a.SourceSession = "a:b" }, "source_session"},
		{"source session wildcard space", func(a *GrantSetArgs) { a.SourceSession = " boss" }, "source_session"},
		{"target session with dot", func(a *GrantSetArgs) { a.TargetSession = "a.b" }, "target_session"},
		{"target session control char", func(a *GrantSetArgs) { a.TargetSession = "a\nb" }, "target_session"},
		{"no level", func(a *GrantSetArgs) { a.Level = "" }, "level"},
		{"unknown level", func(a *GrantSetArgs) { a.Level = "manage" }, "level"},
		{"expiry without milliseconds", func(a *GrantSetArgs) { a.ExpiresAt = "2027-01-02T03:04:05Z" }, "expires_at"},
		{"expiry not a time", func(a *GrantSetArgs) { a.ExpiresAt = "tomorrow" }, "expires_at"},
		{"note too long", func(a *GrantSetArgs) { a.Note = strings.Repeat("\u00e4", MaxGrantNote+1) }, "note"},
		{"note not utf-8", func(a *GrantSetArgs) { a.Note = "\xff" }, "note"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := valid()
			tc.edit(&a)
			err := a.Validate()
			if tc.field == "" {
				if err != nil {
					t.Errorf("err = %v, want none", err)
				}
				return
			}
			invalidOn(t, err, tc.field)
		})
	}
}

func TestGrantRevokeAndApproveArgsValidate(t *testing.T) {
	wa, id := WriteArgs{RequestID: NewUUIDv7()}, NewUUIDv7()
	revoke := []struct {
		name  string
		a     GrantRevokeArgs
		field string
	}{
		{"any revision", GrantRevokeArgs{wa, id, 0}, ""},
		{"at a revision", GrantRevokeArgs{wa, id, 2}, ""},
		{"no request id", GrantRevokeArgs{WriteArgs{}, id, 0}, "request_id"},
		{"no id", GrantRevokeArgs{wa, "", 0}, "id"},
		{"bad id", GrantRevokeArgs{wa, "x", 0}, "id"},
	}
	for _, tc := range revoke {
		if err := tc.a.Validate(); tc.field == "" {
			if err != nil {
				t.Errorf("revoke %s: err = %v, want none", tc.name, err)
			}
		} else {
			invalidOn(t, err, tc.field)
		}
	}
	approve := []struct {
		name  string
		a     GrantApproveArgs
		field string
	}{
		{"ok", GrantApproveArgs{wa, id, 1}, ""},
		{"no request id", GrantApproveArgs{WriteArgs{}, id, 1}, "request_id"},
		{"force", GrantApproveArgs{WriteArgs{RequestID: wa.RequestID, Force: true}, id, 1}, "force"},
		{"bad id", GrantApproveArgs{wa, "x", 1}, "id"},
		{"no revision", GrantApproveArgs{wa, id, 0}, "if_revision"},
	}
	for _, tc := range approve {
		if err := tc.a.Validate(); tc.field == "" {
			if err != nil {
				t.Errorf("approve %s: err = %v, want none", tc.name, err)
			}
		} else {
			invalidOn(t, err, tc.field)
		}
	}
}

func TestGrantListArgsAndResult(t *testing.T) {
	if err := (GrantListArgs{}).Validate(); err != nil {
		t.Errorf("no filter: %v", err)
	}
	if err := (GrantListArgs{WorkerID: NewUUIDv7()}).Validate(); err != nil {
		t.Errorf("filter: %v", err)
	}
	invalidOn(t, GrantListArgs{WorkerID: "x"}.Validate(), "worker_id")
	b, err := json.Marshal(GrantListResult{})
	if err != nil || string(b) != `{"grants":[]}` {
		t.Errorf("empty list = %s, %v", b, err)
	}
}
