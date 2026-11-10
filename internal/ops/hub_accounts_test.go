package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestAccountOpsAreRegisteredWithTheirActions(t *testing.T) {
	cases := []struct {
		name    string
		actions string
		write   bool
	}{
		{OpUsersList, "manage", false},
		{OpUserCreate, "manage", true},
		{OpUserResetPassword, "manage", true},
		{OpUserSetRole, "manage", true},
		{OpUserDeactivate, "manage", true},
		// A user changes their own password: the owner holds operate on the
		// account; manage is global and admin only.
		{OpPasswordChange, "operate", true},
	}
	for _, c := range cases {
		op, ok := Lookup(c.name)
		if !ok {
			t.Fatalf("%s is not registered", c.name)
		}
		if op.Scope != ScopeHub || op.Actions.String() != c.actions || op.Write != c.write {
			t.Errorf("%s: scope %s actions %s write %v", c.name, op.Scope, op.Actions, op.Write)
		}
		for _, g := range op.Guards {
			if g.Class != GuardKeep {
				t.Errorf("%s: guard %s is %s; account ops have nothing force can override", c.name, g.Name, g.Class)
			}
		}
	}
}

const rid = "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b"

func TestUserCreateArgsValidate(t *testing.T) {
	w := WriteArgs{RequestID: rid}
	long := strings.Repeat("a", MaxUsernameLen+1)
	bad := map[string]struct {
		a     UserCreateArgs
		field string
	}{
		"no request id": {UserCreateArgs{Username: "ann"}, "request_id"},
		"empty name":    {UserCreateArgs{WriteArgs: w}, "username"},
		"long name":     {UserCreateArgs{WriteArgs: w, Username: long}, "username"},
		"space edges":   {UserCreateArgs{WriteArgs: w, Username: " ann"}, "username"},
		"control char":  {UserCreateArgs{WriteArgs: w, Username: "a\nb"}, "username"},
		"bad utf8":      {UserCreateArgs{WriteArgs: w, Username: "a\xffb"}, "username"},
		"bad email":     {UserCreateArgs{WriteArgs: w, Username: "ann", Email: "not an address"}, "email"},
		"display name":  {UserCreateArgs{WriteArgs: w, Username: "ann", Email: "Ann <ann@example.com>"}, "email"},
		"unknown role":  {UserCreateArgs{WriteArgs: w, Username: "ann", Role: "root"}, "role"},
	}
	for name, c := range bad {
		err := c.a.Validate()
		var ae *ArgumentError
		if !errors.Is(err, ErrInvalidArgument) || !errors.As(err, &ae) || ae.Field != c.field {
			t.Errorf("%s: err = %v, want invalid_argument on %s", name, err, c.field)
		}
	}
	for _, ok := range []UserCreateArgs{
		{WriteArgs: w, Username: "ann"},
		{WriteArgs: w, Username: "Ann Lee", Email: "ann@example.com", Role: RoleAdmin},
		{WriteArgs: w, Username: "ann", Role: RoleUser},
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	if got := (UserCreateArgs{}).EffectiveRole(); got != RoleUser {
		t.Errorf("default role = %q", got)
	}
}

func TestTargetedAccountArgsNeedAUserID(t *testing.T) {
	w := WriteArgs{RequestID: rid}
	for name, v := range map[string]Validator{
		"reset":      UserResetPasswordArgs{WriteArgs: w, UserID: "42"},
		"set_role":   UserSetRoleArgs{WriteArgs: w, UserID: "", Role: RoleUser},
		"deactivate": UserDeactivateArgs{WriteArgs: w, UserID: "not-a-uuid"},
	} {
		var ae *ArgumentError
		if err := v.Validate(); !errors.As(err, &ae) || ae.Field != "user_id" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := (UserSetRoleArgs{WriteArgs: w, UserID: rid, Role: "owner"}).Validate(); err == nil {
		t.Error("an unknown role passed")
	}
	if err := (UserSetRoleArgs{WriteArgs: w, UserID: rid, Role: RoleAdmin}).Validate(); err != nil {
		t.Error(err)
	}
}

func TestPasswordChangeArgsValidate(t *testing.T) {
	w := WriteArgs{RequestID: rid}
	good := PasswordChangeArgs{WriteArgs: w, OldPassword: "old-password", NewPassword: "a-new-password"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*PasswordChangeArgs){
		"no old":        func(a *PasswordChangeArgs) { a.OldPassword = "" },
		"short new":     func(a *PasswordChangeArgs) { a.NewPassword = "short" },
		"same":          func(a *PasswordChangeArgs) { a.NewPassword = a.OldPassword },
		"too long new":  func(a *PasswordChangeArgs) { a.NewPassword = strings.Repeat("x", MaxPasswordLen+1) },
		"bad utf8 new":  func(a *PasswordChangeArgs) { a.NewPassword = "abcdefghij\xff" },
		"no request id": func(a *PasswordChangeArgs) { a.RequestID = "" },
	} {
		a := good
		mod(&a)
		if err := a.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestPasswordsStayOutOfFormattedOutputAndLogs(t *testing.T) {
	a := PasswordChangeArgs{WriteArgs: WriteArgs{RequestID: rid}, OldPassword: "OLD-secret-value", NewPassword: "NEW-secret-value"}
	for _, out := range []string{fmt.Sprintf("%v", a), fmt.Sprintf("%+v", a), fmt.Sprintf("%#v", a), a.String()} {
		if strings.Contains(out, "secret-value") {
			t.Errorf("formatted output leaks a password: %s", out)
		}
	}
	created := UserCreateResult{User: UserInfo{ID: "u1"}, TemporaryPassword: "TEMP-secret-value"}
	reset := UserResetPasswordResult{UserID: "u1", TemporaryPassword: "TEMP-secret-value"}
	for _, v := range []any{created, reset, &created, &reset} {
		if out := fmt.Sprintf("%v %+v %#v", v, v, v); strings.Contains(out, "secret-value") {
			t.Errorf("formatted output leaks a password: %s", out)
		}
	}
	var sb strings.Builder
	log := slog.New(slog.NewJSONHandler(&sb, nil))
	log.Info("x", "args", a, "created", created, "reset", reset, "created_ptr", &created, "reset_ptr", &reset)
	if strings.Contains(sb.String(), "secret-value") {
		t.Errorf("log leaks a password: %s", sb.String())
	}
}

// The create and reset results are a WriteResult with extra members, flat.
func TestSecretResultsAreFlatWriteResults(t *testing.T) {
	created := UserCreateResult{
		WriteResult:       WriteResult{Outcome: OutcomeDelivered},
		User:              UserInfo{ID: "u1", Username: "ann", Role: RoleUser},
		TemporaryPassword: "pw",
	}
	b, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"outcome", "reason", "forceable", "forced", "warnings", "pane", "user", "temporary_password"} {
		if _, ok := m[k]; !ok {
			t.Errorf("created result has no member %q: %s", k, b)
		}
	}
	if w, _ := m["warnings"].([]any); w == nil {
		t.Errorf("warnings must be an array: %s", b)
	}
	var wr WriteResult
	if err := json.Unmarshal(b, &wr); err != nil || wr.Validate() != nil {
		t.Errorf("not readable as a WriteResult: %v %v", err, wr.Validate())
	}

	b, err = json.Marshal(UserResetPasswordResult{WriteResult: WriteResult{Outcome: OutcomeDelivered}, UserID: "u1", TemporaryPassword: "pw"})
	if err != nil || !strings.Contains(string(b), `"temporary_password":"pw"`) || !strings.Contains(string(b), `"outcome":"delivered"`) {
		t.Errorf("reset result = %s (%v)", b, err)
	}
}

func TestUsersListResultNeverHasNullArrays(t *testing.T) {
	b, err := json.Marshal(UsersListResult{})
	if err != nil || string(b) != `{"users":[]}` {
		t.Errorf("empty = %s (%v)", b, err)
	}
	b, _ = json.Marshal(UsersListResult{Users: []UserInfo{{ID: "u"}}})
	if !strings.Contains(string(b), `"admin_sources":[]`) {
		t.Errorf("admin_sources must be an array: %s", b)
	}
}
