package agentdetect

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

const winTable = "100|1|\"C:\\Program Files\\PowerShell\\7\\pwsh.exe\" -NoLogo\r\n" +
	"200|100|\"C:\\Users\\me\\.local\\bin\\claude.exe\" --resume abc\r\n" +
	"300|100|\"C:\\Program Files\\nodejs\\node.exe\" \"C:\\Users\\me\\AppData\\Roaming\\npm\\node_modules\\@anthropic-ai\\claude-code\\cli.js\"\r\n" +
	"400|1|C:\\Windows\\explorer.exe\r\nbroken line\r\n"

func TestParseWinProcsAndClassifyWindowsPanes(t *testing.T) {
	procs := parseWinProcs(winTable)
	if len(procs) != 4 || procs[1].Args[0] != `C:\Users\me\.local\bin\claude.exe` || procs[1].PPID != 100 {
		t.Fatalf("parsed %+v", procs)
	}
	// A pane whose shell runs claude.exe, and one whose shell runs it through node.
	for name, tree := range map[string][]Proc{"claude.exe": procs[:2], "node cli.js": {procs[0], procs[2]}} {
		r := classify(t, Input{Command: "pwsh", PID: 100, Screen: screen(t, "claude_idle")}, &fakeProcs{procs: tree})
		if r.Kind != ops.PaneKindAgentPrefix+"claude" || r.State != ops.PaneStateStable {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// node as the pane's own command is found through its arguments.
	r := classify(t, Input{Command: "node", PID: 300, Screen: screen(t, "claude_idle")}, &fakeProcs{procs: procs[2:3]})
	if r.Kind != ops.PaneKindAgentPrefix+"claude" {
		t.Errorf("node pane: %+v", r)
	}
}

func TestSplitWinCommandLine(t *testing.T) {
	got := splitWinCommandLine(`"C:\a b\x.exe" -p  "q r" plain ""`)
	want := []string{`C:\a b\x.exe`, "-p", "q r", "plain", ""}
	if len(got) != len(want) {
		t.Fatalf("%q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%q", got)
		}
	}
}
