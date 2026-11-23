package agentdetect

import (
	"context"
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
	// An agent below the pane's shell is telemetry evidence only: Locate finds
	// it, but Classify (which authorizes input) keeps the pane a shell,
	// because Windows cannot say whether the agent is in the foreground.
	for name, tree := range map[string][]Proc{"claude.exe": procs[:2], "node cli.js": {procs[0], procs[2]}} {
		in := Input{Command: "pwsh", PID: 100, Screen: screen(t, "claude_idle")}
		r := classify(t, in, &fakeProcs{procs: tree})
		if r.Kind != ops.PaneKindShell {
			t.Errorf("%s: a background-capable agent made the shell kind %q", name, r.Kind)
		}
		lr, pid := New(WithProcessSource(&fakeProcs{procs: tree})).Locate(context.Background(), in)
		if lr.Kind != ops.PaneKindAgentPrefix+"claude" || pid == 0 || pid == 100 {
			t.Errorf("%s: locate %+v pid %d", name, lr, pid)
		}
	}
	// node as the pane's own command, with an unverifiable foreground, is not an input recipient.
	r := classify(t, Input{Command: "node", PID: 300, Screen: screen(t, "claude_idle")}, &fakeProcs{procs: procs[2:3]})
	if r.IsAgent() {
		t.Errorf("node pane authorized: %+v", r)
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

func TestLocateReturnsTheAgentProcessNotThePaneShell(t *testing.T) {
	tree := []Proc{
		{PID: 100, PPID: 1, PGRP: 100, TPGID: 300, TTY: "pts/1", Args: []string{"-bash"}},
		{PID: 300, PPID: 100, PGRP: 300, TPGID: 300, TTY: "pts/1", Args: []string{"claude"}},
	}
	d := New(WithProcessSource(&fakeProcs{procs: tree}))
	// tmux reports the current command "claude" with the shell's pane PID.
	if _, pid := d.Locate(context.Background(), Input{Command: "claude", PID: 100}); pid != 300 {
		t.Fatalf("pid %d, want the agent process 300", pid)
	}
	// No verified agent process: unknown, never the shell.
	if _, pid := New(WithProcessSource(&fakeProcs{procs: tree[:1]})).Locate(context.Background(), Input{Command: "claude", PID: 100}); pid != 0 {
		t.Fatalf("pid %d, want 0", pid)
	}
	if _, pid := New().Locate(context.Background(), Input{Command: "claude", PID: 0}); pid != 0 {
		t.Fatalf("pid %d, want 0", pid)
	}
}
