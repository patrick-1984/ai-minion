package ops

import "testing"

func TestStrictArgs(t *testing.T) {
	for _, name := range []string{OpGrantList, OpGrantSet, OpGrantRevoke, OpGrantApprove, OpAccessMatrix, OpAccessExplain,
		"grant_future", "access_future"} {
		if !StrictArgs(name) {
			t.Errorf("StrictArgs(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "grant", "access", "trust_set", "workers_list", "send_text", "session_control_set",
		"regrant_set", "control_set_accept"} {
		if StrictArgs(name) {
			t.Errorf("StrictArgs(%q) = true, want false", name)
		}
	}
}

// Every operation of the catalog that starts with a strict prefix is strict,
// and no operation outside the families is: the set changes only on purpose.
func TestStrictArgsCoversTheCatalogFamilies(t *testing.T) {
	strict := map[string]bool{}
	for _, op := range Default.All() {
		if StrictArgs(op.Name) {
			strict[op.Name] = true
		}
	}
	for name := range strictOps {
		if _, ok := Lookup(name); !ok {
			t.Errorf("strict operation %s is not in the catalog", name)
		}
		delete(strict, name)
	}
	for name := range strict {
		t.Errorf("operation %s is strict by prefix but not listed in strictOps", name)
	}
}
