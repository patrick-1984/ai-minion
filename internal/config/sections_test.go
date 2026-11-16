package config

import (
	"slices"
	"testing"
)

// Every Hub setting has a section of its own: "Other" is only a safety net.
func TestEveryHubSettingHasASection(t *testing.T) {
	for _, s := range Default.ForScope(ScopeHub) {
		sec, _ := SectionOf(s.Key)
		if sec == SectionOther {
			t.Errorf("Hub setting %q has no section: add it to sectionOf in sections.go", s.Key)
		}
		if !slices.Contains(SectionOrder, sec) {
			t.Errorf("Hub setting %q is in section %q, which SectionOrder does not list", s.Key, sec)
		}
	}
}

func TestSectionOfUnknownKeyIsOther(t *testing.T) {
	if sec, _ := SectionOf("no_such_setting"); sec != SectionOther {
		t.Fatalf("unknown key in %q", sec)
	}
}
