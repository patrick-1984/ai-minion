package ops

import (
	"errors"
	"strings"
	"testing"
)

func TestOIDCOpsAreDeclared(t *testing.T) {
	for _, tc := range []struct {
		name   string
		write  bool
		guards int
	}{{OpOIDCProviderList, false, 0}, {OpOIDCProviderSet, true, 3}, {OpRoleRuleSet, true, 1}} {
		op, ok := Lookup(tc.name)
		if !ok {
			t.Fatalf("%s not registered", tc.name)
		}
		if op.Scope != ScopeHub || op.Actions.String() != "manage" || op.Write != tc.write || len(op.Guards) != tc.guards {
			t.Errorf("%s: scope %s actions %s write %v guards %d", tc.name, op.Scope, op.Actions, op.Write, len(op.Guards))
		}
	}
	set, _ := Lookup(OpOIDCProviderSet)
	for _, g := range set.Guards {
		if g.Name == "oidc_provider_not_settings" && g.Class != GuardKeep {
			t.Errorf("the settings guard must be KEEP, got %s", g.Class)
		}
	}
}

func TestOIDCProviderSetArgsValidate(t *testing.T) {
	id := NewUUIDv7()
	ok := OIDCProviderSetArgs{WriteArgs: WriteArgs{RequestID: id}, ID: "acme", Name: "Acme", Issuer: "https://idp.example/realms/x", ClientID: "c"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	mods := map[string]func(*OIDCProviderSetArgs){
		"no request id": func(a *OIDCProviderSetArgs) { a.RequestID = "" },
		"bad id":        func(a *OIDCProviderSetArgs) { a.ID = "Acme!" },
		"empty id":      func(a *OIDCProviderSetArgs) { a.ID = "" },
		"long id":       func(a *OIDCProviderSetArgs) { a.ID = strings.Repeat("a", 33) },
		"no name":       func(a *OIDCProviderSetArgs) { a.Name = " " },
		"no issuer":     func(a *OIDCProviderSetArgs) { a.Issuer = "" },
		"no client id":  func(a *OIDCProviderSetArgs) { a.ClientID = "" },
		"ftp issuer":    func(a *OIDCProviderSetArgs) { a.Issuer = "ftp://idp" },
		"credentials":   func(a *OIDCProviderSetArgs) { a.Issuer = "https://u:p@idp" },
		"fragment":      func(a *OIDCProviderSetArgs) { a.Issuer = "https://idp#x" },
		"control char":  func(a *OIDCProviderSetArgs) { a.Name = "a\nb" },
		"too long":      func(a *OIDCProviderSetArgs) { a.RoleClaim = strings.Repeat("a", MaxOIDCFieldLen+1) },
		"bad utf8":      func(a *OIDCProviderSetArgs) { a.CAFile = "\xff" },
	}
	for name, mod := range mods {
		a := ok
		mod(&a)
		err := a.Validate()
		if err == nil || !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	del := OIDCProviderSetArgs{WriteArgs: WriteArgs{RequestID: id}, ID: "acme", Delete: true}
	if err := del.Validate(); err != nil {
		t.Errorf("delete needs only an id: %v", err)
	}
}

func TestRoleRuleSetArgsValidate(t *testing.T) {
	id := NewUUIDv7()
	ok := RoleRuleSetArgs{WriteArgs: WriteArgs{RequestID: id}, ProviderID: "acme", Role: "admin", Values: []string{"g"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (RoleRuleSetArgs{WriteArgs: WriteArgs{RequestID: id}, ProviderID: "acme", Role: "user"}).Validate(); err != nil {
		t.Errorf("no values clears the rule: %v", err)
	}
	for name, mod := range map[string]func(*RoleRuleSetArgs){
		"no request id": func(a *RoleRuleSetArgs) { a.RequestID = "" },
		"bad provider":  func(a *RoleRuleSetArgs) { a.ProviderID = "A B" },
		"bad role":      func(a *RoleRuleSetArgs) { a.Role = "root" },
		"empty value":   func(a *RoleRuleSetArgs) { a.Values = []string{" "} },
		"long value":    func(a *RoleRuleSetArgs) { a.Values = []string{strings.Repeat("a", MaxRoleRuleValueLen+1)} },
		"many values":   func(a *RoleRuleSetArgs) { a.Values = make([]string, MaxRoleRuleValues+1) },
		"control":       func(a *RoleRuleSetArgs) { a.Values = []string{"a\x00"} },
	} {
		a := ok
		mod(&a)
		if err := a.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
