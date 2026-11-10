package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLinkOpsAreRegisteredWithTheirActions(t *testing.T) {
	cases := []struct {
		name  string
		write bool
	}{
		{OpIdentityList, false},
		{OpIdentityDetach, true},
		{OpUserSetPassword, true},
		{OpAdminLinkConsent, true},
		{OpOIDCLinkPolicyList, false},
		{OpOIDCLinkPolicySet, true},
	}
	for _, c := range cases {
		op, ok := Lookup(c.name)
		if !ok {
			t.Fatalf("%s is not registered", c.name)
		}
		// Every linking operation is for admins: manage is global and admin only.
		if op.Scope != ScopeHub || op.Actions.String() != "manage" || op.Write != c.write {
			t.Errorf("%s: scope %s actions %s write %v", c.name, op.Scope, op.Actions, op.Write)
		}
		for _, g := range op.Guards {
			if g.Class != GuardKeep {
				t.Errorf("%s: guard %s is %s; link ops have nothing force can override", c.name, g.Name, g.Class)
			}
		}
	}
}

func invalidOn(t *testing.T, err error, field string) {
	t.Helper()
	var ae *ArgumentError
	if !errors.Is(err, ErrInvalidArgument) || !errors.As(err, &ae) || ae.Field != field {
		t.Errorf("err = %v, want invalid_argument on %s", err, field)
	}
}

func TestIdentityArgsValidate(t *testing.T) {
	w := WriteArgs{RequestID: rid}
	uid := "0190a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2c"

	if err := (IdentityListArgs{}).Validate(); err != nil {
		t.Errorf("empty list args: %v", err)
	}
	if err := (IdentityListArgs{UserID: uid}).Validate(); err != nil {
		t.Errorf("list args: %v", err)
	}
	invalidOn(t, IdentityListArgs{UserID: "ann"}.Validate(), "user_id")

	invalidOn(t, IdentityDetachArgs{IdentityID: uid}.Validate(), "request_id")
	invalidOn(t, IdentityDetachArgs{WriteArgs: w, IdentityID: "x"}.Validate(), "identity_id")
	if err := (IdentityDetachArgs{WriteArgs: w, IdentityID: uid}).Validate(); err != nil {
		t.Errorf("detach args: %v", err)
	}

	invalidOn(t, UserSetPasswordArgs{UserID: uid}.Validate(), "request_id")
	invalidOn(t, UserSetPasswordArgs{WriteArgs: w, UserID: "x"}.Validate(), "user_id")
	invalidOn(t, UserSetPasswordArgs{WriteArgs: w, UserID: uid, Email: "not an address"}.Validate(), "email")
	for _, a := range []UserSetPasswordArgs{{WriteArgs: w, UserID: uid}, {WriteArgs: w, UserID: uid, Email: "ann@example.com"}} {
		if err := a.Validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}

	invalidOn(t, AdminLinkConsentArgs{UserID: uid}.Validate(), "request_id")
	invalidOn(t, AdminLinkConsentArgs{WriteArgs: w, UserID: "x"}.Validate(), "user_id")
	if err := (AdminLinkConsentArgs{WriteArgs: w, UserID: uid}).Validate(); err != nil {
		t.Errorf("consent args: %v", err)
	}
}

func TestOIDCLinkPolicySetArgsValidate(t *testing.T) {
	w := WriteArgs{RequestID: rid}
	ok := func(domains ...string) OIDCLinkPolicySetArgs {
		return OIDCLinkPolicySetArgs{WriteArgs: w, ProviderID: "acme", LinkingAllowed: true, Domains: domains}
	}
	for _, d := range []string{"example.com", "@Example.COM", "corp.example.co.uk", "intranet", "a-b.example", " example.com "} {
		if err := ok(d).Validate(); err != nil {
			t.Errorf("domain %q: %v", d, err)
		}
	}
	for _, d := range []string{"", "@", "*.example.com", "user@example.com", "-a.example", "a-.example", "a..example", "exa mple.com",
		"example.com/", "ex\u00e4mple.com", strings.Repeat("a", 64) + ".example", strings.Repeat("a.", 130) + "com"} {
		invalidOn(t, ok(d).Validate(), "domains")
	}
	tooMany := make([]string, MaxLinkDomains+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("d%d.example", i)
	}
	invalidOn(t, ok(tooMany...).Validate(), "domains")
	invalidOn(t, OIDCLinkPolicySetArgs{ProviderID: "acme"}.Validate(), "request_id")
	invalidOn(t, OIDCLinkPolicySetArgs{WriteArgs: w, ProviderID: "Bad Id"}.Validate(), "provider_id")
	if err := ok().Validate(); err != nil {
		t.Errorf("empty allow-list: %v", err)
	}
	if got := NormalizeLinkDomain(" @Example.COM "); got != "example.com" {
		t.Errorf("NormalizeLinkDomain = %q", got)
	}
}

func TestLinkResultsMarshalArraysNotNull(t *testing.T) {
	b, err := json.Marshal(IdentityListResult{})
	if err != nil || string(b) != `{"identities":[]}` {
		t.Errorf("identity list = %s, %v", b, err)
	}
	b, err = json.Marshal(OIDCLinkPolicyListResult{Policies: []OIDCLinkPolicyInfo{{ProviderID: "acme"}}})
	if err != nil || !strings.Contains(string(b), `"domains":[]`) || strings.Contains(string(b), "null") {
		t.Errorf("policy list = %s, %v", b, err)
	}
	ts := Timestamp(time.UnixMilli(1700000000123))
	b, err = json.Marshal(IdentityListResult{Identities: []IdentityInfo{{ID: "i", CreatedAt: ts, LastLoginAt: ts}}})
	if err != nil || !strings.Contains(string(b), `"created_at":"2023-11-14T22:13:20.123Z"`) {
		t.Errorf("identity = %s, %v", b, err)
	}
}

func TestUserSetPasswordResultNeverLeaksThePassword(t *testing.T) {
	r := UserSetPasswordResult{WriteResult: WriteResult{Outcome: OutcomeDelivered}, UserID: "u1", TemporaryPassword: "s3cret-temp"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["temporary_password"] != "s3cret-temp" || m["user_id"] != "u1" || m["outcome"] == nil || m["warnings"] == nil {
		t.Errorf("json = %s (%v)", b, err)
	}
	for name, s := range map[string]string{
		"String": r.String(), "GoString": r.GoString(), "%v": fmt.Sprintf("%v", r), "%+v": fmt.Sprintf("%+v", r), "%#v": fmt.Sprintf("%#v", r),
		"slog": slog.GroupValue(slog.Any("r", r)).String(),
	} {
		if strings.Contains(s, "s3cret-temp") {
			t.Errorf("%s leaks the password: %s", name, s)
		}
	}
	var sb strings.Builder
	slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "r", r)
	if strings.Contains(sb.String(), "s3cret-temp") {
		t.Errorf("slog leaks the password: %s", sb.String())
	}
}
