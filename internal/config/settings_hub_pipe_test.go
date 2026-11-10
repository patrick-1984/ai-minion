package config

import (
	"strings"
	"testing"
)

func TestPipeBudgetPerDayIsDeclaredForTheHubWithTheRecordedDefault(t *testing.T) {
	s, ok := Default.Lookup(KeyPipeBudgetPerDay)
	if !ok || s.Scope != ScopeHub || s.Default != "200" || s.Help == "" {
		t.Fatalf("setting = %+v, %v", s, ok)
	}
	if s.EnvName() != "MINION_PIPE_BUDGET_PER_DAY" {
		t.Errorf("env = %s", s.EnvName())
	}
}

func TestParsePipeBudgetPerDay(t *testing.T) {
	for in, want := range map[string]int{"1": 1, "200": 200, "100000": 100000} {
		if got, err := ParsePipeBudgetPerDay(in); err != nil || got != want {
			t.Errorf("%q = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "-3", "100001", "ten", "1.5"} {
		if _, err := ParsePipeBudgetPerDay(in); err == nil || !strings.Contains(err.Error(), KeyPipeBudgetPerDay) {
			t.Errorf("%q: err = %v", in, err)
		}
	}
}
