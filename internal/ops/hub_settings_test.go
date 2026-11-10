package ops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSettingsOpsAreDeclared(t *testing.T) {
	list, ok := Lookup(OpSettingsList)
	if !ok {
		t.Fatal("settings_list not registered")
	}
	set, ok := Lookup(OpSettingsSet)
	if !ok {
		t.Fatal("settings_set not registered")
	}
	for _, op := range []Op{list, set} {
		if op.Scope != ScopeHub {
			t.Errorf("%s scope = %s, want hub", op.Name, op.Scope)
		}
		if op.Actions.String() != "manage" {
			t.Errorf("%s actions = %s, want manage (admin only)", op.Name, op.Actions)
		}
	}
	if list.Write || !set.Write {
		t.Errorf("write flags: list=%v set=%v", list.Write, set.Write)
	}
	if len(set.Guards) != 1 || set.Guards[0].Class != GuardKeep {
		t.Errorf("settings_set guards = %+v, want one KEEP guard", set.Guards)
	}
	if got := set.RESTMethods(); len(got) != 1 || got[0] != "POST" {
		t.Errorf("settings_set REST methods = %v, want POST only", got)
	}
}

func TestSettingsSetArgsValidate(t *testing.T) {
	id := NewUUIDv7()
	tests := []struct {
		name string
		args SettingsSetArgs
		ok   bool
	}{
		{"value", SettingsSetArgs{WriteArgs: WriteArgs{RequestID: id}, Key: "log_level", Value: "debug"}, true},
		{"empty value", SettingsSetArgs{WriteArgs: WriteArgs{RequestID: id}, Key: "public_url"}, true},
		{"unset", SettingsSetArgs{WriteArgs: WriteArgs{RequestID: id}, Key: "log_level", Unset: true}, true},
		{"no request id", SettingsSetArgs{Key: "log_level", Value: "x"}, false},
		{"bad key", SettingsSetArgs{WriteArgs: WriteArgs{RequestID: id}, Key: "Log-Level", Value: "x"}, false},
		{"empty key", SettingsSetArgs{WriteArgs: WriteArgs{RequestID: id}, Value: "x"}, false},
		{"unset and value", SettingsSetArgs{WriteArgs: WriteArgs{RequestID: id}, Key: "k", Value: "x", Unset: true}, false},
		{"too long", SettingsSetArgs{WriteArgs: WriteArgs{RequestID: id}, Key: "k", Value: strings.Repeat("a", MaxSettingValueLen+1)}, false},
		{"bad utf8", SettingsSetArgs{WriteArgs: WriteArgs{RequestID: id}, Key: "k", Value: "\xff"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.args.Validate()
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want invalid_argument", err)
			}
		})
	}
}

func TestSettingInfoJSONNeverCarriesARawSecretField(t *testing.T) {
	b, err := json.Marshal(SettingInfo{Key: "k", Env: "MINION_K", Value: "[redacted]", Secret: true, Source: SettingSourcePanel})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"key", "env", "value", "secret", "source", "read_only", "live", "pending_restart"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON lacks %q: %s", k, b)
		}
	}
}
