package ops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInvitationOpsAreRegistered(t *testing.T) {
	for _, c := range []struct {
		name    string
		actions string
		write   bool
	}{
		{OpInvitationCreate, "operate", false},
		{OpInvitationList, "read", false},
		{OpInvitationRevoke, "operate", true},
	} {
		op, ok := Lookup(c.name)
		if !ok {
			t.Fatalf("%s is not registered", c.name)
		}
		if op.Scope != ScopeHub || op.Actions.String() != c.actions || op.Write != c.write {
			t.Errorf("%s: scope %s actions %s write %v", c.name, op.Scope, op.Actions, op.Write)
		}
		// Invitations are a user feature, not the global admin action.
		if op.Actions.Permits(ActionManage) {
			t.Errorf("%s accepts manage", c.name)
		}
	}
	op, _ := Lookup(OpInvitationRevoke)
	if len(op.Guards) != 1 || op.Guards[0].Class != GuardKeep {
		t.Errorf("invitation_revoke guards = %+v, want one KEEP guard", op.Guards)
	}
}

func TestInvitationTTLTable(t *testing.T) {
	want := map[string]time.Duration{
		"1h": time.Hour, "4h": 4 * time.Hour, "8h": 8 * time.Hour, "24h": 24 * time.Hour,
		"1w": 7 * 24 * time.Hour, "1m": 30 * 24 * time.Hour, "unlimited": 0,
	}
	if got := strings.Join(InvitationTTLs(), ","); got != "1h,4h,8h,24h,1w,1m,unlimited" {
		t.Errorf("InvitationTTLs = %s", got)
	}
	for ttl, d := range want {
		got, ok := InvitationTTLDuration(ttl)
		if !ok || got != d {
			t.Errorf("%s = %v, %v; want %v", ttl, got, ok, d)
		}
	}
	for _, bad := range []string{"", "2h", "1d", "1H", "forever", "0", "1y"} {
		if _, ok := InvitationTTLDuration(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestInvitationCreateValidate(t *testing.T) {
	ok := InvitationCreateArgs{Mode: InvitationOneTime, TTL: TTL1h}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.EffectiveKind() != KindPrivate {
		t.Errorf("default kind = %q", ok.EffectiveKind())
	}
	bad := map[string]InvitationCreateArgs{
		"mode":        {Mode: "once", TTL: TTL1h},
		"empty mode":  {TTL: TTL1h},
		"ttl":         {Mode: InvitationReusable, TTL: "30m"},
		"kind":        {Mode: InvitationReusable, TTL: TTL1h, Kind: "public"},
		"control":     {Mode: InvitationReusable, TTL: TTL1h, Name: "a\nb"},
		"long":        {Mode: InvitationReusable, TTL: TTL1h, Name: strings.Repeat("\u00e9", MaxInvitationNameLen+1)},
		"invalid utf": {Mode: InvitationReusable, TTL: TTL1h, Name: "\xff"},
	}
	for name, a := range bad {
		err := a.Validate()
		var ae *ArgumentError
		if !errors.Is(err, ErrInvalidArgument) || !errors.As(err, &ae) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := (InvitationCreateArgs{Mode: InvitationReusable, TTL: TTLUnlimited, Kind: KindInfrastructure, Name: strings.Repeat("\u00e9", MaxInvitationNameLen)}).Validate(); err != nil {
		t.Errorf("a valid infrastructure key: %v", err)
	}
}

func TestInvitationRevokeValidate(t *testing.T) {
	good := InvitationRevokeArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, ID: NewUUIDv7()}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]InvitationRevokeArgs{
		"no request id": {ID: NewUUIDv7()},
		"no id":         {WriteArgs: WriteArgs{RequestID: NewUUIDv7()}},
		"bad id":        {WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, ID: "abc"},
	} {
		if err := a.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestInvitationResultsEncodeStably(t *testing.T) {
	// An empty list is [] and absent times are null, never missing members.
	raw, err := json.Marshal(InvitationListResult{})
	if err != nil || string(raw) != `{"invitations":[]}` {
		t.Errorf("empty list = %s, %v", raw, err)
	}
	raw, _ = json.Marshal(InvitationInfo{ID: "i", Status: InvitationActive})
	for _, m := range []string{`"expires_at":null`, `"max_uses":null`, `"revoked_at":null`, `"uses":0`, `"status":"active"`} {
		if !strings.Contains(string(raw), m) {
			t.Errorf("%s lacks %s", raw, m)
		}
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "hash") {
		t.Errorf("InvitationInfo has a secret member: %s", raw)
	}
}

func TestInvitationCreateChangesStateSoItIsPOSTOnly(t *testing.T) {
	create, _ := Lookup(OpInvitationCreate)
	if !create.Mutates || create.Write {
		t.Errorf("invitation_create: Mutates=%v Write=%v, want Mutates only", create.Mutates, create.Write)
	}
	if got := create.RESTMethods(); len(got) != 1 || got[0] != "POST" {
		t.Errorf("invitation_create methods = %v, want POST only", got)
	}
	list, _ := Lookup(OpInvitationList)
	if got := list.RESTMethods(); len(got) != 2 {
		t.Errorf("invitation_list methods = %v, want POST and GET", got)
	}
}
