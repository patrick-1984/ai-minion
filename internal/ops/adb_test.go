package ops

import (
	"strings"
	"testing"
)

// The device control ops are declared, Unreleased (no MCP tool, hidden CLI
// command) until the device gate passes, and marked so in the OpenAPI.
func TestAdbOpsDeclaredUnreleased(t *testing.T) {
	for _, name := range AdbOpNames {
		op, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is not in the catalog", name)
		}
		if !op.Unreleased || op.Scope != ScopeWorker {
			t.Errorf("%s: unreleased %v scope %s", name, op.Unreleased, op.Scope)
		}
		if name != OpAdbStatus && !op.ChangesState() {
			t.Errorf("%s does not change state", name)
		}
	}
	if s, _ := Lookup(OpAdbStatus); s.ChangesState() || !s.Actions.Permits(ActionRead) {
		t.Error("adb_status must be a read")
	}
	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(doc), `"x-minion-unreleased": true`); n < len(AdbOpNames) {
		t.Errorf("only %d unreleased marks", n)
	}
}

func TestAdbArgs(t *testing.T) {
	if (AdbShellArgs{}).Validate() == nil {
		t.Error("empty shell accepted")
	}
	if (AdbShellArgs{Argv: []string{"id"}, TimeoutS: 3601}).Validate() == nil {
		t.Error("timeout over the cap accepted")
	}
	if (AdbShellArgs{Argv: []string{strings.Repeat("x", AdbMaxCommand)}}).Validate() == nil {
		t.Error("long command accepted")
	}
	if (AdbShellArgs{Argv: []string{"id"}, WorkerID: "nope"}).Validate() == nil {
		t.Error("bad worker id accepted")
	}
	if err := (AdbShellArgs{Argv: []string{"id"}, TimeoutS: 5}).Validate(); err != nil {
		t.Error(err)
	}
	if (AdbInputArgs{Kind: "reboot", Args: []string{"x"}}).Validate() == nil {
		t.Error("unknown input kind accepted")
	}
	if err := (AdbInputArgs{Kind: "keyevent", Args: []string{"KEYCODE_HOME"}}).Validate(); err != nil {
		t.Error(err)
	}
}
