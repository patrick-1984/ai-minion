package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestHubIdentityOpsAreAdminOnlySessionOnlyHubOps(t *testing.T) {
	for _, name := range []string{OpHubIdentitySetPassphrase, OpHubIdentityReissue, OpHubIdentityRotate, OpHubIdentityChangePassphrase} {
		op, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if op.Scope != ScopeHub || !op.Mutates || !op.SessionOnly || op.Write || op.Stream {
			t.Errorf("%s = %+v", name, op)
		}
		if op.Actions.String() != "manage" {
			t.Errorf("%s accepts %s, want manage only", name, op.Actions)
		}
		if got := op.RESTMethods(); len(got) != 1 || got[0] != "POST" {
			t.Errorf("%s methods = %v, want POST only", name, got)
		}
		if op.RESTPath() != "/api/v1/ops/"+name {
			t.Errorf("%s path = %s", name, op.RESTPath())
		}
	}
	st, ok := Lookup(OpHubIdentityStatus)
	if !ok || st.Mutates || st.Write || st.SessionOnly || st.Scope != ScopeHub || st.Actions.String() != "manage" {
		t.Errorf("status op = %+v (%v)", st, ok)
	}
}

func TestHubIdentityArgsValidate(t *testing.T) {
	long := strings.Repeat("a", 1025)
	cases := []struct {
		name string
		err  error
	}{
		{"set ok", HubIdentitySetPassphraseArgs{Passphrase: "x", Confirm: "x"}.Validate()},
		{"reissue ok", HubIdentityReissueArgs{Passphrase: "x", IssueMembers: IssueMembers{ValidityDays: 400}}.Validate()},
		{"rotate ok", HubIdentityRotateArgs{Passphrase: "a", NewPassphrase: "b", Confirm: "b", Word: "ROTATE"}.Validate()},
		{"change ok", HubIdentityChangePassphraseArgs{Passphrase: "a", NewPassphrase: "b", Confirm: "b"}.Validate()},
	}
	for _, c := range cases {
		if c.err != nil {
			t.Errorf("%s: %v", c.name, c.err)
		}
	}
	bad := map[string]error{
		"set: empty":         HubIdentitySetPassphraseArgs{}.Validate(),
		"set: long":          HubIdentitySetPassphraseArgs{Passphrase: long, Confirm: long}.Validate(),
		"reissue: empty":     HubIdentityReissueArgs{}.Validate(),
		"reissue: days":      HubIdentityReissueArgs{Passphrase: "x", IssueMembers: IssueMembers{ValidityDays: 401}}.Validate(),
		"reissue: negative":  HubIdentityReissueArgs{Passphrase: "x", IssueMembers: IssueMembers{SerialFloor: -1}}.Validate(),
		"rotate: no word":    HubIdentityRotateArgs{Passphrase: "a", NewPassphrase: "b", Confirm: "b"}.Validate(),
		"rotate: lower word": HubIdentityRotateArgs{Passphrase: "a", NewPassphrase: "b", Confirm: "b", Word: "rotate"}.Validate(),
		"change: empty new":  HubIdentityChangePassphraseArgs{Passphrase: "a"}.Validate(),
		"set: bad utf-8":     HubIdentitySetPassphraseArgs{Passphrase: "\xff", Confirm: "\xff"}.Validate(),
	}
	for name, err := range bad {
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want invalid_argument", name, err)
		}
	}
}

// A passphrase must not appear in a validation message, whatever the field.
func TestHubIdentityValidationMessagesNeverContainThePassphrase(t *testing.T) {
	canary := "canary-passphrase-0123456789"
	errs := []error{
		HubIdentityRotateArgs{Passphrase: canary, NewPassphrase: canary, Confirm: canary, Word: canary}.Validate(),
		HubIdentitySetPassphraseArgs{Passphrase: canary + strings.Repeat("x", 1100), Confirm: canary}.Validate(),
	}
	for _, err := range errs {
		if err == nil || strings.Contains(err.Error(), canary) {
			t.Errorf("err = %v", err)
		}
	}
}

func TestHubIdentityArgsAreRedactedWhenPrinted(t *testing.T) {
	canary := "canary-passphrase-0123456789"
	vals := []any{
		HubIdentitySetPassphraseArgs{Passphrase: canary, Confirm: canary},
		HubIdentityReissueArgs{Passphrase: canary},
		HubIdentityRotateArgs{Passphrase: canary, NewPassphrase: canary, Confirm: canary, Word: "ROTATE"},
		HubIdentityChangePassphraseArgs{Passphrase: canary, NewPassphrase: canary, Confirm: canary},
	}
	for _, v := range vals {
		for _, s := range []string{fmt.Sprint(v), fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v), fmt.Sprintf("%s", v)} { //nolint:gosec,staticcheck // the point is the verbs
			if strings.Contains(s, canary) {
				t.Errorf("%T printed the passphrase: %s", v, s)
			}
		}
	}
}

func TestHubIdentityArgsDecodeFromJSON(t *testing.T) {
	var a HubIdentityReissueArgs
	if err := json.Unmarshal([]byte(`{"passphrase":"p","validity_days":30,"serial_floor":5,"allow_far_floor":true}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.Passphrase != "p" || a.ValidityDays != 30 || a.SerialFloor != 5 || !a.AllowFarFloor {
		t.Errorf("args = %#v", a.IssueMembers)
	}
}

func TestSessionOnlyNeedsAHubMutation(t *testing.T) {
	r := NewRegistry()
	mustPanic(t, "SessionOnly", func() {
		op := goodRead("things_list")
		op.SessionOnly = true
		r.Register(op)
	})
}
