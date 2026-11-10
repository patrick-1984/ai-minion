package oidc

import (
	"slices"
	"testing"
)

func TestRulesEvaluate(t *testing.T) {
	rules := Rules{Claim: "groups", Admin: []string{"minion-admins", "root"}, User: []string{"minion-users"}}
	tests := []struct {
		name   string
		rules  Rules
		claims map[string]any
		want   []string
	}{
		{"admin list", rules, map[string]any{"groups": []any{"x", "minion-admins"}}, []string{"admin"}},
		{"user list", rules, map[string]any{"groups": []any{"minion-users"}}, []string{"user"}},
		{"both values give both roles", rules, map[string]any{"groups": []any{"minion-users", "root"}}, []string{"admin", "user"}},
		{"string claim", rules, map[string]any{"groups": "minion-users"}, []string{"user"}},
		{"typed string slice", rules, map[string]any{"groups": []string{"root"}}, []string{"admin"}},
		{"no matching rule", rules, map[string]any{"groups": []any{"other", "Minion-Admins"}}, nil},
		{"comparison is case sensitive", rules, map[string]any{"groups": "MINION-USERS"}, nil},
		{"claim missing", rules, map[string]any{"email": "a@b.c"}, nil},
		{"nil claims", rules, nil, nil},
		{"claim null", rules, map[string]any{"groups": nil}, nil},
		{"claim empty string", rules, map[string]any{"groups": ""}, nil},
		{"claim empty list", rules, map[string]any{"groups": []any{}}, nil},
		{"claim of the wrong type", rules, map[string]any{"groups": 42}, nil},
		{"object claim", rules, map[string]any{"groups": map[string]any{"minion-admins": true}}, nil},
		{"non-string elements ignored", rules, map[string]any{"groups": []any{1, true, nil, "minion-users"}}, []string{"user"}},
		{"only the configured claim is read", rules, map[string]any{"roles": []any{"minion-admins"}, "groups": []any{"nobody"}}, nil},
		{"another claim with admin value is ignored", rules, map[string]any{"roles": "minion-admins"}, nil},
		{"no claim configured", Rules{Admin: []string{"a"}}, map[string]any{"": "a", "groups": "a"}, nil},
		{"empty configured value matches nothing", Rules{Claim: "g", Admin: []string{""}, User: []string{""}}, map[string]any{"g": []any{"", "x"}}, nil},
		{"no rules", Rules{Claim: "groups"}, map[string]any{"groups": "minion-admins"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.rules.Evaluate(tc.claims)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Evaluate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasAdmin(t *testing.T) {
	if !HasAdmin([]string{"admin", "user"}) || HasAdmin([]string{"user"}) || HasAdmin(nil) {
		t.Fatal("HasAdmin is wrong")
	}
}
